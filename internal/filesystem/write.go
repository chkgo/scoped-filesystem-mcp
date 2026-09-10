package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

// TextEdit replaces every occurrence of OldText with NewText after verifying
// that the current content has exactly ExpectedOccurrences occurrences.
type TextEdit struct {
	OldText             string
	NewText             string
	ExpectedOccurrences int
}

// EditRequest describes a revision-protected structured edit. An empty Edits
// slice makes ProposedContent an explicit whole-file replacement.
type EditRequest struct {
	ExpectedRevision string
	Edits            []TextEdit
	ProposedContent  string
}

// WriteResult describes a successfully created or replaced text file.
type WriteResult struct {
	Root     string
	Path     string
	Revision string
	Size     int64
	// RecoveryPath is a retained, root-relative prior version created by an
	// edit. Callers may inspect it and explicitly move it to Trash when done.
	RecoveryPath  string
	RecoveryPaths []string
}

// Conflict returns the current text and the proposal that was not applied.
type Conflict struct {
	CurrentContent  string
	CurrentRevision string
	Pending         EditRequest
	RecoveryPath    string
	RecoveryPaths   []string
}

type TargetState string

const (
	TargetProposalApplied TargetState = "proposal_applied"
	TargetCurrentRestored TargetState = "current_restored"
	TargetUnknown         TargetState = "unknown"
)

// RecoveryOutcome truthfully describes a post-swap condition that cannot be
// represented as an ordinary revision conflict.
type RecoveryOutcome struct {
	Root                     string
	Path                     string
	Operation                config.Operation
	TargetState              TargetState
	RecoveryPath             string
	RecoveryContent          string
	RecoveryRevision         string
	RecoveryContentAvailable bool
	RecoveryPaths            []string
}

// RecoveryError carries a durable recovery outcome. Callers must inspect it
// before treating a write error as an ordinary failed operation.
type RecoveryError struct {
	Outcome RecoveryOutcome
	err     error
}

func (e *RecoveryError) Error() string { return "recovery_required" }
func (e *RecoveryError) Unwrap() error { return e.err }

// CreateDirectory creates path, optionally creating missing parent directories.
func (s *Service) CreateDirectory(ctx context.Context, root, path string, createParents bool) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, newError(root, path, config.OpCreate, "filesystem_unavailable", err)
	}
	p, err := s.access.Resolve(root, path, config.OpCreate, access.ParentExisting)
	if err != nil {
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	parent, name, err := s.createParent(root, path, p, createParents)
	if err != nil {
		return WriteResult{}, err
	}
	defer parent.Close()
	if name == "" {
		return WriteResult{}, newError(root, path, config.OpCreate, "destination_exists", nil)
	}
	if err := platform.MkdirAt(parent, name, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return WriteResult{}, newError(root, path, config.OpCreate, "destination_exists", err)
		}
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	return WriteResult{Root: root, Path: path}, nil
}

// CreateText creates a new UTF-8 text file without replacing an existing path.
func (s *Service) CreateText(ctx context.Context, root, path, content string, createParents bool) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, newError(root, path, config.OpCreate, "filesystem_unavailable", err)
	}
	if !utf8.ValidString(content) {
		return WriteResult{}, newError(root, path, config.OpCreate, "invalid_utf8", nil)
	}
	p, err := s.access.Resolve(root, path, config.OpCreate, access.ParentExisting)
	if err != nil {
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	parent, name, err := s.createParent(root, path, p, createParents)
	if err != nil {
		return WriteResult{}, err
	}
	defer parent.Close()
	if name == "" {
		return WriteResult{}, newError(root, path, config.OpCreate, "destination_exists", nil)
	}

	file, temporaryName, err := createTemporaryFile(parent, 0o644, ".scopedfs-tmp-")
	if err != nil {
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	created := true
	defer func() {
		if created {
			_ = file.Close()
			_ = platform.RemoveAt(parent, temporaryName, false)
		}
	}()
	if err := ctx.Err(); err != nil {
		return WriteResult{}, newError(root, path, config.OpCreate, "filesystem_unavailable", err)
	}
	if err := s.write(file, []byte(content)); err != nil {
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	if err := s.sync(file); err != nil {
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	if err := file.Close(); err != nil {
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	if s.beforeCreatePublish != nil {
		if err := s.beforeCreatePublish(); err != nil {
			return WriteResult{}, MapError(root, path, config.OpCreate, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, newError(root, path, config.OpCreate, "filesystem_unavailable", err)
	}
	if err := s.renameExclusive(parent, temporaryName, parent, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return WriteResult{}, newError(root, path, config.OpCreate, "destination_exists", err)
		}
		return WriteResult{}, MapError(root, path, config.OpCreate, err)
	}
	created = false
	return writeResult(root, path, content), nil
}

// EditText applies a structured edit only when the file still has the expected
// revision. A revision mismatch is returned as a Conflict and never writes.
func (s *Service) EditText(ctx context.Context, root, path string, request EditRequest) (*WriteResult, *Conflict, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, newError(root, path, config.OpEdit, "filesystem_unavailable", err)
	}
	if err := s.OperationError(config.OpEdit); err != nil {
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	if !utf8.ValidString(request.ProposedContent) {
		return nil, nil, newError(root, path, config.OpEdit, "invalid_edit", nil)
	}
	parent, name, canonicalRelative, err := s.editParent(root, path)
	if err != nil {
		return nil, nil, err
	}
	defer parent.Close()

	current, _, err := s.readTextAt(ctx, root, path, config.OpEdit, parent, name)
	if err != nil {
		return nil, nil, err
	}
	if Revision([]byte(current)) != request.ExpectedRevision {
		return nil, conflict(current, request), nil
	}
	proposed, err := applyEdits(current, request)
	if err != nil {
		return nil, nil, newError(root, path, config.OpEdit, "invalid_edit", err)
	}
	return s.replaceText(ctx, root, path, canonicalRelative, parent, name, request.ExpectedRevision, proposed, request)
}

// OverwriteText replaces text after a conflict was explicitly approved. It
// verifies the revision shown in the prompt again immediately before rename.
func (s *Service) OverwriteText(ctx context.Context, root, path, expectedRevision, proposedContent string) (*WriteResult, *Conflict, error) {
	request := EditRequest{ExpectedRevision: expectedRevision, ProposedContent: proposedContent}
	return s.EditText(ctx, root, path, request)
}

func (s *Service) createParent(root, path string, p access.Path, createParents bool) (*os.File, string, error) {
	parent, remaining, err := p.OpenParent()
	if err != nil {
		return nil, "", MapError(root, path, config.OpCreate, err)
	}
	if len(remaining) == 0 {
		return parent, "", nil
	}
	for _, component := range remaining[:len(remaining)-1] {
		if !createParents {
			parent.Close()
			return nil, "", newError(root, path, config.OpCreate, "path_not_found", nil)
		}
		if err := platform.MkdirAt(parent, component, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			parent.Close()
			return nil, "", MapError(root, path, config.OpCreate, err)
		}
		next, err := platform.OpenDirectoryAt(parent, component)
		if err != nil {
			parent.Close()
			return nil, "", MapError(root, path, config.OpCreate, err)
		}
		parent.Close()
		parent = next
	}
	return parent, remaining[len(remaining)-1], nil
}

func (s *Service) editParent(root, path string) (*os.File, string, string, error) {
	p, err := s.access.Resolve(root, path, config.OpEdit, access.Existing)
	if err != nil {
		return nil, "", "", MapError(root, path, config.OpEdit, err)
	}
	parent, remaining, err := p.OpenParent()
	if err != nil {
		return nil, "", "", MapError(root, path, config.OpEdit, err)
	}
	if len(remaining) != 1 {
		parent.Close()
		return nil, "", "", newError(root, path, config.OpEdit, "unsupported_file_type", nil)
	}
	return parent, remaining[0], p.CanonicalRelative(), nil
}

func (s *Service) readTextAt(ctx context.Context, root, path string, operation config.Operation, parent *os.File, name string) (string, os.FileInfo, error) {
	if s.beforeReadAt != nil {
		if err := s.beforeReadAt(name); err != nil {
			return "", nil, MapError(root, path, operation, err)
		}
	}
	file, err := platform.OpenFileAt(parent, name, os.O_RDONLY, 0)
	if err != nil {
		return "", nil, MapError(root, path, operation, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", nil, MapError(root, path, operation, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, newError(root, path, operation, "unsupported_file_type", nil)
	}
	if s.afterReadAtStat != nil {
		if err := s.afterReadAtStat(name); err != nil {
			return "", nil, MapError(root, path, operation, err)
		}
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, s.maxReadBytes+1))
	if err != nil {
		return "", nil, MapError(root, path, operation, err)
	}
	if int64(len(data)) > s.maxReadBytes {
		return "", nil, newError(root, path, operation, "response_too_large", nil)
	}
	if !utf8.Valid(data) {
		return "", nil, newError(root, path, operation, "invalid_utf8", nil)
	}
	return string(data), info, nil
}

func (s *Service) replaceText(ctx context.Context, root, path, canonicalRelative string, parent *os.File, name, expectedRevision, proposed string, request EditRequest) (*WriteResult, *Conflict, error) {
	current, info, err := s.readTextAt(ctx, root, path, config.OpEdit, parent, name)
	if err != nil {
		return nil, nil, err
	}
	if Revision([]byte(current)) != expectedRevision {
		return nil, conflict(current, request), nil
	}
	temporary, temporaryName, err := createTemporaryFile(parent, info.Mode().Perm(), ".scopedfs-recovery-")
	if err != nil {
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = temporary.Close()
			_ = platform.RemoveAt(parent, temporaryName, false)
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, newError(root, path, config.OpEdit, "filesystem_unavailable", err)
	}
	if err := s.write(temporary, []byte(proposed)); err != nil {
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	if err := s.sync(temporary); err != nil {
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	if err := temporary.Close(); err != nil {
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, newError(root, path, config.OpEdit, "filesystem_unavailable", err)
	}
	if s.beforeReplace != nil {
		if err := s.beforeReplace(); err != nil {
			return nil, nil, MapError(root, path, config.OpEdit, err)
		}
	}
	current, _, err = s.readTextAt(ctx, root, path, config.OpEdit, parent, name)
	if err != nil {
		return nil, nil, err
	}
	if Revision([]byte(current)) != expectedRevision {
		return nil, conflict(current, request), nil
	}
	if s.beforeSwap != nil {
		if err := s.beforeSwap(); err != nil {
			return nil, nil, MapError(root, path, config.OpEdit, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, newError(root, path, config.OpEdit, "filesystem_unavailable", err)
	}
	recoveryPath := temporaryRecoveryPath(canonicalRelative, temporaryName)
	if err := s.swapNames(parent, temporaryName, name); err != nil {
		if errors.Is(err, platform.ErrAtomicReplaceUnsupported) {
			return nil, nil, newError(root, path, config.OpEdit, "atomic_replace_unsupported", err)
		}
		return nil, nil, MapError(root, path, config.OpEdit, err)
	}
	removeTemporary = false

	displaced, _, err := s.readTextAt(ctx, root, path, config.OpEdit, parent, temporaryName)
	if err != nil {
		return nil, nil, recoveryErrorUnknown(root, path, TargetUnknown, recoveryPath, err)
	}
	if Revision([]byte(displaced)) == expectedRevision {
		if s.afterDisplacedRead != nil {
			if err := s.afterDisplacedRead(); err != nil {
				return nil, nil, recoveryErrorKnown(root, path, TargetProposalApplied, recoveryPath, displaced, MapError(root, path, config.OpEdit, err))
			}
		}
		result := writeResult(root, path, proposed)
		result.RecoveryPath = recoveryPath
		result.RecoveryPaths = []string{recoveryPath}
		return &result, nil, nil
	}
	if s.beforeRollback != nil {
		if err := s.beforeRollback(); err != nil {
			return nil, nil, recoveryErrorKnown(root, path, TargetProposalApplied, recoveryPath, displaced, MapError(root, path, config.OpEdit, err))
		}
	}
	if err := s.swapNames(parent, temporaryName, name); err != nil {
		return nil, nil, recoveryErrorKnown(root, path, TargetProposalApplied, recoveryPath, displaced, MapError(root, path, config.OpEdit, err))
	}

	// Verify the retained artifact before the live target. It may contain a
	// third writer's bytes displaced by rollback, so never substitute the local
	// proposal when either read fails.
	rollbackDisplaced, _, err := s.readTextAt(ctx, root, path, config.OpEdit, parent, temporaryName)
	if err != nil {
		return nil, nil, recoveryErrorUnknown(root, path, TargetCurrentRestored, recoveryPath, err)
	}
	if s.afterRollbackRead != nil {
		if err := s.afterRollbackRead(parent, temporaryName); err != nil {
			return nil, nil, recoveryErrorKnown(root, path, TargetCurrentRestored, recoveryPath, rollbackDisplaced, MapError(root, path, config.OpEdit, err))
		}
	}
	rolledBack, _, err := s.readTextAt(ctx, root, path, config.OpEdit, parent, name)
	if err != nil {
		return nil, nil, recoveryErrorKnown(root, path, TargetCurrentRestored, recoveryPath, rollbackDisplaced, err)
	}
	if Revision([]byte(rollbackDisplaced)) == Revision([]byte(proposed)) {
		return nil, recoveryConflict(rolledBack, request, recoveryPath), nil
	}
	return nil, nil, recoveryErrorKnown(root, path, TargetCurrentRestored, recoveryPath, rollbackDisplaced, nil)
}

func (s *Service) swapNames(parent *os.File, from, to string) error {
	if s.renameSwap != nil {
		return s.renameSwap(parent, from, to)
	}
	return platform.Exchange(parent, from, to)
}

func (s *Service) renameExclusive(sourceParent *os.File, sourceName string, destinationParent *os.File, destinationName string) error {
	if s.renameAt != nil {
		return s.renameAt(sourceParent, sourceName, destinationParent, destinationName)
	}
	return platform.RenameNoReplace(sourceParent, sourceName, destinationParent, destinationName)
}

func (s *Service) write(file *os.File, data []byte) error {
	if s.writeFile != nil {
		return s.writeFile(file, data)
	}
	return writeAll(file, data)
}

func (s *Service) sync(file *os.File) error {
	if s.syncFile != nil {
		return s.syncFile(file)
	}
	return file.Sync()
}

func createTemporaryFile(parent *os.File, mode os.FileMode, prefix string) (*os.File, string, error) {
	for range 32 {
		name, err := temporaryName(prefix)
		if err != nil {
			return nil, "", err
		}
		file, err := platform.OpenFileAt(parent, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", os.ErrExist
}

func temporaryName(prefix string) (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes), nil
}

func applyEdits(content string, request EditRequest) (string, error) {
	if len(request.Edits) == 0 {
		return request.ProposedContent, nil
	}
	result := content
	for _, edit := range request.Edits {
		if edit.OldText == "" || !utf8.ValidString(edit.OldText) || !utf8.ValidString(edit.NewText) || edit.ExpectedOccurrences < 0 {
			return "", errors.New("invalid text edit")
		}
		if count := strings.Count(result, edit.OldText); count != edit.ExpectedOccurrences {
			return "", errors.New("unexpected occurrence count")
		}
		result = strings.ReplaceAll(result, edit.OldText, edit.NewText)
	}
	if result != request.ProposedContent {
		return "", errors.New("proposed content does not match edits")
	}
	return result, nil
}

func conflict(current string, request EditRequest) *Conflict {
	return &Conflict{CurrentContent: current, CurrentRevision: Revision([]byte(current)), Pending: cloneEditRequest(request)}
}

func recoveryConflict(current string, request EditRequest, recoveryPath string) *Conflict {
	result := conflict(current, request)
	result.RecoveryPath = recoveryPath
	result.RecoveryPaths = []string{recoveryPath}
	return result
}

func recoveryErrorKnown(root, path string, state TargetState, recoveryPath, content string, cause error) *RecoveryError {
	outcome := RecoveryOutcome{Root: root, Path: path, Operation: config.OpEdit, TargetState: state, RecoveryPath: recoveryPath, RecoveryPaths: []string{recoveryPath}, RecoveryContent: content, RecoveryContentAvailable: true, RecoveryRevision: Revision([]byte(content))}
	return &RecoveryError{Outcome: outcome, err: cause}
}

func recoveryErrorUnknown(root, path string, state TargetState, recoveryPath string, cause error) *RecoveryError {
	return &RecoveryError{Outcome: RecoveryOutcome{Root: root, Path: path, Operation: config.OpEdit, TargetState: state, RecoveryPath: recoveryPath, RecoveryPaths: []string{recoveryPath}}, err: cause}
}

func temporaryRecoveryPath(path, temporaryName string) string {
	directory := filepath.Dir(path)
	if directory == "." {
		return temporaryName
	}
	return filepath.ToSlash(filepath.Join(directory, temporaryName))
}

func cloneEditRequest(request EditRequest) EditRequest {
	clone := request
	clone.Edits = append([]TextEdit(nil), request.Edits...)
	return clone
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func writeResult(root, path, content string) WriteResult {
	bytes := []byte(content)
	return WriteResult{Root: root, Path: path, Revision: Revision(bytes), Size: int64(len(bytes))}
}
