package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

func TestListDirectoryIsSortedAndReportsTypes(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "b.txt"), []byte("hello"))
	if err := os.Mkdir(filepath.Join(root, "a-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(root, "a-dir", "nested.txt"), []byte("not listed"))
	mustSymlink(t, "b.txt", filepath.Join(root, "c-link"))
	s := queryService(t, root)
	got, err := s.ListDirectory(context.Background(), "notes", ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Path != "a-dir" || got[1].Path != "b.txt" || got[2].Path != "c-link" {
		t.Fatalf("entries = %#v", got)
	}
	if got[0].Type != "directory" || got[1].Type != "file" || got[2].Type != "symlink" || got[1].Size != 5 {
		t.Fatalf("entries = %#v", got)
	}
}

func TestStatReportsRevisionForRegularFile(t *testing.T) {
	root := t.TempDir()
	data := []byte("hello")
	writeBytes(t, filepath.Join(root, "note.md"), data)
	got, err := queryService(t, root).Stat(context.Background(), "notes", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "file" || got.Size != int64(len(data)) || got.Revision != Revision(data) {
		t.Fatalf("stat = %#v", got)
	}
}

func TestListDirectoryReportsUnresolvableSymlinksWithoutFollowingThem(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeBytes(t, filepath.Join(root, "note.txt"), []byte("hello"))
	writeBytes(t, filepath.Join(outside, "secret"), []byte("outside"))
	mustSymlink(t, "missing", filepath.Join(root, "broken"))
	mustSymlink(t, "loop", filepath.Join(root, "loop"))
	mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(root, "outside"))
	s := queryService(t, root)
	entries, err := s.ListDirectory(context.Background(), "notes", ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ path, kind string }{{"broken", "symlink"}, {"loop", "symlink"}, {"note.txt", "file"}, {"outside", "symlink"}}
	if len(entries) != len(want) {
		t.Fatalf("entries = %#v", entries)
	}
	for i, expected := range want {
		if entries[i].Path != expected.path || entries[i].Type != expected.kind {
			t.Fatalf("entry %d = %#v", i, entries[i])
		}
		if expected.kind == "symlink" && entries[i].Revision != "" {
			t.Fatalf("symlink target was hashed: %#v", entries[i])
		}
	}
	_, err = s.ReadText(context.Background(), "notes", "outside")
	assertFSCode(t, err, "symlink_escape")
}

func TestSearchPathsUsesLiteralCaseAndLimit(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "Alpha.txt"), []byte("x"))
	writeBytes(t, filepath.Join(root, "alpha.md"), []byte("x"))
	s := queryService(t, root)
	got, err := s.SearchPaths(context.Background(), "notes", ".", ".TXT", SearchOptions{CaseSensitive: false, MaxResults: 1})
	if err != nil || len(got) != 1 || got[0].Path != "Alpha.txt" {
		t.Fatalf("search = %#v, %v", got, err)
	}
}

func TestSearchPathsSortsRootRelativeResults(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(root, "a", "a.txt"), []byte("x"))
	writeBytes(t, filepath.Join(root, "a.txt"), []byte("x"))

	got, err := queryService(t, root).SearchPaths(context.Background(), "notes", ".", ".txt", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "a.txt" || got[1].Path != "a/a.txt" {
		t.Fatalf("SearchPaths() = %#v, want root-relative sorted paths", got)
	}
}

func TestSearchPathsRequiresOnlySearchPermission(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.txt"), []byte("secret"))

	got, err := queryServiceWithOperations(t, root, config.OpSearch).SearchPaths(context.Background(), "notes", ".", "note", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "note.txt" {
		t.Fatalf("SearchPaths() = %#v", got)
	}
}

func TestSearchPathsCapsHugeMaxResults(t *testing.T) {
	root := t.TempDir()
	for i := range 1001 {
		writeBytes(t, filepath.Join(root, fmt.Sprintf("match-%04d.txt", i)), []byte("x"))
	}

	got, err := queryService(t, root).SearchPaths(context.Background(), "notes", ".", "match", SearchOptions{MaxResults: maxInt()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1000 {
		t.Fatalf("SearchPaths() returned %d matches, want capped 1000", len(got))
	}
}

func TestSearchTextReturnsLineNumbersAndSkipsBinary(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.txt"), []byte("one\nSecret here\nthree\n"))
	writeBytes(t, filepath.Join(root, "binary"), []byte{0, 1, 2, 3})
	got, err := queryService(t, root).SearchText(context.Background(), "notes", ".", "secret", SearchOptions{CaseSensitive: false, MaxResults: 10})
	if err != nil || len(got) != 1 || got[0].Path != "note.txt" || got[0].Line != 2 || got[0].Text != "Secret here" {
		t.Fatalf("matches = %#v, %v", got, err)
	}
}

func TestSearchTextFindsMatchesInNestedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(root, "notes", "nested.txt"), []byte("first needle\nsecond\n"))

	got, err := queryService(t, root).SearchText(context.Background(), "notes", ".", "needle", SearchOptions{MaxResults: 10})
	if err != nil || len(got) != 1 || got[0].Path != "notes/nested.txt" || got[0].Line != 1 || got[0].Text != "first needle" {
		t.Fatalf("nested matches = %#v, %v", got, err)
	}
}

func TestSearchTextDoesNotFollowDirectorySymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeBytes(t, filepath.Join(outside, "secret.txt"), []byte("secret"))
	mustSymlink(t, outside, filepath.Join(root, "linked"))
	got, err := queryService(t, root).SearchText(context.Background(), "notes", ".", "secret", SearchOptions{MaxResults: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("SearchText leaked %#v", got)
	}
}

func TestSearchTextMapsFileProviderReadError(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.txt"), []byte("secret"))
	service := queryService(t, root)
	service.beforeRead = func() error { return syscall.ESTALE }

	_, err := service.SearchText(context.Background(), "notes", ".", "secret", SearchOptions{MaxResults: 10})
	assertFSCode(t, err, "filesystem_unavailable")
}

func TestSearchTextRequiresReadPermission(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.txt"), []byte("secret"))

	_, err := queryServiceWithOperations(t, root, config.OpSearch).SearchText(context.Background(), "notes", ".", "secret", SearchOptions{MaxResults: 10})
	assertFSCode(t, err, "operation_not_allowed")
}

func TestSearchTextCapsHugeMaxResults(t *testing.T) {
	root := t.TempDir()
	lines := make([]byte, 0, 1001*6)
	for range 1001 {
		lines = append(lines, "match\n"...)
	}
	writeBytes(t, filepath.Join(root, "matches.txt"), lines)

	got, err := queryService(t, root).SearchText(context.Background(), "notes", ".", "match", SearchOptions{MaxResults: maxInt()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1000 {
		t.Fatalf("SearchText() returned %d matches, want capped 1000", len(got))
	}
}

func queryService(t *testing.T, root string) *Service {
	t.Helper()
	return queryServiceWithOperations(t, root, config.OpList, config.OpSearch, config.OpRead)
}

func queryServiceWithOperations(t *testing.T, root string, operations ...config.Operation) *Service {
	t.Helper()
	m, err := access.New([]config.DirectoryRule{{Name: "notes", Path: root, Allow: operations}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return New(m, Options{})
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
