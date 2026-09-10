package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
)

const pendingLifetime = 5 * time.Minute

type pendingRecord struct {
	tool          string
	arguments     [sha256.Size]byte
	kind          string
	expires       time.Time
	revision      string
	fingerprint   fileFingerprint
	conflict      *filesystem.Conflict
	recoveryPaths []string
	target        operationTarget
}

// Only this random lookup token crosses the protocol boundary. Action details
// and the approved revision stay on the server, and every retry consumes it.
type pendingStore struct {
	mu      sync.Mutex
	records map[string]pendingRecord
}

func (s *pendingStore) put(record pendingRecord) (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(bytes[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[string]pendingRecord)
	}
	record.recoveryPaths = recordRecoveryPaths(&record)
	now := time.Now()
	for key, value := range s.records {
		if !now.Before(value.expires) {
			paths := recordRecoveryPaths(&value)
			if len(paths) == 0 {
				delete(s.records, key)
				continue
			}
			// Keep only the small cleanup ledger after expiry. The complete pending
			// edit can approach the 10 MiB envelope limit and must not be retained
			// indefinitely merely so a late continuation can recover artifact paths.
			value.conflict = nil
			value.recoveryPaths = paths
			s.records[key] = value
		}
	}
	record.expires = now.Add(pendingLifetime)
	s.records[token] = record
	return token, nil
}

func (s *pendingStore) take(token, tool string, digest [sha256.Size]byte) (*pendingRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[token]
	if !ok {
		return nil, false
	}
	if record.tool != tool || record.arguments != digest {
		// Knowledge of a valid random token is sufficient to disclose its cleanup
		// ledger, but never to authorize a different operation. Keep the token so
		// the original tool and arguments can still consume it exactly once.
		return &record, false
	}
	delete(s.records, token)
	if !time.Now().Before(record.expires) {
		return &record, false
	}
	return &record, true
}

// lookup returns cleanup context without authorizing or consuming the token.
// Early request validation failures must still disclose retained versions.
func (s *pendingStore) lookup(token string) *pendingRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[token]
	if !ok {
		return nil
	}
	return &record
}

func argumentDigest(arguments json.RawMessage) ([sha256.Size]byte, error) {
	// Preserve all supplied fields, while ignoring JSON object key order and
	// whitespace. UseNumber avoids losing precision when canonicalizing numbers.
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var input any
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return [sha256.Size]byte{}, err
	}
	data, err := json.Marshal(input)
	return sha256.Sum256(data), err
}

type operationTarget struct {
	root, path string
	operation  config.Operation
	intent     access.Intent
}

func target(input PathInput, op config.Operation, intent access.Intent) operationTarget {
	return operationTarget{root: input.Root, path: input.Path, operation: op, intent: intent}
}

// gate authorizes all targets before prompting. A valid retry carries approval
// for this tool's exact arguments; no retry can fall through as a fresh call.
func (s toolServer) gate(req *mcp.CallToolRequest, input any, targets []operationTarget, permanent bool) (*pendingRecord, string, *mcp.CallToolResult, error) {
	digest, err := argumentDigest(req.Params.Arguments)
	if err != nil {
		return nil, "", nil, err
	}
	var record *pendingRecord
	choice := ""
	if req.Params.RequestState != "" || req.Params.InputResponses != nil {
		var ok bool
		record, ok = s.pending.take(req.Params.RequestState, req.Params.Name, digest)
		if !ok {
			return nil, "", confirmationErrorWithRecord("invalid_confirmation", record), nil
		}
		response, ok := req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
		if !ok || response == nil || len(req.Params.InputResponses) != 1 {
			return nil, "", confirmationErrorWithRecord("invalid_confirmation", record), nil
		}
		if response.Action == "decline" || response.Action == "cancel" {
			return nil, "", cancelled(record), nil
		}
		if response.Action != "accept" || len(response.Content) != 1 {
			return nil, "", confirmationErrorWithRecord("invalid_confirmation", record), nil
		}
		choice, ok = response.Content["choice"].(string)
		if !ok || !validChoice(record.kind, choice) {
			return nil, "", confirmationErrorWithRecord("invalid_confirmation", record), nil
		}
		if choice == "cancel" {
			return nil, "", cancelled(record), nil
		}
	}
	asks := permanent
	confirmationTarget := operationTarget{}
	for _, t := range targets {
		if _, err := s.access.Resolve(t.root, t.path, t.operation, t.intent); err != nil {
			result, unexpected := failureResult(filesystem.MapError(t.root, t.path, t.operation, err))
			preserveRecordRecovery(result, record)
			return nil, "", result, unexpected
		}
		if s.access.Asks(t.root, t.operation) {
			asks = true
			if confirmationTarget.root == "" {
				confirmationTarget = t
			}
		}
	}
	if permanent {
		fingerprint, err := s.fingerprint(targets[0])
		if err != nil {
			result, unexpected := failureResult(err)
			return nil, "", result, unexpected
		}
		if record == nil || record.kind != "permanent" || record.fingerprint != fingerprint {
			result, err := s.prompt(req, pendingRecord{tool: req.Params.Name, arguments: digest, kind: "permanent", fingerprint: fingerprint, target: targets[0]}, fmt.Sprintf("Permanently delete %s:%s? This cannot be moved back from Trash.", targets[0].root, targets[0].path))
			return nil, "", result, err
		}
	} else if record == nil && asks {
		result, err := s.prompt(req, pendingRecord{tool: req.Params.Name, arguments: digest, kind: "approval", target: confirmationTarget}, fmt.Sprintf("Allow %s with these arguments? %s", req.Params.Name, mustJSON(input)))
		return nil, "", result, err
	}
	return record, choice, nil, nil
}

func preserveRecordRecovery(result *mcp.CallToolResult, record *pendingRecord) {
	if result == nil || record == nil {
		return
	}
	if toolError, ok := result.StructuredContent.(ToolError); ok {
		toolError.RecoveryPaths = uniquePaths(append(toolError.RecoveryPaths, recordRecoveryPaths(record)...))
		setToolErrorResult(result, toolError)
	}
}

func recordRecoveryPaths(record *pendingRecord) []string {
	if record == nil {
		return nil
	}
	return uniquePaths(append(append([]string(nil), record.recoveryPaths...), conflictRecoveryPaths(record.conflict)...))
}

func validChoice(kind, choice string) bool {
	if kind == "conflict" {
		return choice == "reload_and_rebase" || choice == "overwrite" || choice == "cancel"
	}
	return (kind == "approval" || kind == "permanent") && (choice == "proceed" || choice == "cancel")
}

func mustJSON(input any) string { data, _ := json.Marshal(input); return string(data) }

func (s toolServer) prompt(req *mcp.CallToolRequest, record pendingRecord, message string) (*mcp.CallToolResult, error) {
	if record.kind == "conflict" && record.conflict != nil {
		output := conflictOutput("awaiting_resolution", record.conflict)
		if err := validateEditOutputSize(output); err != nil {
			return checkedConflictResult("awaiting_resolution", record.conflict, record.target, ""), nil
		}
		if paths := conflictRecoveryPaths(record.conflict); len(paths) > 0 {
			message += " Retained recovery paths: " + strings.Join(paths, ", ") + "."
		}
	}
	initialize := req.Session.InitializeParams()
	if initialize == nil || initialize.Capabilities == nil || initialize.Capabilities.Elicitation == nil {
		return unavailableConfirmation(record), nil
	}
	capabilities := initialize.Capabilities.Elicitation
	if capabilities.Form == nil && capabilities.URL != nil {
		return unavailableConfirmation(record), nil
	}
	choices := []any{"proceed", "cancel"}
	if record.kind == "conflict" {
		choices = []any{"reload_and_rebase", "overwrite", "cancel"}
	}
	token, err := s.pending.put(record)
	if err != nil {
		return nil, err
	}
	result := &mcp.CallToolResult{RequestState: token, InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{
		Message:         message,
		RequestedSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"choice": {Type: "string", Enum: choices}}, Required: []string{"choice"}},
	}}}
	return result, nil
}

func confirmationError(code string) *mcp.CallToolResult {
	return confirmationErrorWithRecord(code, nil)
}

func confirmationErrorWithRecord(code string, record *pendingRecord) *mcp.CallToolResult {
	message := "confirmation was invalid, expired, or already consumed; no operation was performed"
	if code == "elicitation_unavailable" {
		message = "confirmation is required but this client does not support form elicitation"
	}
	if code == "confirmation_declined" {
		message = "confirmation was declined; no operation was performed"
	}
	toolError := ToolError{Code: code, Message: message}
	if record != nil {
		toolError.Root = record.target.root
		toolError.Path = record.target.path
		toolError.Operation = string(record.target.operation)
		if record.conflict != nil {
			pending := conflictOutput("cancelled", record.conflict).Pending
			toolError.Pending = &pending
		}
		toolError.RecoveryPaths = recordRecoveryPaths(record)
	}
	return toolErrorResult(toolError)
}

func unavailableConfirmation(record pendingRecord) *mcp.CallToolResult {
	result := ToolError{Code: "elicitation_unavailable", Root: record.target.root, Path: record.target.path, Operation: string(record.target.operation), Message: "confirmation is required but this client does not support form elicitation"}
	if record.kind == "conflict" && record.conflict != nil {
		pending := conflictOutput("cancelled", record.conflict).Pending
		result.Pending = &pending
		result.RecoveryPaths = conflictRecoveryPaths(record.conflict)
	}
	return toolErrorResult(result)
}

func cancelled(record *pendingRecord) *mcp.CallToolResult {
	if record.kind == "conflict" && record.conflict != nil {
		return checkedConflictResult("cancelled", record.conflict, record.target, "edit cancelled; proposal retained")
	}
	return confirmationError("confirmation_declined")
}

// Fingerprints describe the requested directory entry, including a final
// symlink itself. Opening the original parent keeps this aligned with deletion.
type fileFingerprint struct {
	mode     uint16
	size     int64
	modified unix.Timespec
	device   int32
	inode    uint64
}

func (s toolServer) fingerprint(t operationTarget) (fileFingerprint, error) {
	path, err := s.access.Resolve(t.root, t.path, t.operation, access.Existing)
	if err != nil {
		return fileFingerprint{}, filesystem.MapError(t.root, t.path, t.operation, err)
	}
	parent, remaining, err := path.OpenRequestedParent()
	if err != nil {
		return fileFingerprint{}, filesystem.MapError(t.root, t.path, t.operation, err)
	}
	defer parent.Close()
	if len(remaining) != 1 {
		return fileFingerprint{}, &filesystem.Error{Root: t.root, Path: t.path, Operation: t.operation, Code: "path_outside_root"}
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), remaining[0], &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileFingerprint{}, filesystem.MapError(t.root, t.path, t.operation, err)
	}
	return fileFingerprint{mode: stat.Mode, size: stat.Size, modified: stat.Mtim, device: stat.Dev, inode: stat.Ino}, nil
}
