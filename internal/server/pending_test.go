package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPendingMalformedRetriesConsumeToken(t *testing.T) {
	for _, kind := range []string{"bad_choice", "missing_choice", "extra_choice", "wrong_action", "decline", "missing_response"} {
		t.Run(kind, func(t *testing.T) {
			manager := testManager(t, []config.Operation{config.OpCreate}, []config.Operation{config.OpCreate})
			root := manager.Roots()[0].Path
			session := mutationSession(t, manager, manualOptions())
			args := map[string]any{"root": "vault", "path": "note", "content": "new"}
			first := callTool(t, session, "create_file", args)
			assertPrompt(t, first, "proceed", "cancel")
			name := "create_file"
			retryArgs := map[string]any{"root": "vault", "path": "note", "content": "new"}
			response := acceptance("proceed")
			switch kind {
			case "bad_choice":
				response = acceptance("overwrite")
			case "missing_choice":
				response.Content = map[string]any{}
			case "extra_choice":
				response.Content["extra"] = true
			case "wrong_action":
				response.Action = "other"
			case "decline":
				response.Action = "decline"
			}
			var result *mcp.CallToolResult
			if kind == "missing_response" {
				var err error
				result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: retryArgs, RequestState: first.RequestState})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				result = retry(t, session, name, retryArgs, first.RequestState, response)
			}
			if !result.IsError {
				t.Fatalf("accepted %s: %#v", kind, result)
			}
			if _, err := os.Stat(filepath.Join(root, "note")); !os.IsNotExist(err) {
				t.Fatalf("mutated for %s: %v", kind, err)
			}
			reused := retry(t, session, "create_file", args, first.RequestState, acceptance("proceed"))
			if !reused.IsError || structuredString(t, reused, "code") != "invalid_confirmation" {
				t.Fatalf("token survived %s: %#v", kind, reused)
			}
		})
	}
}

func TestPendingExpiredAndConcurrentOneUse(t *testing.T) {
	store := &pendingStore{}
	digest := sha256.Sum256([]byte("arguments"))
	token, err := store.put(pendingRecord{tool: "edit_file", arguments: digest, kind: "approval"})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	record := store.records[token]
	record.expires = time.Now().Add(-time.Second)
	store.records[token] = record
	store.mu.Unlock()
	if _, ok := store.take(token, "edit_file", digest); ok {
		t.Fatal("accepted expired token")
	}
	token, err = store.put(pendingRecord{tool: "edit_file", arguments: digest, kind: "approval"})
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			if _, ok := store.take(token, "edit_file", digest); ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("token accepted %d times", accepted.Load())
	}
}

func TestPendingExpiredRetryDoesNotWrite(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpCreate}, []config.Operation{config.OpCreate})
	root := manager.Roots()[0].Path
	tools := toolServer{access: manager, filesystem: filesystem.New(manager, filesystem.Options{}), pending: &pendingStore{}}
	// Real MCP sessions expose the gate with a store whose expiry can be advanced
	// deterministically; no five-minute sleep or production clock injection.
	srv := mcp.NewServer(&mcp.Implementation{Name: "expiry-test", Version: "1"}, nil)
	tools.registerWrites(srv)
	a, b := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close(); _ = manager.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, manualOptions()).Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	args := map[string]any{"root": "vault", "path": "note", "content": "new"}
	first := callTool(t, cs, "create_file", args)
	tools.pending.mu.Lock()
	record := tools.pending.records[first.RequestState]
	record.expires = time.Now().Add(-time.Second)
	tools.pending.records[first.RequestState] = record
	tools.pending.mu.Unlock()
	result := retry(t, cs, "create_file", args, first.RequestState, acceptance("proceed"))
	if !result.IsError || structuredString(t, result, "code") != "invalid_confirmation" {
		t.Fatalf("expired result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "note")); !os.IsNotExist(err) {
		t.Fatalf("disk = %v", err)
	}
}

func TestPendingCanonicalArgumentsPreserveAllSuppliedFields(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpRead, config.OpEdit}, []config.Operation{config.OpEdit})
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("old"))
	session := mutationSession(t, manager, manualOptions())
	args := map[string]any{"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("old")), "proposed_content": "new"}
	first := callTool(t, session, "edit_file", args)
	args["edits"] = []any{}
	result := retry(t, session, "edit_file", args, first.RequestState, acceptance("proceed"))
	if !result.IsError {
		t.Fatalf("changed supplied fields accepted: %#v", result)
	}
	assertFile(t, filepath.Join(root, "note"), "old")
}

func TestPendingCanonicalArgumentsIgnoreObjectKeyOrder(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpCreate}, []config.Operation{config.OpCreate})
	root := manager.Roots()[0].Path
	session := mutationSession(t, manager, manualOptions())
	first := callTool(t, session, "create_file", map[string]any{"root": "vault", "path": "note", "content": "new"})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_file", Arguments: json.RawMessage(`{"path":"note", "root":"vault", "content":"new"}`), RequestState: first.RequestState, InputResponses: mcp.InputResponseMap{"confirm": acceptance("proceed")}})
	if err != nil || result.IsError {
		t.Fatalf("reordered arguments = %#v, %v", result, err)
	}
	assertFile(t, filepath.Join(root, "note"), "new")
}
