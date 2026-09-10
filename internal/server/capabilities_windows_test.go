package server

import (
	"context"
	"encoding/json"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestWindowsUnsupportedOperationsRejectBeforePrompt(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpRead, config.OpEdit, config.OpTrash}, []config.Operation{config.OpRead, config.OpEdit, config.OpTrash})
	defer manager.Close()
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("original"))
	srv := New(filesystem.New(manager, filesystem.Options{}), manager)
	a, b := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	var prompts atomic.Int32
	client := mcp.NewClient(&mcp.Implementation{Name: "windows-contract", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		prompts.Add(1)
		return &mcp.ElicitResult{Action: "decline"}, nil
	}})
	cs, err := client.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, tc := range []struct {
		name, code string
		args       map[string]any
	}{
		{"edit_file", "atomic_replace_unsupported", map[string]any{"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("original")), "proposed_content": "replacement"}},
		{"trash_path", "trash_unsupported", map[string]any{"root": "vault", "path": "note"}},
	} {
		got := callTool(t, cs, tc.name, tc.args)
		var out ToolError
		raw, err := json.Marshal(got.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if !got.IsError || got.NeedsInput() || out.Code != tc.code {
			t.Fatalf("%s: %#v", tc.name, got)
		}
	}
	if prompts.Load() != 0 {
		t.Fatal("unsupported operation prompted")
	}
	got := callTool(t, cs, "list_roots", map[string]any{})
	var roots ListRootsOutput
	raw, _ := json.Marshal(got.StructuredContent)
	if err := json.Unmarshal(raw, &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots.Roots) != 1 || len(roots.Roots[0].Unsupported) != 2 || len(roots.Roots[0].Allow) != 3 {
		t.Fatalf("capabilities/policy: %#v", roots)
	}
	data, err := os.ReadFile(filepath.Join(root, "note"))
	if err != nil || string(data) != "original" {
		t.Fatalf("source changed: %q %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected artifacts: %v %v", entries, err)
	}
}
