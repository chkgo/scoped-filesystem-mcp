package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
)

const maximumPendingEditEnvelopeBytes = 10 << 20
const maximumReservedRecoveryPathBytes = 4096

var errPendingEditEnvelopeTooLarge = errors.New("pending edit envelope exceeds 10 MiB")

func (s toolServer) registerWrites(server *mcp.Server) {
	mcp.AddTool(server, mutationTool[WriteOutput]("create_directory", "Create a directory, optionally including missing parents.", false), s.createDirectory)
	mcp.AddTool(server, mutationTool[WriteOutput]("create_file", "Create a new UTF-8 file without replacing an existing file.", false), s.createFile)
	editTool := mutationTool[WriteOutput]("edit_file", "Edit UTF-8 text using an expected revision; resolve conflicts through elicitation.", true)
	conflictSchema, err := jsonschema.For[ConflictOutput](nil)
	if err != nil {
		panic(err)
	}
	editTool.OutputSchema.(*jsonschema.Schema).OneOf = append(editTool.OutputSchema.(*jsonschema.Schema).OneOf, conflictSchema)
	mcp.AddTool(server, editTool, s.editFile)
	mcp.AddTool(server, mutationTool[MutationOutput]("move_path", "Rename a path between allowed locations without replacing a destination.", true), s.movePath)
	mcp.AddTool(server, mutationTool[MutationOutput]("trash_path", "Move a path to macOS Trash.", true), s.trashPath)
	mcp.AddTool(server, mutationTool[MutationOutput]("permanent_delete_path", "Permanently remove a path only after confirmation and target fingerprint revalidation.", true), s.permanentDeletePath)
}

func mutationTool[Out any](name, description string, destructive bool) *mcp.Tool {
	result := tool[Out](name, description+" destructiveHint is informational; configured ask rules enforce confirmation.")
	result.Annotations.ReadOnlyHint = false
	result.Annotations.DestructiveHint = &destructive
	return result
}

func (s toolServer) createDirectory(ctx context.Context, req *mcp.CallToolRequest, input CreateDirectoryInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input.PathInput, config.OpCreate, access.ParentExisting)}, false); result != nil || err != nil {
		return result, nil, err
	}
	write, err := s.filesystem.CreateDirectory(ctx, input.Root, input.Path, input.Parents)
	if err != nil {
		return failed(err)
	}
	return successResult(writeOutput(write), "created directory"), nil, nil
}

func (s toolServer) createFile(ctx context.Context, req *mcp.CallToolRequest, input CreateFileInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input.PathInput, config.OpCreate, access.ParentExisting)}, false); result != nil || err != nil {
		return result, nil, err
	}
	write, err := s.filesystem.CreateText(ctx, input.Root, input.Path, input.Content, false)
	if err != nil {
		return failed(err)
	}
	return successResult(writeOutput(write), "created text file"), nil, nil
}

func (s toolServer) editFile(ctx context.Context, req *mcp.CallToolRequest, input EditFileInput) (*mcp.CallToolResult, any, error) {
	proposalOutput := pendingEditOutput(input)
	if err := validatePendingEditEnvelope(proposalOutput); err != nil {
		if req.Params.RequestState != "" || req.Params.InputResponses != nil {
			// Oversized proposals are never stored, so this cannot be an exact
			// continuation. Preserve its cleanup ledger and leave the original
			// token available without running authorization or filesystem work.
			return confirmationErrorWithRecord("invalid_confirmation", s.pending.lookup(req.Params.RequestState)), nil, nil
		}
		return toolErrorResult(ToolError{Code: "response_too_large", Root: input.Root, Path: input.Path, Operation: string(config.OpEdit), Message: "the pending edit exceeds the 10 MiB serialized envelope limit"}), nil, nil
	}
	editTargets := []operationTarget{target(input.PathInput, config.OpRead, access.Existing), target(input.PathInput, config.OpEdit, access.Existing)}
	pending, choice, result, err := s.gate(req, input, editTargets, false)
	if result != nil || err != nil {
		return result, nil, err
	}
	var write *filesystem.WriteResult
	var conflict *filesystem.Conflict
	proposal := editRequest(input)
	priorRecoveryPaths := []string(nil)
	if pending != nil && pending.conflict != nil {
		priorRecoveryPaths = conflictRecoveryPaths(pending.conflict)
	}
	if (pending == nil || pending.kind != "conflict" || choice != "reload_and_rebase") && validateRecoveryLedgerReservation(priorRecoveryPaths) != nil {
		return toolErrorResult(ToolError{Code: "response_too_large", Message: "the recovery-path ledger has no room for another atomic edit artifact", RecoveryPaths: priorRecoveryPaths}), nil, nil
	}
	if pending != nil && pending.kind == "conflict" {
		if choice == "reload_and_rebase" {
			// A deliberately non-matching revision obtains the latest text using edit
			// authorization, without requiring an unrelated read permission or writing.
			_, current, err := s.filesystem.EditText(ctx, input.Root, input.Path, filesystem.EditRequest{ProposedContent: proposal.ProposedContent})
			if err != nil {
				return failedWithRecoveryPaths(err, priorRecoveryPaths)
			}
			current.Pending = proposal
			setConflictRecoveryPaths(current, priorRecoveryPaths)
			return checkedConflictResult("reload_and_rebase", current, target(input.PathInput, config.OpEdit, access.Existing), "current content and pending edit returned for rebasing"), nil, nil
		}
		write, conflict, err = s.filesystem.OverwriteText(ctx, input.Root, input.Path, pending.revision, input.ProposedContent)
		if conflict != nil {
			conflict.Pending = proposal
		}
	} else {
		write, conflict, err = s.filesystem.EditText(ctx, input.Root, input.Path, proposal)
	}
	// RecoveryError must win over a conflict or generic error. failed preserves
	// every RecoveryOutcome field and never starts an overwrite/rebase prompt.
	if err != nil {
		return failedWithRecoveryPaths(err, priorRecoveryPaths)
	}
	if conflict != nil {
		setConflictRecoveryPaths(conflict, append(priorRecoveryPaths, conflictRecoveryPaths(conflict)...))
		digest, err := argumentDigest(req.Params.Arguments)
		if err != nil {
			return nil, nil, err
		}
		result, err := s.prompt(req, pendingRecord{tool: req.Params.Name, arguments: digest, kind: "conflict", revision: conflict.CurrentRevision, conflict: conflict, target: target(input.PathInput, config.OpEdit, access.Existing)}, fmt.Sprintf("%s:%s changed. Current revision: %s. Reload and rebase, overwrite this revision with the pending proposal, or cancel?", input.Root, input.Path, conflict.CurrentRevision))
		return result, nil, err
	}
	setWriteRecoveryPaths(write, append(priorRecoveryPaths, writeRecoveryPaths(write)...))
	return successResult(writeOutput(*write), "edited text file"), nil, nil
}

func (s toolServer) movePath(ctx context.Context, req *mcp.CallToolRequest, input MovePathInput) (*mcp.CallToolResult, any, error) {
	targets := []operationTarget{{input.SourceRoot, input.SourcePath, config.OpMove, access.Existing}, {input.DestinationRoot, input.DestinationPath, config.OpMove, access.ParentExisting}}
	if _, _, result, err := s.gate(req, input, targets, false); result != nil || err != nil {
		return result, nil, err
	}
	mutation, err := s.filesystem.Move(ctx, input.SourceRoot, input.SourcePath, input.DestinationRoot, input.DestinationPath)
	if err != nil {
		return failed(err)
	}
	return successResult(MutationOutput{Root: mutation.Root, Path: mutation.Path}, "moved path"), nil, nil
}

func (s toolServer) trashPath(ctx context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input, config.OpTrash, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	mutation, err := s.filesystem.Trash(ctx, input.Root, input.Path)
	if err != nil {
		return failed(err)
	}
	return successResult(MutationOutput{Root: mutation.Root, Path: mutation.Path}, "moved path to Trash"), nil, nil
}

func (s toolServer) permanentDeletePath(ctx context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input, config.OpPermanentDelete, access.Existing)}, true); result != nil || err != nil {
		return result, nil, err
	}
	mutation, err := s.filesystem.PermanentDelete(ctx, input.Root, input.Path)
	if err != nil {
		return failed(err)
	}
	return successResult(MutationOutput{Root: mutation.Root, Path: mutation.Path}, "permanently deleted path"), nil, nil
}

func editRequest(input EditFileInput) filesystem.EditRequest {
	edits := make([]filesystem.TextEdit, len(input.Edits))
	for i, edit := range input.Edits {
		edits[i] = filesystem.TextEdit{OldText: edit.OldText, NewText: edit.NewText, ExpectedOccurrences: edit.ExpectedOccurrences}
	}
	return filesystem.EditRequest{ExpectedRevision: input.ExpectedRevision, ProposedContent: input.ProposedContent, Edits: edits}
}

func writeOutput(write filesystem.WriteResult) WriteOutput {
	return WriteOutput{Root: write.Root, Path: write.Path, Revision: write.Revision, Size: write.Size, RecoveryPath: write.RecoveryPath, RecoveryPaths: writeRecoveryPaths(&write)}
}

func conflictOutput(status string, conflict *filesystem.Conflict) ConflictOutput {
	edits := make([]TextEditInput, len(conflict.Pending.Edits))
	for i, edit := range conflict.Pending.Edits {
		edits[i] = TextEditInput{OldText: edit.OldText, NewText: edit.NewText, ExpectedOccurrences: edit.ExpectedOccurrences}
	}
	return ConflictOutput{Status: status, CurrentContent: conflict.CurrentContent, CurrentRevision: conflict.CurrentRevision, Pending: PendingEditOutput{ExpectedRevision: conflict.Pending.ExpectedRevision, ProposedContent: conflict.Pending.ProposedContent, Edits: edits}, RecoveryPath: conflict.RecoveryPath, RecoveryPaths: conflictRecoveryPaths(conflict)}
}

func pendingEditOutput(input EditFileInput) PendingEditOutput {
	edits := append([]TextEditInput(nil), input.Edits...)
	return PendingEditOutput{ExpectedRevision: input.ExpectedRevision, ProposedContent: input.ProposedContent, Edits: edits}
}

func pendingEditEnvelopeSize(pending PendingEditOutput) (int, error) {
	return serializedEditOutputSize(pending)
}
func validatePendingEditEnvelope(pending PendingEditOutput) error {
	size, err := pendingEditEnvelopeSize(pending)
	if err != nil {
		return err
	}
	if size > maximumPendingEditEnvelopeBytes {
		return errPendingEditEnvelopeTooLarge
	}
	return nil
}

func serializedEditOutputSize(output any) (int, error) {
	data, err := json.Marshal(output)
	return len(data), err
}

func validateEditOutputSize(output any) error {
	size, err := serializedEditOutputSize(output)
	if err != nil {
		return err
	}
	if size > maximumPendingEditEnvelopeBytes {
		return errPendingEditEnvelopeTooLarge
	}
	return nil
}

func validateRecoveryLedgerReservation(paths []string) error {
	// Control bytes exercise JSON's six-byte \u00XX encoding and therefore
	// reserve the worst serialized size of any 4,096-byte filesystem path.
	reserved := append(append([]string(nil), paths...), strings.Repeat("\x01", maximumReservedRecoveryPathBytes))
	return validateEditOutputSize(ToolError{
		Code:          "response_too_large",
		Message:       "the recovery-path ledger has no room for another atomic edit artifact",
		RecoveryPaths: reserved,
	})
}

func checkedConflictResult(status string, conflict *filesystem.Conflict, target operationTarget, message string) *mcp.CallToolResult {
	output := conflictOutput(status, conflict)
	if validateEditOutputSize(output) == nil {
		return successResult(output, message)
	}
	pending := output.Pending
	return toolErrorResult(ToolError{
		Code:          "response_too_large",
		Root:          target.root,
		Path:          target.path,
		Operation:     string(target.operation),
		Message:       "the complete conflict result exceeds the 10 MiB serialized output limit; no content was truncated",
		Pending:       &pending,
		RecoveryPaths: conflictRecoveryPaths(conflict),
	})
}

func conflictRecoveryPaths(conflict *filesystem.Conflict) []string {
	if conflict == nil {
		return nil
	}
	paths := append([]string(nil), conflict.RecoveryPaths...)
	if len(paths) == 0 && conflict.RecoveryPath != "" {
		paths = []string{conflict.RecoveryPath}
	}
	return uniquePaths(paths)
}
func writeRecoveryPaths(write *filesystem.WriteResult) []string {
	if write == nil {
		return nil
	}
	paths := append([]string(nil), write.RecoveryPaths...)
	if len(paths) == 0 && write.RecoveryPath != "" {
		paths = []string{write.RecoveryPath}
	}
	return uniquePaths(paths)
}
func setConflictRecoveryPaths(conflict *filesystem.Conflict, paths []string) {
	if conflict == nil {
		return
	}
	conflict.RecoveryPaths = uniquePaths(paths)
	if len(conflict.RecoveryPaths) > 0 {
		conflict.RecoveryPath = conflict.RecoveryPaths[len(conflict.RecoveryPaths)-1]
	}
}
func setWriteRecoveryPaths(write *filesystem.WriteResult, paths []string) {
	if write == nil {
		return
	}
	write.RecoveryPaths = uniquePaths(paths)
	if len(write.RecoveryPaths) > 0 {
		write.RecoveryPath = write.RecoveryPaths[len(write.RecoveryPaths)-1]
	}
}
func uniquePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := map[string]struct{}{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func failedWithRecoveryPaths(err error, paths []string) (*mcp.CallToolResult, any, error) {
	result, unexpected := failureResult(err)
	if unexpected != nil {
		return nil, nil, unexpected
	}
	if toolError, ok := result.StructuredContent.(ToolError); ok {
		toolError.RecoveryPaths = uniquePaths(append(paths, toolError.RecoveryPaths...))
		if toolError.Recovery != nil {
			toolError.Recovery.RecoveryPaths = append([]string(nil), toolError.RecoveryPaths...)
		}
		setToolErrorResult(result, toolError)
	}
	return result, nil, nil
}
