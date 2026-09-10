package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestEditRequiresReadAndEditWithLoaderValidRules(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	for _, ask := range [][]config.Operation{{config.OpRead}, {config.OpEdit}} {
		manager := loaderValidEditManager(t, ask)
		root := manager.Roots()[0].Path
		writeTestFile(t, filepath.Join(root, "note"), []byte("old"))
		prompts := 0
		opts := choose("proceed")
		opts.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			prompts++
			return acceptance("proceed"), nil
		}
		result := callTool(t, mutationSession(t, manager, opts), "edit_file", map[string]any{"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("old")), "proposed_content": "new"})
		if result.IsError || prompts != 1 {
			t.Fatalf("ask %v: result=%#v prompts=%d", ask, result, prompts)
		}
	}
}

func TestConflictUnavailablePreservesContextAndProposal(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	manager := loaderValidEditManager(t, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("current"))
	result := callTool(t, mutationSession(t, manager, nil), "edit_file", map[string]any{"root": "vault", "path": "note", "expected_revision": "stale", "proposed_content": "agent"})
	if !result.IsError || structuredString(t, result, "code") != "elicitation_unavailable" || structuredString(t, result, "root") != "vault" || structuredString(t, result, "path") != "note" || structuredString(t, result, "operation") != "edit" || structuredString(t, result, "pending.proposed_content") != "agent" {
		t.Fatalf("result = %#v", result)
	}
}

func loaderValidEditManager(t *testing.T, ask []config.Operation) *access.Manager {
	t.Helper()
	root := t.TempDir()
	askText := ""
	if len(ask) > 0 {
		askText = string(ask[0])
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf("version: 1\ndirectories:\n  - name: vault\n    path: %q\n    allow: [read, edit]\n    ask: [%s]\n    on_conflict: ask\n", root, askText)
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := access.New(cfg.Directories)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestEditWithoutReadIsDeniedBeforeContentDisclosure(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpEdit}, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("secret"))
	result := callTool(t, mutationSession(t, manager, nil), "edit_file", map[string]any{"root": "vault", "path": "note", "expected_revision": "stale", "proposed_content": "agent"})
	if !result.IsError || structuredString(t, result, "code") != "operation_not_allowed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRecoveryOutputKeepsFilesystemContext(t *testing.T) {
	err := &filesystem.RecoveryError{Outcome: filesystem.RecoveryOutcome{Root: "vault", Path: "note", Operation: config.OpEdit, TargetState: filesystem.TargetUnknown, RecoveryPath: ".recovery"}}
	result, unexpected := failureResult(err)
	if unexpected != nil || structuredString(t, result, "root") != "vault" || structuredString(t, result, "path") != "note" || structuredString(t, result, "operation") != "edit" {
		t.Fatalf("result = %#v %v", result, unexpected)
	}
}

func TestSuccessfulEditReturnsRecoveryPathReadableThroughMCP(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	manager := loaderValidEditManager(t, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("old"))
	session := mutationSession(t, manager, nil)
	result := callTool(t, session, "edit_file", map[string]any{"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("old")), "proposed_content": "new"})
	recoveryPath := structuredString(t, result, "recovery_path")
	if result.IsError || recoveryPath == "" {
		t.Fatalf("edit = %#v", result)
	}
	recovery := callTool(t, session, "read_text_file", map[string]any{"root": "vault", "path": recoveryPath})
	if recovery.IsError || structuredString(t, recovery, "content") != "old" {
		t.Fatalf("recovery = %#v", recovery)
	}
}

func TestSpecificMutationMessagesAndSafeErrnoDetail(t *testing.T) {
	for _, code := range []string{"destination_exists", "invalid_edit", "atomic_replace_unsupported", "cross_filesystem_move_unsupported", "unknown_root", "partial_delete"} {
		if got := conciseMessage(code); got == conciseMessage("filesystem_unavailable") {
			t.Fatalf("generic message for %s", code)
		}
	}
	result, unexpected := failureResult(filesystem.MapError("vault", "note", config.OpEdit, &os.PathError{Op: "open", Path: "/secret/absolute/path", Err: syscall.EIO}))
	if unexpected != nil || structuredString(t, result, "detail") == "" || strings.Contains(mustJSON(result), "/secret/absolute/path") {
		t.Fatalf("safe detail = %#v %v", result, unexpected)
	}
}

func TestEntryOutputIncludesTimestamps(t *testing.T) {
	out := entryOutput(filesystem.Entry{CreatedAt: "2026-01-01T00:00:00Z", ModifiedAt: "2026-01-02T00:00:00Z"})
	if out.CreatedAt == "" || out.ModifiedAt == "" {
		t.Fatalf("output = %#v", out)
	}
	manager := testManager(t, []config.Operation{config.OpList}, nil)
	writeTestFile(t, filepath.Join(manager.Roots()[0].Path, "note"), []byte("x"))
	result := callTool(t, mutationSession(t, manager, nil), "stat_path", map[string]any{"root": "vault", "path": "note"})
	if result.IsError || structuredString(t, result, "entry.created_at") == "" || structuredString(t, result, "entry.modified_at") == "" {
		t.Fatalf("stat = %#v", result)
	}
}

func TestPendingEditEnvelopeBudgetBoundaries(t *testing.T) {
	proposal := PendingEditOutput{ExpectedRevision: "r", ProposedContent: "", Edits: []TextEditInput{}}
	base, err := pendingEditEnvelopeSize(proposal)
	if err != nil {
		t.Fatal(err)
	}
	proposal.ProposedContent = strings.Repeat("x", maximumPendingEditEnvelopeBytes-base)
	if err := validatePendingEditEnvelope(proposal); err != nil {
		t.Fatalf("exact proposal boundary: %v", err)
	}
	copyAtBoundary := proposal.ProposedContent
	proposal.ProposedContent += "x"
	if err := validatePendingEditEnvelope(proposal); err == nil {
		t.Fatal("oversized proposal accepted")
	}
	if copyAtBoundary != strings.Repeat("x", len(copyAtBoundary)) {
		t.Fatal("accepted proposal changed")
	}

	edits := PendingEditOutput{ExpectedRevision: "r", Edits: []TextEditInput{{OldText: "a", NewText: "", ExpectedOccurrences: 1}}}
	base, err = pendingEditEnvelopeSize(edits)
	if err != nil {
		t.Fatal(err)
	}
	edits.Edits[0].NewText = strings.Repeat("x", maximumPendingEditEnvelopeBytes-base)
	if err := validatePendingEditEnvelope(edits); err != nil {
		t.Fatalf("exact edit boundary: %v", err)
	}
	edits.Edits[0].NewText += "x"
	if err := validatePendingEditEnvelope(edits); err == nil {
		t.Fatal("oversized edit array accepted")
	}
}

func TestOversizedPendingEditFailsBeforeFilesystemAccess(t *testing.T) {
	manager := loaderValidEditManager(t, nil)
	result := callTool(t, mutationSession(t, manager, nil), "edit_file", map[string]any{"root": "vault", "path": "missing", "expected_revision": "r", "proposed_content": strings.Repeat("x", maximumPendingEditEnvelopeBytes)})
	if !result.IsError || structuredString(t, result, "code") != "response_too_large" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOversizedChangedContinuationPreservesRecoveryAndCorrectRetry(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	session, store, root := conflictContinuationSession(t)
	args := conflictArgs("current")
	token := seedConflict(t, store, args, "current", ".saved-proposal")
	changed := conflictArgs("current")
	changed["proposed_content"] = strings.Repeat("x", maximumPendingEditEnvelopeBytes)
	result := retry(t, session, "edit_file", changed, token, acceptance("overwrite"))
	if !result.IsError || structuredString(t, result, "code") != "invalid_confirmation" {
		t.Fatalf("oversized continuation = %#v", result)
	}
	assertRecoveryPaths(t, result, ".saved-proposal")
	assertFile(t, filepath.Join(root, "note"), "current")
	result = retry(t, session, "edit_file", args, token, acceptance("cancel"))
	if result.IsError || structuredString(t, result, "status") != "cancelled" {
		t.Fatalf("correct retry = %#v", result)
	}
	assertRecoveryPaths(t, result, ".saved-proposal")
}

func TestReloadAndRebaseRejectsCombinedGrowthWithoutTruncatingProposalOrRecoveryPaths(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	session, store, root := conflictContinuationSession(t)
	proposal := strings.Repeat("p", 6<<20)
	args := map[string]any{"root": "vault", "path": "note", "expected_revision": "stale", "proposed_content": proposal}
	token := seedConflict(t, store, args, "current", ".saved-proposal")
	assertFileWrite(t, filepath.Join(root, "note"), strings.Repeat("g", 6<<20))

	result := retry(t, session, "edit_file", args, token, acceptance("reload_and_rebase"))
	if !result.IsError || structuredString(t, result, "code") != "response_too_large" {
		t.Fatalf("result = %#v", result)
	}
	if got := structuredString(t, result, "pending.proposed_content"); got != proposal {
		t.Fatalf("proposal length = %d want %d", len(got), len(proposal))
	}
	assertRecoveryPaths(t, result, ".saved-proposal")
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maximumPendingEditEnvelopeBytes {
		t.Fatalf("structured error size = %d", len(data))
	}
}

func TestBoundedToolErrorFinalFallbackAlwaysFitsAndDisclosesOmittedLedger(t *testing.T) {
	t.Run("nonessential context omitted before recovery paths", func(t *testing.T) {
		result := toolErrorResult(ToolError{
			Code:          "recovery_required",
			Root:          strings.Repeat("r", maximumPendingEditEnvelopeBytes),
			Path:          strings.Repeat("p", maximumPendingEditEnvelopeBytes),
			Operation:     "edit",
			Message:       "oversized context",
			RecoveryPaths: []string{".saved-proposal"},
		})
		assertStructuredWithinEditBudget(t, result)
		assertRecoveryPaths(t, result, ".saved-proposal")
		if structuredString(t, result, "root") != "" || structuredString(t, result, "path") != "" {
			t.Fatal("oversized nonessential context was retained")
		}
	})

	t.Run("unrepresentable recovery ledger is explicit", func(t *testing.T) {
		result := toolErrorResult(ToolError{
			Code:          "recovery_required",
			Message:       "oversized ledger",
			RecoveryPaths: []string{strings.Repeat("x", maximumPendingEditEnvelopeBytes)},
		})
		assertStructuredWithinEditBudget(t, result)
		if got := structuredNumber(t, result, "recovery_paths_omitted"); got != 1 {
			t.Fatalf("omitted recovery paths = %v", got)
		}
		if !strings.Contains(structuredString(t, result, "message"), "recovery") {
			t.Fatal("omitted recovery ledger was not disclosed")
		}
	})
}

func TestOverwriteReservesRecoveryLedgerBeforeMutation(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	session, store, root := conflictContinuationSession(t)
	args := conflictArgs("current")
	// This ledger still fits a minimal error, but not after reserving the
	// maximum root-relative path that one more atomic swap can retain.
	path := strings.Repeat("r", maximumPendingEditEnvelopeBytes-2048)
	token := seedConflict(t, store, args, "current", path)
	result := retry(t, session, "edit_file", args, token, acceptance("overwrite"))
	if !result.IsError || structuredString(t, result, "code") != "response_too_large" {
		t.Fatalf("code = %q error=%t", structuredString(t, result, "code"), result.IsError)
	}
	assertRecoveryPaths(t, result, path)
	assertStructuredWithinEditBudget(t, result)
	assertFile(t, filepath.Join(root, "note"), "current")
}

func TestRecoveryLedgerReservationAccountsForWorstCaseJSONEscaping(t *testing.T) {
	// A literal 4,096-byte placeholder would still fit here, but an actual path
	// may require six JSON bytes per input byte (for example control bytes).
	paths := []string{strings.Repeat("r", maximumPendingEditEnvelopeBytes-5000)}
	if err := validateRecoveryLedgerReservation(paths); err == nil {
		t.Fatal("reservation ignored worst-case JSON escaping")
	}
}

func TestRollbackRecoveryPathsSurviveConflictContinuations(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	for _, action := range []string{"cancel", "decline"} {
		t.Run(action, func(t *testing.T) {
			session, store, root := conflictContinuationSession(t)
			args := conflictArgs("current")
			token := seedConflict(t, store, args, "current", ".saved-proposal")
			var response mcp.InputResponse = acceptance("cancel")
			if action == "decline" {
				response = &mcp.ElicitResult{Action: "decline"}
			}
			result := retry(t, session, "edit_file", args, token, response)
			assertRecoveryPaths(t, result, ".saved-proposal")
			assertFile(t, filepath.Join(root, "note"), "current")
		})
	}

	t.Run("overwrite success", func(t *testing.T) {
		session, store, root := conflictContinuationSession(t)
		args := conflictArgs("current")
		result := retry(t, session, "edit_file", args, seedConflict(t, store, args, "current", ".saved-proposal"), acceptance("overwrite"))
		paths := recoveryPaths(t, result)
		if result.IsError || len(paths) != 2 || paths[0] != ".saved-proposal" || paths[1] == "" {
			t.Fatalf("result = %#v paths=%v", result, paths)
		}
		for _, path := range paths {
			if _, err := os.Stat(filepath.Join(root, path)); err != nil {
				t.Fatalf("recovery path %q not openable: %v", path, err)
			}
		}
	})

	t.Run("reload and rebase", func(t *testing.T) {
		session, store, _ := conflictContinuationSession(t)
		args := conflictArgs("current")
		result := retry(t, session, "edit_file", args, seedConflict(t, store, args, "current", ".saved-proposal"), acceptance("reload_and_rebase"))
		if result.IsError || result.NeedsInput() || structuredString(t, result, "status") != "reload_and_rebase" {
			t.Fatalf("result = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
	})

	t.Run("repeated conflict", func(t *testing.T) {
		session, store, root := conflictContinuationSession(t)
		args := conflictArgs("current")
		token := seedConflict(t, store, args, "current", ".saved-proposal")
		assertFileWrite(t, filepath.Join(root, "note"), "newer")
		result := retry(t, session, "edit_file", args, token, acceptance("overwrite"))
		if !result.NeedsInput() {
			t.Fatalf("not a prompt: %#v", result)
		}
		if !strings.Contains(promptMessage(t, result), ".saved-proposal") {
			t.Fatalf("prompt omitted recovery path: %#v", result)
		}
	})

	t.Run("retry failure", func(t *testing.T) {
		session, store, root := conflictContinuationSession(t)
		args := conflictArgs("current")
		token := seedConflict(t, store, args, "current", ".saved-proposal")
		if err := os.Remove(filepath.Join(root, "note")); err != nil {
			t.Fatal(err)
		}
		result := retry(t, session, "edit_file", args, token, acceptance("overwrite"))
		if !result.IsError {
			t.Fatalf("result = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
	})

	t.Run("expired", func(t *testing.T) {
		session, store, _ := conflictContinuationSession(t)
		args := conflictArgs("current")
		digest := digestArgs(t, args)
		token := "expired-token"
		store.records = map[string]pendingRecord{token: {tool: "edit_file", arguments: digest, kind: "conflict", expires: time.Now().Add(-time.Second), conflict: recoveryConflictRecord("current", ".saved-proposal"), target: operationTarget{root: "vault", path: "note", operation: config.OpEdit, intent: access.Existing}}}
		// A later prompt performs expiry housekeeping before the original client
		// retries. That cleanup must retain a compact recovery-path tombstone.
		if _, err := store.put(pendingRecord{tool: "create_file", kind: "approval"}); err != nil {
			t.Fatal(err)
		}
		result := retry(t, session, "edit_file", args, token, acceptance("overwrite"))
		if !result.IsError {
			t.Fatalf("result = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
	})

	t.Run("invalid continuation", func(t *testing.T) {
		session, store, _ := conflictContinuationSession(t)
		args := conflictArgs("current")
		result := retry(t, session, "edit_file", args, seedConflict(t, store, args, "current", ".saved-proposal"), acceptance("bogus"))
		if !result.IsError {
			t.Fatalf("result = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
	})
}

func TestMismatchedConflictContinuationsPreserveRecoveryAndCorrectRetry(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	t.Run("changed arguments", func(t *testing.T) {
		session, store, _ := conflictContinuationSession(t)
		args := conflictArgs("current")
		token := seedConflict(t, store, args, "current", ".saved-proposal")
		changed := conflictArgs("current")
		changed["proposed_content"] = "tampered"
		result := retry(t, session, "edit_file", changed, token, acceptance("cancel"))
		if !result.IsError || structuredString(t, result, "code") != "invalid_confirmation" {
			t.Fatalf("changed arguments = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
		result = retry(t, session, "edit_file", args, token, acceptance("cancel"))
		if result.IsError || structuredString(t, result, "status") != "cancelled" {
			t.Fatalf("correct retry = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
	})

	t.Run("wrong tool", func(t *testing.T) {
		store := &pendingStore{}
		tools := toolServer{pending: store}
		srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		handler := func(_ context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
			_, _, result, err := tools.gate(req, input, nil, false)
			if result != nil || err != nil {
				return result, nil, err
			}
			return successResult(MutationOutput{Root: input.Root, Path: input.Path}, "continued"), nil, nil
		}
		mcp.AddTool(srv, mutationTool[ConflictOutput]("right_tool", "test", true), handler)
		mcp.AddTool(srv, mutationTool[ConflictOutput]("wrong_tool", "test", true), handler)
		a, b := mcp.NewInMemoryTransports()
		ss, err := srv.Connect(context.Background(), a, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ss.Close() })
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, manualOptions()).Connect(context.Background(), b, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		args := map[string]any{"root": "vault", "path": "note"}
		token, err := store.put(pendingRecord{tool: "right_tool", arguments: digestArgs(t, args), kind: "conflict", conflict: recoveryConflictRecord("current", ".saved-proposal"), target: operationTarget{root: "vault", path: "note", operation: config.OpEdit, intent: access.Existing}})
		if err != nil {
			t.Fatal(err)
		}
		result := retry(t, cs, "wrong_tool", args, token, acceptance("cancel"))
		if !result.IsError || structuredString(t, result, "code") != "invalid_confirmation" {
			t.Fatalf("wrong tool = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
		result = retry(t, cs, "right_tool", args, token, acceptance("cancel"))
		if result.IsError || structuredString(t, result, "status") != "cancelled" {
			t.Fatalf("correct tool = %#v", result)
		}
		assertRecoveryPaths(t, result, ".saved-proposal")
	})
}

func TestRollbackRecoveryPathIsDisclosedBeforeAndWithoutConflictElicitation(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	for _, tc := range []struct {
		name       string
		options    *mcp.ClientOptions
		needsInput bool
	}{
		{"abandoned prompt", manualOptions(), true},
		{"elicitation unavailable", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pendingStore{}
			tools := toolServer{pending: store}
			srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			mcp.AddTool(srv, mutationTool[ConflictOutput]("conflict_test", "test", true), func(_ context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
				digest, err := argumentDigest(req.Params.Arguments)
				if err != nil {
					return nil, nil, err
				}
				conflict := recoveryConflictRecord("current", ".saved-proposal")
				result, err := tools.prompt(req, pendingRecord{tool: req.Params.Name, arguments: digest, kind: "conflict", revision: conflict.CurrentRevision, conflict: conflict, target: target(input, config.OpEdit, access.Existing)}, "resolve")
				return result, nil, err
			})
			a, b := mcp.NewInMemoryTransports()
			ss, err := srv.Connect(context.Background(), a, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close() })
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, tc.options).Connect(context.Background(), b, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			result := callTool(t, cs, "conflict_test", map[string]any{"root": "vault", "path": "note"})
			if result.NeedsInput() != tc.needsInput {
				t.Fatalf("needs input = %t result=%#v", result.NeedsInput(), result)
			}
			if tc.needsInput {
				if !strings.Contains(promptMessage(t, result), ".saved-proposal") {
					t.Fatalf("prompt omitted path: %#v", result)
				}
			} else {
				assertRecoveryPaths(t, result, ".saved-proposal")
			}
		})
	}
}

func conflictContinuationSession(t *testing.T) (*mcp.ClientSession, *pendingStore, string) {
	t.Helper()
	manager := loaderValidEditManager(t, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("current"))
	writeTestFile(t, filepath.Join(root, ".saved-proposal"), []byte("proposal"))
	store := &pendingStore{}
	tools := toolServer{filesystem: filesystem.New(manager, filesystem.Options{}), access: manager, pending: store}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	tools.registerWrites(srv)
	a, b := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, manualOptions()).Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, store, root
}

func conflictArgs(current string) map[string]any {
	return map[string]any{"root": "vault", "path": "note", "expected_revision": "stale", "proposed_content": "agent"}
}

func seedConflict(t *testing.T, store *pendingStore, args map[string]any, current, recoveryPath string) string {
	t.Helper()
	token, err := store.put(pendingRecord{tool: "edit_file", arguments: digestArgs(t, args), kind: "conflict", revision: filesystem.Revision([]byte(current)), conflict: recoveryConflictRecord(current, recoveryPath), target: operationTarget{root: "vault", path: "note", operation: config.OpEdit, intent: access.Existing}})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func recoveryConflictRecord(current, path string) *filesystem.Conflict {
	return &filesystem.Conflict{CurrentContent: current, CurrentRevision: filesystem.Revision([]byte(current)), Pending: filesystem.EditRequest{ExpectedRevision: "stale", ProposedContent: "agent"}, RecoveryPaths: []string{path}}
}

func digestArgs(t *testing.T, args map[string]any) [32]byte {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := argumentDigest(data)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func recoveryPaths(t *testing.T, result *mcp.CallToolResult) []string {
	t.Helper()
	value := structuredValue(t, result, "recovery_paths")
	raw, _ := value.([]any)
	paths := make([]string, 0, len(raw))
	for _, item := range raw {
		if path, ok := item.(string); ok {
			paths = append(paths, path)
		}
	}
	return paths
}
func assertRecoveryPaths(t *testing.T, result *mcp.CallToolResult, want ...string) {
	t.Helper()
	got := recoveryPaths(t, result)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery paths = %v want %v; result=%#v", got, want, result)
	}
}
func assertStructuredWithinEditBudget(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maximumPendingEditEnvelopeBytes {
		t.Fatalf("structured output size = %d", len(data))
	}
}
func assertFileWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func promptMessage(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	prompt, ok := result.InputRequests["confirm"].(*mcp.ElicitParams)
	if !ok {
		t.Fatalf("missing prompt: %#v", result)
	}
	return prompt.Message
}
