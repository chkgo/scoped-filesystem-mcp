package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mutationSession(t *testing.T, manager *access.Manager, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	t.Cleanup(func() { _ = manager.Close() })
	srv := New(filesystem.New(manager, filesystem.Options{TrashDir: t.TempDir()}), manager)
	a, b := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts).Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestAskAllMutationsAcceptOrDecline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		op          config.Operation
		args        map[string]any
		destination string
	}{
		{"create_directory", config.OpCreate, map[string]any{"root": "vault", "path": "parent/new", "parents": true}, "parent/new"},
		{"create_file", config.OpCreate, map[string]any{"root": "vault", "path": "new", "content": "new text"}, "new"},
		{"edit_file", config.OpEdit, map[string]any{"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("old")), "proposed_content": "new text", "edits": []any{map[string]any{"old_text": "old", "new_text": "new text", "expected_occurrences": 1}}}, "note"},
		{"move_path", config.OpMove, map[string]any{"source_root": "vault", "source_path": "note", "destination_root": "vault", "destination_path": "new"}, "new"},
		{"trash_path", config.OpTrash, map[string]any{"root": "vault", "path": "note"}, ""},
		{"permanent_delete_path", config.OpPermanentDelete, map[string]any{"root": "vault", "path": "note"}, ""},
	} {
		for _, action := range []string{"proceed", "decline"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				if tc.op == config.OpEdit && !platform.SupportsAtomicEdit {
					t.Skip("atomic edits unavailable")
				}
				allow := []config.Operation{tc.op}
				if tc.op == config.OpEdit {
					allow = []config.Operation{config.OpRead, config.OpEdit}
				}
				manager := testManager(t, allow, []config.Operation{tc.op})
				root := manager.Roots()[0].Path
				writeTestFile(t, filepath.Join(root, "note"), []byte("old"))
				prompts := 0
				opts := choose("proceed")
				opts.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					prompts++
					data, err := os.ReadFile(filepath.Join(root, "note"))
					if err != nil || string(data) != "old" {
						return nil, fmt.Errorf("changed before approval: %q, %v", data, err)
					}
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 1 {
						return nil, fmt.Errorf("created before approval: %v, %v", entries, err)
					}
					if action == "decline" {
						return &mcp.ElicitResult{Action: "decline"}, nil
					}
					return acceptance("proceed"), nil
				}
				session := mutationSession(t, manager, opts)
				result := callTool(t, session, tc.name, tc.args)
				if prompts != 1 {
					t.Fatalf("prompts = %d", prompts)
				}
				assertStructuredOutputMatchesSchema(t, session, tc.name, result)
				if action == "decline" {
					if !result.IsError {
						t.Fatalf("decline = %#v", result)
					}
					assertFile(t, filepath.Join(root, "note"), "old")
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 1 {
						t.Fatalf("declined mutation = %v, %v", entries, err)
					}
					return
				}
				if result.IsError || result.NeedsInput() {
					t.Fatalf("accept = %#v", result)
				}
				switch tc.name {
				case "create_directory":
					info, err := os.Stat(filepath.Join(root, tc.destination))
					if err != nil || !info.IsDir() {
						t.Fatalf("directory = %v, %v", info, err)
					}
				case "create_file", "edit_file":
					assertFile(t, filepath.Join(root, tc.destination), "new text")
				case "move_path":
					assertFile(t, filepath.Join(root, "new"), "old")
				case "trash_path", "permanent_delete_path":
					if _, err := os.Stat(filepath.Join(root, "note")); !os.IsNotExist(err) {
						t.Fatalf("not removed: %v", err)
					}
				}
			})
		}
	}
}

func TestAskReadsDeclinedDoNotDisclose(t *testing.T) {
	for _, name := range []string{"read_text_file", "read_binary_file", "list_directory", "stat_path", "search_paths", "search_text"} {
		t.Run(name, func(t *testing.T) {
			ops := []config.Operation{config.OpRead, config.OpList, config.OpSearch}
			manager := testManager(t, ops, ops)
			writeTestFile(t, filepath.Join(manager.Roots()[0].Path, "secret-name"), []byte("secret-content"))
			session := mutationSession(t, manager, choose("cancel"))
			args := map[string]any{"root": "vault", "path": "secret-name"}
			if name == "list_directory" || strings.HasPrefix(name, "search_") {
				args["path"] = "."
			}
			if strings.HasPrefix(name, "search_") {
				args["query"] = "secret"
			}
			result := callTool(t, session, name, args)
			if !result.IsError || strings.Contains(mustJSON(result), "secret-content") || strings.Contains(mustJSON(result), "secret-name") {
				t.Fatalf("disclosed after cancel = %#v", result)
			}
		})
	}
}

func TestAskMoveDestinationAndDeniedDestination(t *testing.T) {
	for _, allowDestination := range []bool{true, false} {
		t.Run(fmt.Sprint(allowDestination), func(t *testing.T) {
			source, destination := t.TempDir(), t.TempDir()
			destinationAllow := []config.Operation{}
			destinationAsk := []config.Operation{}
			if allowDestination {
				destinationAllow = []config.Operation{config.OpMove}
				destinationAsk = destinationAllow
			}
			manager, err := access.New([]config.DirectoryRule{
				{Name: "source", Path: source, Allow: []config.Operation{config.OpMove}, OnConflict: "ask"},
				{Name: "destination", Path: destination, Allow: destinationAllow, Ask: destinationAsk, OnConflict: "ask"},
			})
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(source, "note"), []byte("old"))
			prompts := 0
			opts := choose("proceed")
			opts.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				prompts++
				return acceptance("proceed"), nil
			}
			result := callTool(t, mutationSession(t, manager, opts), "move_path", map[string]any{"source_root": "source", "source_path": "note", "destination_root": "destination", "destination_path": "note"})
			if allowDestination {
				if result.IsError || prompts != 1 {
					t.Fatalf("destination ask = %#v, %d", result, prompts)
				}
				assertFile(t, filepath.Join(destination, "note"), "old")
			} else {
				if !result.IsError || prompts != 0 {
					t.Fatalf("denied destination = %#v, %d", result, prompts)
				}
				assertFile(t, filepath.Join(source, "note"), "old")
			}
		})
	}
}

func TestMutationEditRequiresExpectedOccurrences(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	manager := testManager(t, []config.Operation{config.OpRead, config.OpEdit}, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("old"))
	session := mutationSession(t, manager, nil)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "edit_file", Arguments: map[string]any{"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("old")), "proposed_content": "new", "edits": []any{map[string]any{"old_text": "old", "new_text": "new"}}}})
	if err == nil && !result.IsError {
		t.Fatalf("missing expected_occurrences accepted = %#v", result)
	}
	assertFile(t, filepath.Join(root, "note"), "old")
}

func TestPermanentIdentityChangeWithSameMetadataRequiresApproval(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpPermanentDelete}, nil)
	root := manager.Roots()[0].Path
	path := filepath.Join(root, "note")
	writeTestFile(t, path, []byte("old"))
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session := mutationSession(t, manager, manualOptions())
	args := map[string]any{"root": "vault", "path": "note"}
	first := callTool(t, session, "permanent_delete_path", args)
	if err := os.Rename(path, filepath.Join(root, "retained")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, []byte("new"))
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	result := retry(t, session, "permanent_delete_path", args, first.RequestState, acceptance("proceed"))
	assertPrompt(t, result, "proceed", "cancel")
	assertFile(t, path, "new")
	assertFile(t, filepath.Join(root, "retained"), "old")
}

func TestPermanentSymlinkChangeRequiresApprovalAndUnlinksOnlyLink(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpPermanentDelete}, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "first"), []byte("first target"))
	writeTestFile(t, filepath.Join(root, "second"), []byte("second target"))
	link := filepath.Join(root, "link")
	if err := os.Symlink("first", link); err != nil {
		t.Fatal(err)
	}
	session := mutationSession(t, manager, manualOptions())
	args := map[string]any{"root": "vault", "path": "link"}
	first := callTool(t, session, "permanent_delete_path", args)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("second", link); err != nil {
		t.Fatal(err)
	}
	second := retry(t, session, "permanent_delete_path", args, first.RequestState, acceptance("proceed"))
	assertPrompt(t, second, "proceed", "cancel")
	result := retry(t, session, "permanent_delete_path", args, second.RequestState, acceptance("proceed"))
	if result.IsError {
		t.Fatalf("delete symlink = %#v", result)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link remains: %v", err)
	}
	assertFile(t, filepath.Join(root, "first"), "first target")
	assertFile(t, filepath.Join(root, "second"), "second target")
}

func TestConflictAutomaticReloadUsesLatestContentAndPreservesProposal(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	manager := testManager(t, []config.Operation{config.OpRead, config.OpEdit}, nil)
	root := manager.Roots()[0].Path
	path := filepath.Join(root, "note")
	writeTestFile(t, path, []byte("background\n"))
	opts := choose("reload_and_rebase")
	opts.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		if err := os.WriteFile(path, []byte("newer background\n"), 0o600); err != nil {
			return nil, err
		}
		return acceptance("reload_and_rebase"), nil
	}
	session := mutationSession(t, manager, opts)
	result := callTool(t, session, "edit_file", map[string]any{"root": "vault", "path": "note", "expected_revision": "stale", "proposed_content": "agent\n", "edits": []any{map[string]any{"old_text": "old", "new_text": "agent", "expected_occurrences": 1}}})
	if result.IsError || result.NeedsInput() || structuredString(t, result, "current_content") != "newer background\n" || structuredString(t, result, "current_revision") != filesystem.Revision([]byte("newer background\n")) || structuredString(t, result, "pending.expected_revision") != "stale" || structuredString(t, result, "pending.proposed_content") != "agent\n" || structuredNumber(t, result, "pending.edits.0.expected_occurrences") != 1 {
		t.Fatalf("reload = %#v", result)
	}
	assertStructuredOutputMatchesSchema(t, session, "edit_file", result)
	assertFile(t, path, "newer background\n")
}

func choose(choice string) *mcp.ClientOptions {
	return &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice": choice}}, nil
	}}
}

func manualOptions() *mcp.ClientOptions {
	opts := choose("proceed")
	opts.MultiRoundTrip = &mcp.MultiRoundTripOptions{Disabled: true}
	return opts
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("file = %q, %v; want %q", data, err, want)
	}
}

func assertPrompt(t *testing.T, result *mcp.CallToolResult, choices ...string) {
	t.Helper()
	if !result.NeedsInput() || len(result.Content) != 0 || result.StructuredContent != nil || len(result.InputRequests) != 1 {
		t.Fatalf("prompt = %#v", result)
	}
	token, err := base64.RawURLEncoding.DecodeString(result.RequestState)
	if err != nil || len(token) != 32 {
		t.Fatalf("token = %q, %v", result.RequestState, err)
	}
	prompt, ok := result.InputRequests["confirm"].(*mcp.ElicitParams)
	if !ok {
		t.Fatalf("request = %#v", result.InputRequests)
	}
	schema := toolSchema(t, prompt.RequestedSchema)
	if schema["type"] != "object" || !sameStringSet(schemaStringSet(t, schema["required"]), []string{"choice"}) {
		t.Fatalf("schema = %#v", schema)
	}
	enumeration := schema["properties"].(map[string]any)["choice"].(map[string]any)["enum"].([]any)
	if len(enumeration) != len(choices) {
		t.Fatalf("choices = %#v", enumeration)
	}
	for i, choice := range choices {
		if enumeration[i] != choice {
			t.Fatalf("choices = %#v", enumeration)
		}
	}
}

func retry(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, token string, response mcp.InputResponse) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args, RequestState: token, InputResponses: mcp.InputResponseMap{"confirm": response}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func acceptance(choice string) *mcp.ElicitResult {
	return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice": choice}}
}

func TestMutationAnnotationsAndSchemas(t *testing.T) {
	session := mutationSession(t, testManager(t, nil, nil), nil)
	for _, tc := range []struct {
		name        string
		destructive bool
		required    []string
	}{
		{"create_directory", false, []string{"root", "path", "parents"}},
		{"create_file", false, []string{"root", "path", "content"}},
		{"edit_file", true, []string{"root", "path", "expected_revision", "proposed_content"}},
		{"move_path", true, []string{"source_root", "source_path", "destination_root", "destination_path"}},
		{"trash_path", true, []string{"root", "path"}},
		{"permanent_delete_path", true, []string{"root", "path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := findTool(t, session, tc.name)
			a := tool.Annotations
			if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint != tc.destructive || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Fatalf("annotations = %#v", a)
			}
			if !strings.Contains(tool.Description, "informational") || !strings.Contains(tool.Description, "ask") {
				t.Fatalf("description = %q", tool.Description)
			}
			assertExactRequired(t, tool, tc.required)
			schema := toolSchema(t, tool.InputSchema)
			properties := schema["properties"].(map[string]any)
			wantCount := len(tc.required)
			if tc.name == "edit_file" {
				wantCount++
			}
			if len(properties) != wantCount {
				t.Fatalf("properties = %#v", properties)
			}
			if tc.name == "edit_file" {
				item := properties["edits"].(map[string]any)["items"].(map[string]any)
				if !sameStringSet(schemaStringSet(t, item["required"]), []string{"old_text", "new_text", "expected_occurrences"}) {
					t.Fatalf("edit schema = %#v", item)
				}
			}
		})
	}
}

func TestMutationDeniedDoesNotTouchDisk(t *testing.T) {
	manager := testManager(t, nil, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("original"))
	session := mutationSession(t, manager, choose("proceed"))
	for name, args := range map[string]map[string]any{
		"create_directory":      {"root": "vault", "path": "new-dir", "parents": true},
		"create_file":           {"root": "vault", "path": "new-file", "content": "new"},
		"edit_file":             {"root": "vault", "path": "note", "expected_revision": filesystem.Revision([]byte("original")), "proposed_content": "changed"},
		"move_path":             {"source_root": "vault", "source_path": "note", "destination_root": "vault", "destination_path": "moved"},
		"trash_path":            {"root": "vault", "path": "note"},
		"permanent_delete_path": {"root": "vault", "path": "note"},
	} {
		result := callTool(t, session, name, args)
		if !result.IsError || structuredString(t, result, "code") != "operation_not_allowed" {
			t.Fatalf("%s = %#v", name, result)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %#v, %v", entries, err)
	}
	assertFile(t, filepath.Join(root, "note"), "original")
}

func TestMutationAllowedMoveWithoutElicitation(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpMove}, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note"), []byte("original"))
	session := mutationSession(t, manager, nil)
	result := callTool(t, session, "move_path", map[string]any{"source_root": "vault", "source_path": "note", "destination_root": "vault", "destination_path": "moved"})
	if result.IsError || result.NeedsInput() {
		t.Fatalf("move = %#v", result)
	}
	assertFile(t, filepath.Join(root, "moved"), "original")
}

func TestAskReadListAndSearchAcceptance(t *testing.T) {
	for _, name := range []string{"read_text_file", "read_binary_file", "list_directory", "stat_path", "search_paths", "search_text"} {
		t.Run(name, func(t *testing.T) {
			ops := []config.Operation{config.OpRead, config.OpList, config.OpSearch}
			manager := testManager(t, ops, ops)
			root := manager.Roots()[0].Path
			writeTestFile(t, filepath.Join(root, "note"), []byte("needle"))
			prompts := 0
			opts := choose("proceed")
			opts.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				prompts++
				return acceptance("proceed"), nil
			}
			session := mutationSession(t, manager, opts)
			args := map[string]any{"root": "vault", "path": "note"}
			if name == "list_directory" || strings.HasPrefix(name, "search_") {
				args["path"] = "."
			}
			if strings.HasPrefix(name, "search_") {
				args["query"] = "needle"
			}
			result := callTool(t, session, name, args)
			if result.IsError || result.NeedsInput() || prompts != 1 {
				t.Fatalf("result = %#v; prompts = %d", result, prompts)
			}
		})
	}
}

func TestAskMutationBeforeWriteAndOneUse(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpCreate}, []config.Operation{config.OpCreate})
	root := manager.Roots()[0].Path
	session := mutationSession(t, manager, manualOptions())
	args := map[string]any{"root": "vault", "path": "note", "content": "new"}
	prompt := callTool(t, session, "create_file", args)
	assertPrompt(t, prompt, "proceed", "cancel")
	if _, err := os.Stat(filepath.Join(root, "note")); !os.IsNotExist(err) {
		t.Fatalf("mutated before approval: %v", err)
	}
	result := retry(t, session, "create_file", args, prompt.RequestState, acceptance("proceed"))
	if result.IsError {
		t.Fatalf("result = %#v", result)
	}
	assertFile(t, filepath.Join(root, "note"), "new")
	result = retry(t, session, "create_file", args, prompt.RequestState, acceptance("proceed"))
	if !result.IsError || structuredString(t, result, "code") != "invalid_confirmation" {
		t.Fatalf("reused = %#v", result)
	}
}

func TestPermanentDeleteConfirmations(t *testing.T) {
	for _, choice := range []string{"proceed", "cancel", "decline", "unavailable"} {
		t.Run(choice, func(t *testing.T) {
			manager := testManager(t, []config.Operation{config.OpPermanentDelete}, nil)
			root := manager.Roots()[0].Path
			writeTestFile(t, filepath.Join(root, "note"), []byte("original"))
			opts := choose(choice)
			if choice == "unavailable" {
				opts = nil
			}
			if choice == "decline" {
				opts = &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					return &mcp.ElicitResult{Action: "decline"}, nil
				}}
			}
			result := callTool(t, mutationSession(t, manager, opts), "permanent_delete_path", map[string]any{"root": "vault", "path": "note"})
			if choice == "proceed" {
				if result.IsError {
					t.Fatalf("delete = %#v", result)
				}
				if _, err := os.Stat(filepath.Join(root, "note")); !os.IsNotExist(err) {
					t.Fatalf("not deleted: %v", err)
				}
			} else {
				if !result.IsError {
					t.Fatalf("unsafe completion = %#v", result)
				}
				assertFile(t, filepath.Join(root, "note"), "original")
			}
		})
	}
}

func TestPermanentChangedFingerprintRequiresFreshApproval(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpPermanentDelete}, nil)
	root := manager.Roots()[0].Path
	path := filepath.Join(root, "note")
	writeTestFile(t, path, []byte("original"))
	session := mutationSession(t, manager, manualOptions())
	args := map[string]any{"root": "vault", "path": "note"}
	first := callTool(t, session, "permanent_delete_path", args)
	assertPrompt(t, first, "proceed", "cancel")
	writeTestFile(t, path, []byte("external change"))
	second := retry(t, session, "permanent_delete_path", args, first.RequestState, acceptance("proceed"))
	assertPrompt(t, second, "proceed", "cancel")
	assertFile(t, path, "external change")
	if first.RequestState == second.RequestState {
		t.Fatal("reused token")
	}
	result := retry(t, session, "permanent_delete_path", args, second.RequestState, acceptance("cancel"))
	if !result.IsError {
		t.Fatalf("cancel = %#v", result)
	}
	assertFile(t, path, "external change")
}

func TestConflictChoicesAndRepeatedExternalEdit(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	for _, choice := range []string{"reload_and_rebase", "overwrite", "cancel"} {
		t.Run(choice, func(t *testing.T) {
			manager := testManager(t, []config.Operation{config.OpRead, config.OpEdit}, nil)
			root := manager.Roots()[0].Path
			path := filepath.Join(root, "note")
			writeTestFile(t, path, []byte("background\n"))
			session := mutationSession(t, manager, manualOptions())
			args := map[string]any{"root": "vault", "path": "note", "expected_revision": "stale", "proposed_content": "agent\n", "edits": []any{map[string]any{"old_text": "old", "new_text": "agent", "expected_occurrences": 1}}}
			first := callTool(t, session, "edit_file", args)
			assertPrompt(t, first, "reload_and_rebase", "overwrite", "cancel")
			if choice == "overwrite" {
				writeTestFile(t, path, []byte("newer\n"))
			}
			result := retry(t, session, "edit_file", args, first.RequestState, acceptance(choice))
			if choice == "overwrite" {
				assertPrompt(t, result, "reload_and_rebase", "overwrite", "cancel")
				assertFile(t, path, "newer\n")
				result = retry(t, session, "edit_file", args, result.RequestState, acceptance("overwrite"))
				if result.IsError || result.NeedsInput() {
					t.Fatalf("overwrite = %#v", result)
				}
				assertFile(t, path, "agent\n")
			} else {
				if result.NeedsInput() || structuredString(t, result, "pending.proposed_content") != "agent\n" || structuredString(t, result, "pending.edits.0.old_text") != "old" || structuredString(t, result, "current_content") != "background\n" {
					t.Fatalf("conflict = %#v", result)
				}
				assertFile(t, path, "background\n")
			}
		})
	}
}
