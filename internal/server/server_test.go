package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
)

// TestEndToEndScenario catches lost background edits or pending proposals,
// broken cross-root mutations/native content, and bypassed confirmation or
// path authorization across the real SDK boundary. All disk operations are
// real; only the Trash destination is redirected to an isolated test directory.
func TestEndToEndScenario(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	vault, archive, trash := t.TempDir(), t.TempDir(), t.TempDir()
	archiveLink := filepath.Join(t.TempDir(), "archive-link")
	if err := os.Symlink(archive, archiveLink); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeTestFile(t, configPath, []byte(fmt.Sprintf(`version: 1
directories:
  - name: vault
    path: %q
    allow: [list, search, read, create, edit, move, trash, permanent_delete]
    ask: []
    on_conflict: ask
  - name: archive
    path: %q
    allow: [list, search, read, create, edit, move, trash, permanent_delete]
    ask: []
    on_conflict: ask
`, vault, archiveLink)))
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := access.New(cfg.Directories)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	srv := New(filesystem.New(manager, filesystem.Options{TrashDir: trash}), manager)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	// Disable automatic replies so the test can inspect the real MRTR prompt
	// and disk state before explicitly supplying each user's decision.
	session, err := mcp.NewClient(&mcp.Implementation{Name: "scenario", Version: "1"}, manualOptions()).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	success := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result := callTool(t, session, name, args)
		if result.IsError || result.NeedsInput() {
			t.Fatalf("%s unexpectedly failed or prompted: %s", name, mustJSON(result))
		}
		assertStructuredOutputMatchesSchema(t, session, name, result)
		return result
	}
	decode := func(result *mcp.CallToolResult, output any) {
		t.Helper()
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
	absent := func(path string) {
		t.Helper()
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be absent, got %v", path, err)
		}
	}

	t.Log("list both configured roots, retaining the user-facing symlink path")
	var roots ListRootsOutput
	decode(success("list_roots", map[string]any{}), &roots)
	if len(roots.Roots) != 2 || roots.Roots[0].Name != "vault" || roots.Roots[0].Path != vault || roots.Roots[1].Name != "archive" || roots.Roots[1].Path != archiveLink {
		t.Fatalf("roots = %#v", roots.Roots)
	}
	for _, root := range roots.Roots {
		if !sameStringSet(root.Allow, []string{"list", "search", "read", "create", "edit", "move", "trash", "permanent_delete"}) {
			t.Fatalf("allow = %#v", root)
		}
	}

	t.Log("create a directory and note, read the revision, and edit it")
	success("create_directory", map[string]any{"root": "vault", "path": "draft", "parents": false})
	const initial = "title: draft\nstatus: todo\n"
	created := success("create_file", map[string]any{"root": "vault", "path": "draft/note.md", "content": initial})
	notePath := filepath.Join(vault, "draft", "note.md")
	assertFile(t, notePath, initial)
	read := success("read_text_file", map[string]any{"root": "vault", "path": "draft/note.md"})
	firstRevision := structuredString(t, read, "revision")
	if structuredString(t, read, "content") != initial || firstRevision == "" || structuredString(t, created, "revision") != firstRevision {
		t.Fatalf("initial read = %s", mustJSON(read))
	}
	const edited = "title: reviewed\nstatus: todo\n"
	edit := success("edit_file", map[string]any{"root": "vault", "path": "draft/note.md", "expected_revision": firstRevision, "proposed_content": edited,
		"edits": []TextEditInput{{OldText: "title: draft", NewText: "title: reviewed", ExpectedOccurrences: 1}}})
	editedRevision := structuredString(t, edit, "revision")
	if editedRevision == "" || editedRevision == firstRevision {
		t.Fatalf("edit revision = %q", editedRevision)
	}
	assertFile(t, notePath, edited)

	t.Log("inject a background edit, reload/rebase, and retain the complete proposal")
	const background = "title: reviewed\nstatus: todo\nexternal: keep me\n"
	writeTestFile(t, notePath, []byte(background))
	const proposal = "title: final\nstatus: done\n"
	pendingEdits := []TextEditInput{
		{OldText: "title: reviewed", NewText: "title: final", ExpectedOccurrences: 1},
		{OldText: "status: todo", NewText: "status: done", ExpectedOccurrences: 1},
	}
	args := map[string]any{"root": "vault", "path": "draft/note.md", "expected_revision": editedRevision, "proposed_content": proposal, "edits": pendingEdits}
	prompt := callTool(t, session, "edit_file", args)
	assertPrompt(t, prompt, "reload_and_rebase", "overwrite", "cancel")
	assertFile(t, notePath, background)
	reloaded := retry(t, session, "edit_file", args, prompt.RequestState, acceptance("reload_and_rebase"))
	if reloaded.IsError || reloaded.NeedsInput() {
		t.Fatalf("reload = %s", mustJSON(reloaded))
	}
	assertStructuredOutputMatchesSchema(t, session, "edit_file", reloaded)
	var conflict ConflictOutput
	decode(reloaded, &conflict)
	wantPending := PendingEditOutput{ExpectedRevision: editedRevision, ProposedContent: proposal, Edits: pendingEdits}
	if conflict.Status != "reload_and_rebase" || conflict.CurrentContent != background || conflict.CurrentRevision == "" || conflict.CurrentRevision == editedRevision || !reflect.DeepEqual(conflict.Pending, wantPending) || conflict.RecoveryPath != "" {
		t.Fatalf("conflict = %#v; want complete pending %#v", conflict, wantPending)
	}
	assertFile(t, notePath, background)
	latest := success("read_text_file", map[string]any{"root": "vault", "path": "draft/note.md"})
	if structuredString(t, latest, "revision") != conflict.CurrentRevision {
		t.Fatal("reload revision does not match the live background file")
	}
	const rebased = "title: final\nstatus: done\nexternal: keep me\n"
	args["expected_revision"] = conflict.CurrentRevision
	args["proposed_content"] = rebased
	retried := success("edit_file", args)
	assertFile(t, notePath, rebased)
	if structuredString(t, retried, "revision") == conflict.CurrentRevision {
		t.Fatal("retry did not advance the revision")
	}

	t.Log("move the non-empty directory across roots without elicitation")
	success("move_path", map[string]any{"source_root": "vault", "source_path": "draft", "destination_root": "archive", "destination_path": "finished"})
	absent(filepath.Join(vault, "draft"))
	assertFile(t, filepath.Join(archive, "finished", "note.md"), rebased)
	moved := success("read_text_file", map[string]any{"root": "archive", "path": "finished/note.md"})
	if structuredString(t, moved, "content") != rebased || structuredString(t, moved, "revision") != structuredString(t, retried, "revision") || structuredString(t, moved, "root") != "archive" || structuredString(t, moved, "path") != "finished/note.md" {
		t.Fatalf("moved note = %s", mustJSON(moved))
	}

	t.Log("read actual PNG/PDF attachments as native MCP content")
	var pngBuffer bytes.Buffer
	if err := png.Encode(&pngBuffer, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// A complete one-page blank PDF with a real xref table.
	var pdfBuffer bytes.Buffer
	pdfBuffer.WriteString("%PDF-1.4\n")
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 72 72] >>"}
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = pdfBuffer.Len()
		fmt.Fprintf(&pdfBuffer, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdfBuffer.Len()
	fmt.Fprintf(&pdfBuffer, "xref\n0 4\n0000000000 65535 f \n")
	for _, offset := range offsets {
		fmt.Fprintf(&pdfBuffer, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdfBuffer, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	for _, attachment := range []struct {
		name, mime, kind string
		data             []byte
	}{
		{"pixel.png", "image/png", "image", pngBuffer.Bytes()},
		{"blank.pdf", "application/pdf", "resource", pdfBuffer.Bytes()},
	} {
		path := "finished/" + attachment.name
		uri := "scopedfs://archive/finished%2F" + attachment.name
		writeTestFile(t, filepath.Join(archive, filepath.FromSlash(path)), attachment.data)
		result := success("read_binary_file", map[string]any{"root": "archive", "path": path})
		if len(result.Content) != 1 || structuredString(t, result, "root") != "archive" || structuredString(t, result, "path") != path || structuredString(t, result, "mime_type") != attachment.mime || structuredString(t, result, "kind") != attachment.kind || structuredString(t, result, "uri") != uri || structuredNumber(t, result, "size") != float64(len(attachment.data)) || structuredString(t, result, "revision") == "" {
			t.Fatalf("binary result = %s", mustJSON(result))
		}
		switch content := result.Content[0].(type) {
		case *mcp.ImageContent:
			if attachment.kind != "image" || content.MIMEType != attachment.mime || !bytes.Equal(content.Data, attachment.data) {
				t.Fatalf("PNG content = %#v", content)
			}
		case *mcp.EmbeddedResource:
			if attachment.kind != "resource" || content.Resource.MIMEType != attachment.mime || content.Resource.URI != uri || !bytes.Equal(content.Resource.Blob, attachment.data) {
				t.Fatalf("PDF content = %#v", content)
			}
		default:
			t.Fatalf("unexpected native content: %T", content)
		}
	}

	t.Log("Trash the non-empty directory and verify every byte remains recoverable")
	success("trash_path", map[string]any{"root": "archive", "path": "finished"})
	absent(filepath.Join(archive, "finished"))
	assertFile(t, filepath.Join(trash, "finished", "note.md"), rebased)
	assertFile(t, filepath.Join(trash, "finished", "pixel.png"), pngBuffer.String())
	assertFile(t, filepath.Join(trash, "finished", "blank.pdf"), pdfBuffer.String())

	t.Log("decline deletion, recreate a disposable target, then explicitly accept deletion")
	disposable := filepath.Join(archive, "disposable.txt")
	success("create_file", map[string]any{"root": "archive", "path": "disposable.txt", "content": "first disposable"})
	deleteArgs := map[string]any{"root": "archive", "path": "disposable.txt"}
	deletePrompt := callTool(t, session, "permanent_delete_path", deleteArgs)
	assertPrompt(t, deletePrompt, "proceed", "cancel")
	assertFile(t, disposable, "first disposable")
	declined := retry(t, session, "permanent_delete_path", deleteArgs, deletePrompt.RequestState, &mcp.ElicitResult{Action: "decline"})
	if !declined.IsError || declined.NeedsInput() || structuredString(t, declined, "code") != "confirmation_declined" {
		t.Fatalf("declined = %s", mustJSON(declined))
	}
	assertStructuredOutputMatchesSchema(t, session, "permanent_delete_path", declined)
	assertFile(t, disposable, "first disposable")
	success("trash_path", deleteArgs)
	absent(disposable)
	assertFile(t, filepath.Join(trash, "disposable.txt"), "first disposable")
	success("create_file", map[string]any{"root": "archive", "path": "disposable.txt", "content": "recreated disposable"})
	deletePrompt = callTool(t, session, "permanent_delete_path", deleteArgs)
	assertPrompt(t, deletePrompt, "proceed", "cancel")
	assertFile(t, disposable, "recreated disposable")
	deleted := retry(t, session, "permanent_delete_path", deleteArgs, deletePrompt.RequestState, acceptance("proceed"))
	if deleted.IsError || deleted.NeedsInput() {
		t.Fatalf("accepted deletion = %s", mustJSON(deleted))
	}
	assertStructuredOutputMatchesSchema(t, session, "permanent_delete_path", deleted)
	absent(disposable)
	assertFile(t, filepath.Join(trash, "disposable.txt"), "first disposable")

	t.Log("reject traversal, absolute paths, and an escaping symlink without disclosure or mutation")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeTestFile(t, outside, []byte("outside secret"))
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(vault, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, rejected := range []struct{ path, code string }{
		{"../outside.txt", "path_outside_root"},
		{outside, "path_outside_root"},
		{"escape/outside.txt", "symlink_escape"},
	} {
		for _, name := range []string{"read_text_file", "create_file"} {
			args := map[string]any{"root": "vault", "path": rejected.path}
			if name == "create_file" {
				args["content"] = "must not overwrite"
			}
			result := callTool(t, session, name, args)
			if !result.IsError || result.NeedsInput() || structuredString(t, result, "code") != rejected.code || strings.Contains(mustJSON(result), "outside secret") {
				t.Fatalf("%s %q = %s", name, rejected.path, mustJSON(result))
			}
			assertStructuredOutputMatchesSchema(t, session, name, result)
			assertFile(t, outside, "outside secret")
		}
	}
	invalid, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_file", Arguments: map[string]any{"root": "vault", "path": "invalid.md"}})
	if err == nil && !invalid.IsError {
		t.Fatalf("missing required content accepted: %s", mustJSON(invalid))
	}
	absent(filepath.Join(vault, "invalid.md"))
}

type toolContract struct {
	description string
	required    []string
	parameters  map[string]string
	output      []string
}

var pathDescriptions = map[string]string{
	"root": "configured root name returned by list_roots",
	"path": "relative path within root; absolute paths and .. traversal are rejected",
}

var searchDescriptions = map[string]string{
	"root":           "configured root name returned by list_roots",
	"path":           "relative path within root; absolute paths and .. traversal are rejected",
	"query":          "text to match in paths or text-file lines",
	"case_sensitive": "match case exactly when true; false uses case-insensitive matching",
	"max_results":    "maximum number of results; default 100 and values above 1000 are capped at 1000",
}

func TestReadOnlyToolAnnotationsAndSchemas(t *testing.T) {
	session := testSession(t, testManager(t, []config.Operation{config.OpList, config.OpSearch, config.OpRead}, nil))

	want := map[string]toolContract{
		"list_roots":       {description: "List configured filesystem roots and their allowed operations.", required: nil, parameters: map[string]string{}, output: []string{"roots"}},
		"list_directory":   {description: "List immediate entries in one configured directory.", required: []string{"root", "path"}, parameters: pathDescriptions, output: []string{"entries"}},
		"stat_path":        {description: "Report metadata for one path in a configured root.", required: []string{"root", "path"}, parameters: pathDescriptions, output: []string{"entry"}},
		"search_paths":     {description: "Search relative paths below a configured directory; results are capped at 1000.", required: []string{"root", "path", "query"}, parameters: searchDescriptions, output: []string{"matches", "max_results"}},
		"search_text":      {description: "Search UTF-8 text-file lines below a configured directory; results are capped at 1000.", required: []string{"root", "path", "query"}, parameters: searchDescriptions, output: []string{"matches", "max_results"}},
		"read_text_file":   {description: "Read one UTF-8 text file and its revision.", required: []string{"root", "path"}, parameters: pathDescriptions, output: []string{"root", "path", "content", "revision", "size"}},
		"read_binary_file": {description: "Read one binary file as native image, audio, or embedded resource content together with metadata.", required: []string{"root", "path"}, parameters: pathDescriptions, output: []string{"root", "path", "mime_type", "size", "revision", "uri", "kind"}},
	}
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != len(want)+6 {
		t.Fatalf("tool count = %d, want 13: %#v", len(tools.Tools), tools.Tools)
	}
	for _, listedTool := range tools.Tools {
		contract, ok := want[listedTool.Name]
		if !ok {
			if listedTool.Annotations != nil && !listedTool.Annotations.ReadOnlyHint {
				continue
			}
			t.Fatalf("unexpected read tool %q", listedTool.Name)
		}
		delete(want, listedTool.Name)
		name := listedTool.Name
		if listedTool.Description != contract.description {
			t.Fatalf("%s description = %q, want %q", name, listedTool.Description, contract.description)
		}
		tool := findTool(t, session, name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("%s annotations = %#v, want read-only and closed-world", name, tool.Annotations)
		}
		assertExactRequired(t, tool, contract.required)
		assertExactDescriptions(t, tool, contract.parameters)
		assertSuccessOrErrorOutputSchema(t, tool, contract.output)
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %#v", want)
	}
}

func TestListAndSearchToolsReturnStructuredResults(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpList, config.OpSearch, config.OpRead}, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "notes", "alpha.md"), []byte("first needle\nsecond\n"))
	session := testSession(t, manager)
	roots := callTool(t, session, "list_roots", map[string]any{})
	if roots.IsError || structuredString(t, roots, "roots.0.name") != "vault" || structuredString(t, roots, "roots.0.path") != root {
		t.Fatalf("list_roots result = %#v", roots)
	}

	list := callTool(t, session, "list_directory", map[string]any{"root": "vault", "path": "notes"})
	if list.IsError || structuredString(t, list, "entries.0.path") != "notes/alpha.md" {
		t.Fatalf("list_directory result = %#v", list)
	}

	stat := callTool(t, session, "stat_path", map[string]any{"root": "vault", "path": "notes/alpha.md"})
	if stat.IsError || structuredString(t, stat, "entry.type") != "file" {
		t.Fatalf("stat_path result = %#v", stat)
	}

	paths := callTool(t, session, "search_paths", map[string]any{"root": "vault", "path": ".", "query": "ALPHA", "max_results": 1001})
	if paths.IsError || structuredString(t, paths, "matches.0.path") != "notes/alpha.md" || structuredNumber(t, paths, "max_results") != 1000 {
		t.Fatalf("search_paths result = %#v", paths)
	}

	text := callTool(t, session, "search_text", map[string]any{"root": "vault", "path": ".", "query": "needle"})
	if text.IsError || structuredString(t, text, "matches.0.text") != "first needle" || structuredNumber(t, text, "matches.0.line") != 1 {
		t.Fatalf("search_text result = %#v", text)
	}
}

func TestReadTextToolReturnsContentAndRevision(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpRead}, nil)
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note.md"), []byte("hello\n"))
	session := testSession(t, manager)

	result := callTool(t, session, "read_text_file", map[string]any{"root": "vault", "path": "note.md"})
	if result.IsError || structuredString(t, result, "content") != "hello\n" || structuredString(t, result, "revision") == "" {
		t.Fatalf("read_text_file result = %#v", result)
	}
}

func TestReadBinaryToolUsesNativeContentBlocks(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpRead}, nil)
	root := manager.Roots()[0].Path
	pngData := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	wavData := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 32)...)
	writeTestFile(t, filepath.Join(root, "pixel.png"), pngData)
	writeTestFile(t, filepath.Join(root, "tone.wav"), wavData)
	writeTestFile(t, filepath.Join(root, "guide.pdf"), []byte("%PDF-1.4\n"))
	session := testSession(t, manager)

	image := callTool(t, session, "read_binary_file", map[string]any{"root": "vault", "path": "pixel.png"})
	if image.IsError || len(image.Content) != 1 {
		t.Fatalf("PNG result = %#v", image)
	}
	imageContent, ok := image.Content[0].(*mcp.ImageContent)
	if !ok || imageContent.MIMEType != "image/png" || !bytes.Equal(imageContent.Data, pngData) {
		t.Fatalf("PNG content = %T, want *mcp.ImageContent", image.Content[0])
	}
	if structuredString(t, image, "root") != "vault" || structuredString(t, image, "path") != "pixel.png" || structuredString(t, image, "mime_type") != "image/png" || structuredNumber(t, image, "size") != float64(len(pngData)) || structuredString(t, image, "kind") != "image" {
		t.Fatalf("PNG metadata = %#v", image.StructuredContent)
	}

	audio := callTool(t, session, "read_binary_file", map[string]any{"root": "vault", "path": "tone.wav"})
	if audio.IsError || len(audio.Content) != 1 {
		t.Fatalf("WAV result = %#v", audio)
	}
	audioContent, ok := audio.Content[0].(*mcp.AudioContent)
	if !ok || audioContent.MIMEType != "audio/wave" || !bytes.Equal(audioContent.Data, wavData) {
		t.Fatalf("WAV content = %T, want *mcp.AudioContent", audio.Content[0])
	}
	if structuredString(t, audio, "root") != "vault" || structuredString(t, audio, "path") != "tone.wav" || structuredString(t, audio, "mime_type") != "audio/wave" || structuredNumber(t, audio, "size") != float64(len(wavData)) || structuredString(t, audio, "kind") != "audio" {
		t.Fatalf("WAV metadata = %#v", audio.StructuredContent)
	}

	pdf := callTool(t, session, "read_binary_file", map[string]any{"root": "vault", "path": "guide.pdf"})
	if pdf.IsError || len(pdf.Content) != 1 {
		t.Fatalf("PDF result = %#v", pdf)
	}
	resource, ok := pdf.Content[0].(*mcp.EmbeddedResource)
	if !ok || resource.Resource.MIMEType != "application/pdf" || resource.Resource.URI != "scopedfs://vault/guide.pdf" {
		t.Fatalf("PDF content = %#v, want embedded scoped PDF resource", pdf.Content[0])
	}
	if structuredString(t, pdf, "mime_type") != "application/pdf" || structuredNumber(t, pdf, "size") != 9 || structuredString(t, pdf, "revision") == "" {
		t.Fatalf("PDF metadata = %#v", pdf.StructuredContent)
	}
}

func TestDomainErrorsAreStructured(t *testing.T) {
	session := testSession(t, testManager(t, []config.Operation{config.OpRead}, nil))
	result := callTool(t, session, "read_text_file", map[string]any{"root": "vault", "path": "missing.md"})
	if !result.IsError || structuredString(t, result, "code") != "path_not_found" || structuredString(t, result, "root") != "vault" || structuredString(t, result, "path") != "missing.md" || structuredString(t, result, "operation") != "read" {
		t.Fatalf("domain error = %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != structuredString(t, result, "message") {
		t.Fatalf("domain error content = %T, want *mcp.TextContent", result.Content[0])
	}
	assertStructuredOutputMatchesSchema(t, session, "read_text_file", result)
}

func TestAskedReadOperationFailsClosedWithoutElicitation(t *testing.T) {
	manager := testManager(t, []config.Operation{config.OpRead}, []config.Operation{config.OpRead})
	root := manager.Roots()[0].Path
	writeTestFile(t, filepath.Join(root, "note.md"), []byte("must not be exposed\n"))
	session := testSession(t, manager)

	result := callTool(t, session, "read_text_file", map[string]any{"root": "vault", "path": "note.md"})
	if !result.IsError || structuredString(t, result, "code") != "elicitation_unavailable" {
		t.Fatalf("asked read result = %#v, want elicitation_unavailable", result)
	}
}

func TestAskedListAndSearchOperationsFailClosedWithoutElicitation(t *testing.T) {
	listManager := testManager(t, []config.Operation{config.OpList}, []config.Operation{config.OpList})
	listRoot := listManager.Roots()[0].Path
	writeTestFile(t, filepath.Join(listRoot, "note.md"), []byte("must not be listed\n"))
	listSession := testSession(t, listManager)
	list := callTool(t, listSession, "list_directory", map[string]any{"root": "vault", "path": "."})
	if !list.IsError || structuredString(t, list, "code") != "elicitation_unavailable" || structuredString(t, list, "operation") != "list" {
		t.Fatalf("asked list result = %#v, want elicitation_unavailable", list)
	}
	stat := callTool(t, listSession, "stat_path", map[string]any{"root": "vault", "path": "note.md"})
	if !stat.IsError || structuredString(t, stat, "code") != "elicitation_unavailable" || structuredString(t, stat, "operation") != "list" {
		t.Fatalf("asked stat result = %#v, want elicitation_unavailable", stat)
	}

	searchManager := testManager(t, []config.Operation{config.OpSearch, config.OpRead}, []config.Operation{config.OpSearch})
	searchRoot := searchManager.Roots()[0].Path
	writeTestFile(t, filepath.Join(searchRoot, "note.md"), []byte("must not be searched\n"))
	searchSession := testSession(t, searchManager)
	search := callTool(t, searchSession, "search_paths", map[string]any{"root": "vault", "path": ".", "query": "note"})
	if !search.IsError || structuredString(t, search, "code") != "elicitation_unavailable" || structuredString(t, search, "operation") != "search" {
		t.Fatalf("asked search result = %#v, want elicitation_unavailable", search)
	}
	readAskedManager := testManager(t, []config.Operation{config.OpSearch, config.OpRead}, []config.Operation{config.OpRead})
	readAskedRoot := readAskedManager.Roots()[0].Path
	writeTestFile(t, filepath.Join(readAskedRoot, "note.md"), []byte("must not be searched or read\n"))
	readAskedSession := testSession(t, readAskedManager)
	searchText := callTool(t, readAskedSession, "search_text", map[string]any{"root": "vault", "path": ".", "query": "searched"})
	if !searchText.IsError || structuredString(t, searchText, "code") != "elicitation_unavailable" || structuredString(t, searchText, "operation") != "read" {
		t.Fatalf("asked text search result = %#v, want elicitation_unavailable", searchText)
	}
	binary := callTool(t, readAskedSession, "read_binary_file", map[string]any{"root": "vault", "path": "note.md"})
	if !binary.IsError || structuredString(t, binary, "code") != "elicitation_unavailable" || structuredString(t, binary, "operation") != "read" {
		t.Fatalf("asked binary read result = %#v, want elicitation_unavailable", binary)
	}
}

func testSession(t *testing.T, manager *access.Manager) *mcp.ClientSession {
	t.Helper()
	t.Cleanup(func() { _ = manager.Close() })
	server := New(filesystem.New(manager, filesystem.Options{}), manager)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func testManager(t *testing.T, allow, ask []config.Operation) *access.Manager {
	t.Helper()
	root := t.TempDir()
	manager, err := access.New([]config.DirectoryRule{{Name: "vault", Path: root, Allow: allow, Ask: ask, OnConflict: "ask"}})
	if err != nil {
		t.Fatalf("new access manager: %v", err)
	}
	return manager
}

func writeTestFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make parent directory: %v", err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func findTool(t *testing.T, session *mcp.ClientSession, name string) *mcp.Tool {
	t.Helper()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func assertExactRequired(t *testing.T, tool *mcp.Tool, want []string) {
	t.Helper()
	schema := toolSchema(t, tool.InputSchema)
	if got := schemaStringSet(t, schema["required"]); !sameStringSet(got, want) {
		t.Fatalf("%s required fields = %#v, want %#v", tool.Name, got, want)
	}
}

func assertExactDescriptions(t *testing.T, tool *mcp.Tool, want map[string]string) {
	t.Helper()
	schema := toolSchema(t, tool.InputSchema)
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) != len(want) {
		t.Fatalf("%s parameter fields = %#v, want %#v", tool.Name, properties, want)
	}
	for field, description := range want {
		property, _ := properties[field].(map[string]any)
		if property["description"] != description {
			t.Fatalf("%s.%s description = %#v, want %q", tool.Name, field, property["description"], description)
		}
	}
}

func assertSuccessOrErrorOutputSchema(t *testing.T, tool *mcp.Tool, successFields []string) {
	t.Helper()
	schema := toolSchema(t, tool.OutputSchema)
	oneOf, _ := schema["oneOf"].([]any)
	if len(oneOf) != 2 {
		t.Fatalf("%s output schema oneOf = %#v, want success and ToolError branches", tool.Name, oneOf)
	}
	var success, toolError map[string]any
	for _, branch := range oneOf {
		candidate, _ := branch.(map[string]any)
		properties, _ := candidate["properties"].(map[string]any)
		if _, isError := properties["code"]; isError {
			toolError = candidate
		} else {
			success = candidate
		}
	}
	if success == nil || toolError == nil {
		t.Fatalf("%s output schema branches = %#v, want success and ToolError", tool.Name, oneOf)
	}
	if got := schemaStringSet(t, success["required"]); !sameStringSet(got, successFields) {
		t.Fatalf("%s success required fields = %#v, want %#v", tool.Name, got, successFields)
	}
	if got := schemaPropertySet(success); !sameStringSet(got, successFields) {
		t.Fatalf("%s success output fields = %#v, want %#v", tool.Name, got, successFields)
	}
	if got := schemaStringSet(t, toolError["required"]); !sameStringSet(got, []string{"code", "message"}) {
		t.Fatalf("%s ToolError required fields = %#v, want code/message", tool.Name, got)
	}
	if got := schemaPropertySet(toolError); !sameStringSet(got, []string{"code", "root", "path", "operation", "message", "detail", "pending", "recovery", "recovery_paths", "recovery_paths_omitted"}) {
		t.Fatalf("%s ToolError fields = %#v", tool.Name, got)
	}
}

func assertStructuredOutputMatchesSchema(t *testing.T, session *mcp.ClientSession, name string, result *mcp.CallToolResult) {
	t.Helper()
	tool := findTool(t, session, name)
	encodedSchema, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal %s output schema: %v", name, err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encodedSchema, &schema); err != nil {
		t.Fatalf("decode %s output schema: %v", name, err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve %s output schema: %v", name, err)
	}
	encodedOutput, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal %s structured output: %v", name, err)
	}
	var output any
	if err := json.Unmarshal(encodedOutput, &output); err != nil {
		t.Fatalf("decode %s structured output: %v", name, err)
	}
	if err := resolved.Validate(output); err != nil {
		t.Fatalf("%s structured output violates advertised schema: %v; output=%#v", name, err, result.StructuredContent)
	}
}

func schemaStringSet(t *testing.T, raw any) []string {
	t.Helper()
	values, _ := raw.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		stringValue, ok := value.(string)
		if !ok {
			t.Fatalf("schema string set = %#v", raw)
		}
		result = append(result, stringValue)
	}
	return result
}

func schemaPropertySet(schema map[string]any) []string {
	properties, _ := schema["properties"].(map[string]any)
	result := make([]string, 0, len(properties))
	for property := range properties {
		result = append(result, property)
	}
	return result
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, value := range got {
		seen[value] = true
	}
	if len(seen) != len(got) {
		return false
	}
	for _, value := range want {
		if !seen[value] {
			return false
		}
	}
	return true
}

func toolSchema(t *testing.T, raw any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	return schema
}

func structuredString(t *testing.T, result *mcp.CallToolResult, path string) string {
	t.Helper()
	value := structuredValue(t, result, path)
	stringValue, _ := value.(string)
	return stringValue
}

func structuredNumber(t *testing.T, result *mcp.CallToolResult, path string) float64 {
	t.Helper()
	value := structuredValue(t, result, path)
	number, _ := value.(float64)
	return number
}

func structuredBool(t *testing.T, result *mcp.CallToolResult, path string) bool {
	t.Helper()
	value := structuredValue(t, result, path)
	boolean, _ := value.(bool)
	return boolean
}

func structuredValue(t *testing.T, result *mcp.CallToolResult, path string) any {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	// Paths use dots and optional numeric indexes, for example entries.0.path.
	for _, part := range strings.Split(path, ".") {
		switch current := value.(type) {
		case map[string]any:
			value = current[part]
		case []any:
			if part != "0" || len(current) == 0 {
				return nil
			}
			value = current[0]
		default:
			return nil
		}
	}
	return value
}
