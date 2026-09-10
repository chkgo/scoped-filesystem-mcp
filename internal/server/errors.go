package server

import (
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
)

// ToolError is the structured domain failure returned to MCP clients.
type ToolError struct {
	Code                 string             `json:"code" jsonschema:"stable machine-readable error category"`
	Root                 string             `json:"root,omitempty" jsonschema:"configured root name when known"`
	Path                 string             `json:"path,omitempty" jsonschema:"relative path when known"`
	Operation            string             `json:"operation,omitempty" jsonschema:"filesystem operation that failed"`
	Message              string             `json:"message" jsonschema:"concise safe explanation for the caller"`
	Detail               string             `json:"detail,omitempty" jsonschema:"safe operating-system detail without an absolute path"`
	Pending              *PendingEditOutput `json:"pending,omitempty" jsonschema:"pending edit retained when conflict confirmation is unavailable"`
	RecoveryPaths        []string           `json:"recovery_paths,omitempty" jsonschema:"all retained root-relative versions that remain the caller's responsibility"`
	RecoveryPathsOmitted int                `json:"recovery_paths_omitted,omitempty" jsonschema:"number of paths explicitly omitted only when an untrusted error ledger cannot fit the output limit"`
	Recovery             *RecoveryOutput    `json:"recovery,omitempty" jsonschema:"durable recovery details when a write cannot be safely classified as ordinary failure"`
}

// RecoveryOutput preserves a RecoveryError rather than reducing it to a
// generic filesystem error when mutation tools are added.
type RecoveryOutput struct {
	TargetState      string   `json:"target_state" jsonschema:"last-known target transition state during the interrupted write, not a guarantee of current live state"`
	RecoveryPath     string   `json:"recovery_path" jsonschema:"relative recovery file path when available"`
	RecoveryContent  string   `json:"recovery_content" jsonschema:"recoverable content when available"`
	RecoveryRevision string   `json:"recovery_revision" jsonschema:"revision of recoverable content when available"`
	ContentAvailable bool     `json:"content_available" jsonschema:"whether recovery_content and recovery_revision were verified and are available"`
	RecoveryPaths    []string `json:"recovery_paths,omitempty" jsonschema:"all retained root-relative versions associated with the recovery outcome"`
}

func failureResult(err error) (*mcp.CallToolResult, error) {
	var recovery *filesystem.RecoveryError
	if errors.As(err, &recovery) {
		recoveryPaths := append([]string(nil), recovery.Outcome.RecoveryPaths...)
		if len(recoveryPaths) == 0 && recovery.Outcome.RecoveryPath != "" {
			recoveryPaths = []string{recovery.Outcome.RecoveryPath}
		}
		recoveryPaths = uniquePaths(recoveryPaths)
		return toolErrorResult(ToolError{
			Code:      "recovery_required",
			Root:      recovery.Outcome.Root,
			Path:      recovery.Outcome.Path,
			Operation: string(recovery.Outcome.Operation),
			Message:   "a write reached an uncertain state; inspect recovery details before continuing",
			Recovery: &RecoveryOutput{
				TargetState:      string(recovery.Outcome.TargetState),
				RecoveryPath:     recovery.Outcome.RecoveryPath,
				RecoveryContent:  recovery.Outcome.RecoveryContent,
				RecoveryRevision: recovery.Outcome.RecoveryRevision,
				ContentAvailable: recovery.Outcome.RecoveryContentAvailable,
				RecoveryPaths:    recoveryPaths,
			},
			RecoveryPaths: recoveryPaths,
		}), nil
	}

	var domain *filesystem.Error
	if errors.As(err, &domain) {
		return toolErrorResult(ToolError{
			Code:      domain.Code,
			Root:      domain.Root,
			Path:      domain.Path,
			Operation: string(domain.Operation),
			Message:   conciseMessage(domain.Code),
			Detail:    domain.SafeDetail(),
		}), nil
	}
	return nil, err
}

func toolErrorResult(toolError ToolError) *mcp.CallToolResult {
	result := &mcp.CallToolResult{IsError: true}
	setToolErrorResult(result, toolError)
	return result
}

func setToolErrorResult(result *mcp.CallToolResult, toolError ToolError) {
	toolError = boundedToolError(toolError)
	result.IsError = true
	result.Content = []mcp.Content{&mcp.TextContent{Text: toolError.Message}}
	result.StructuredContent = toolError
}

func boundedToolError(toolError ToolError) ToolError {
	if validateEditOutputSize(toolError) == nil {
		return toolError
	}
	paths := append([]string(nil), toolError.RecoveryPaths...)
	if toolError.Recovery != nil {
		paths = append(paths, toolError.Recovery.RecoveryPaths...)
		if toolError.Recovery.RecoveryPath != "" {
			paths = append(paths, toolError.Recovery.RecoveryPath)
		}
	}
	fallback := ToolError{
		Code:                 "response_too_large",
		Root:                 toolError.Root,
		Path:                 toolError.Path,
		Operation:            toolError.Operation,
		Message:              "the complete structured result exceeds the 10 MiB serialized output limit; no content was truncated",
		Pending:              toolError.Pending,
		RecoveryPaths:        uniquePaths(paths),
		RecoveryPathsOmitted: toolError.RecoveryPathsOmitted,
	}
	if toolError.Recovery != nil {
		recovery := *toolError.Recovery
		recovery.RecoveryContent = ""
		recovery.RecoveryRevision = ""
		recovery.ContentAvailable = false
		recovery.RecoveryPaths = append([]string(nil), fallback.RecoveryPaths...)
		fallback.Recovery = &recovery
	}
	if validateEditOutputSize(fallback) == nil {
		return fallback
	}
	// Root/path/operation are useful context but not recoverable data. Drop
	// adversarially large context before considering any proposal or path ledger.
	fallback.Root = ""
	fallback.Path = ""
	fallback.Operation = ""
	if validateEditOutputSize(fallback) == nil {
		return fallback
	}
	// Recovery metadata duplicates the authoritative top-level path ledger; its
	// content has already been made unavailable above.
	fallback.Recovery = nil
	if validateEditOutputSize(fallback) == nil {
		return fallback
	}
	// The accepted proposal is indivisible: omit it instead of returning a
	// misleading prefix. The caller still owns the exact request it supplied.
	fallback.Pending = nil
	if validateEditOutputSize(fallback) == nil {
		return fallback
	}
	// Real edit paths are reserved before mutation. This final branch handles
	// only an already-invalid/untrusted ToolError and makes any omission explicit.
	fallback.RecoveryPathsOmitted += len(fallback.RecoveryPaths)
	fallback.RecoveryPaths = nil
	fallback.Message = "the structured error and its recovery-path ledger exceed the 10 MiB limit; recovery paths were explicitly omitted"
	return fallback
}

func conciseMessage(code string) string {
	switch code {
	case "operation_not_allowed":
		return "this operation is not allowed for the configured root"
	case "path_outside_root", "symlink_escape":
		return "the requested path is outside the configured root"
	case "path_not_found":
		return "the requested path was not found"
	case "root_target_changed":
		return "the configured root changed after server startup"
	case "response_too_large":
		return "the file exceeds the configured response-size limit"
	case "invalid_utf8":
		return "the requested file is not valid UTF-8 text"
	case "unsupported_file_type":
		return "the requested path has an unsupported file type"
	case "destination_exists":
		return "the destination already exists; nothing was replaced"
	case "invalid_edit":
		return "the proposed text edit is invalid or does not match the declared replacements"
	case "atomic_replace_unsupported":
		return "this filesystem does not support the atomic exchange required for a safe edit"
	case "cross_filesystem_move_unsupported":
		return "the move crosses filesystems; copy-and-delete moves are not supported"
	case "unknown_root":
		return "the configured root name is unknown"
	case "partial_delete":
		return "permanent deletion stopped after removing part of the requested tree"
	default:
		return "the filesystem is currently unavailable"
	}
}
