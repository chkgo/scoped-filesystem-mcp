package access

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

func TestResolveExistingPathWithinRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "daily.md"), "today")
	m := newManager(t, root, config.OpRead)

	got, err := m.Resolve("notes", "daily.md", config.OpRead, Existing)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.RootName != "notes" || got.Relative != "daily.md" || got.Display != filepath.Join(root, "daily.md") || got.Canonical != filepath.Join(canonicalRoot(t, root), "daily.md") {
		t.Fatalf("Resolve() = %#v", got)
	}
}

func TestPathOpenReadsResolvedFileRelativeToPinnedRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "daily.md"), "today")
	m := newManager(t, root, config.OpRead)
	path, err := m.Resolve("notes", "daily.md", config.OpRead, Existing)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	file, err := path.Open(os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Path.Open() error = %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "today" {
		t.Fatalf("Path.Open() read %q, want %q", data, "today")
	}
}

func TestPathOpenRootUsesIndependentDirectoryOffsets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "daily.md"), "today")
	manager := newManager(t, root, config.OpList)
	path, err := manager.Resolve("notes", ".", config.OpList, Existing)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	first, err := path.Open(os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("first Path.Open() error = %v", err)
	}
	if _, err := first.ReadDir(-1); err != nil {
		first.Close()
		t.Fatalf("first ReadDir() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := path.Open(os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("second Path.Open() error = %v", err)
	}
	defer second.Close()
	entries, err := second.ReadDir(-1)
	if err != nil || len(entries) != 1 || entries[0].Name() != "daily.md" {
		t.Fatalf("second ReadDir() = %#v, %v; want fresh directory entries", entries, err)
	}
}

func TestPathOpenRejectsDirectorySymlinkSwappedAfterResolve(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	insideDir := filepath.Join(root, "docs")
	if err := os.Mkdir(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(insideDir, "note.md"), "safe")
	writeFile(t, filepath.Join(outside, "note.md"), "secret")
	m := newManager(t, root, config.OpRead)
	path, err := m.Resolve("notes", "docs/note.md", config.OpRead, Existing)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := os.Rename(insideDir, filepath.Join(root, "docs-original")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, insideDir)

	file, err := path.Open(os.O_RDONLY, 0)
	if err == nil {
		file.Close()
		t.Fatal("Path.Open() succeeded after a symlink swap")
	}
}

func TestPathOpenRejectsRootReplacedAfterResolve(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "notes")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "daily.md"), "today")
	m := newManager(t, root, config.OpRead)
	path, err := m.Resolve("notes", "daily.md", config.OpRead, Existing)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := os.Rename(root, filepath.Join(parent, "notes-original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = path.Open(os.O_RDONLY, 0)
	assertAccessCode(t, err, "root_target_changed")
}

func TestResolveNotYetExistingDestinationUsesExistingParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "drafts"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, root, config.OpCreate)

	got, err := m.Resolve("notes", "drafts/new.md", config.OpCreate, ParentExisting)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Canonical != filepath.Join(canonicalRoot(t, root), "drafts", "new.md") || got.Display != filepath.Join(root, "drafts", "new.md") {
		t.Fatalf("Resolve() = %#v", got)
	}
}

func TestResolveRejectsAbsoluteToolPath(t *testing.T) {
	m := newManager(t, t.TempDir(), config.OpRead)

	_, err := m.Resolve("notes", "/tmp/secret.md", config.OpRead, Existing)
	assertAccessCode(t, err, "path_outside_root")
}

func TestResolveRejectsTraversal(t *testing.T) {
	m := newManager(t, t.TempDir(), config.OpRead)

	_, err := m.Resolve("notes", "../secret.md", config.OpRead, Existing)
	assertAccessCode(t, err, "path_outside_root")
}

func TestResolveRejectsPrefixConfusion(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	other := filepath.Join(parent, "root-other")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, other, filepath.Join(root, "other"))
	m := newManager(t, root, config.OpRead)

	_, err := m.Resolve("notes", "other/secret.md", config.OpRead, Existing)
	assertAccessCode(t, err, "symlink_escape")
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	mustSymlink(t, outside, filepath.Join(root, "escape"))
	m := newManager(t, root, config.OpRead)

	_, err := m.Resolve("notes", "escape/secret.md", config.OpRead, Existing)
	assertAccessCode(t, err, "symlink_escape")
}

func TestConfiguredSymlinkRootIsAccepted(t *testing.T) {
	realRoot, linkParent := t.TempDir(), t.TempDir()
	link := filepath.Join(linkParent, "notes")
	mustSymlink(t, realRoot, link)
	m := newManagerAt(t, link, config.OpRead)

	got, err := m.Resolve("notes", "Index.md", config.OpRead, ParentExisting)
	if err != nil || !strings.HasPrefix(got.Canonical, canonicalRoot(t, realRoot)+string(os.PathSeparator)) {
		t.Fatalf("Resolve() = %#v, %v", got, err)
	}
}

func TestResolveRejectsRetargetedConfiguredRoot(t *testing.T) {
	realRoot, replacement, linkParent := t.TempDir(), t.TempDir(), t.TempDir()
	link := filepath.Join(linkParent, "notes")
	mustSymlink(t, realRoot, link)
	m := newManagerAt(t, link, config.OpRead)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, replacement, link)

	_, err := m.Resolve("notes", "Index.md", config.OpRead, ParentExisting)
	assertAccessCode(t, err, "root_target_changed")
}

func TestResolveRejectsRootReplacedAtSamePath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "notes")
	oldRoot := filepath.Join(parent, "notes-old")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, root, config.OpRead)
	if err := os.Rename(root, oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := m.Resolve("notes", "Index.md", config.OpRead, ParentExisting)
	assertAccessCode(t, err, "root_target_changed")
}

func TestResolveRejectsUnknownRootAndDisallowedOperation(t *testing.T) {
	m := newManager(t, t.TempDir(), config.OpRead)

	_, err := m.Resolve("unknown", "Index.md", config.OpRead, ParentExisting)
	assertAccessCode(t, err, "unknown_root")

	_, err = m.Resolve("notes", "Index.md", config.OpCreate, ParentExisting)
	assertAccessCode(t, err, "operation_not_allowed")
}

func TestRootsAndAsksExposeConfiguredRules(t *testing.T) {
	root := t.TempDir()
	m, err := New([]config.DirectoryRule{{
		Name:  "notes",
		Path:  root,
		Allow: []config.Operation{config.OpRead, config.OpTrash},
		Ask:   []config.Operation{config.OpTrash},
	}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	roots := m.Roots()
	if len(roots) != 1 || roots[0].Name != "notes" || roots[0].Path != root || !roots[0].Allows(config.OpRead) || roots[0].Allows(config.OpCreate) {
		t.Fatalf("Roots() = %#v", roots)
	}
	if !m.Asks("notes", config.OpTrash) || m.Asks("notes", config.OpRead) || m.Asks("unknown", config.OpTrash) {
		t.Fatalf("Asks() returned incorrect results")
	}
}

func TestNewClonesRuleOperationSlices(t *testing.T) {
	rules := []config.DirectoryRule{{
		Name:  "notes",
		Path:  t.TempDir(),
		Allow: []config.Operation{config.OpRead, config.OpTrash},
		Ask:   []config.Operation{config.OpTrash},
	}}
	m, err := New(rules)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rules[0].Allow[0] = config.OpCreate
	rules[0].Ask[0] = config.OpRead
	if _, err := m.Resolve("notes", "Index.md", config.OpRead, ParentExisting); err != nil {
		t.Fatalf("Resolve() error = %v, want original read permission", err)
	}
	if !m.Asks("notes", config.OpTrash) || m.Asks("notes", config.OpRead) {
		t.Fatalf("Asks() reflected caller mutation")
	}
}

func newManager(t *testing.T, root string, operations ...config.Operation) *Manager {
	t.Helper()
	return newManagerAt(t, root, operations...)
}

func newManagerAt(t *testing.T, path string, operations ...config.Operation) *Manager {
	t.Helper()
	m, err := New([]config.DirectoryRule{{Name: "notes", Path: path, Allow: operations}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Manager.Close() error = %v", err)
		}
	})
	return m
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func canonicalRoot(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func assertAccessCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Resolve() error = nil, want code %q", want)
	}
	var accessErr *Error
	if !errors.As(err, &accessErr) || accessErr.Code != want {
		t.Fatalf("Resolve() error = %v, want code %q", err, want)
	}
}
