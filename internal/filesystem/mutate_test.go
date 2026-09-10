package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

func TestMoveFileRename(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "from.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove}}}, Options{})

	got, err := s.Move(context.Background(), "notes", "from.md", "notes", "to.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != "notes" || got.Path != "to.md" {
		t.Fatalf("Move() result = %#v", got)
	}
	assertMutationText(t, root, "to.md", "body")
	assertMutationMissing(t, filepath.Join(root, "from.md"))
}

func TestMoveNonEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "from/child/note.md", "body")
	if err := os.Mkdir(filepath.Join(root, "archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove}}}, Options{})

	if _, err := s.Move(context.Background(), "notes", "from", "notes", "archive/from"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, root, "archive/from/child/note.md", "body")
}

func TestMoveRejectsDestinationCollision(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "from.md", "source")
	writeMutationText(t, root, "to.md", "destination")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove}}}, Options{})

	_, err := s.Move(context.Background(), "notes", "from.md", "notes", "to.md")
	assertFSCode(t, err, "destination_exists")
	assertMutationText(t, root, "from.md", "source")
	assertMutationText(t, root, "to.md", "destination")
}

func TestMoveRejectsDirectorySelfDescendant(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "from/child.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove}}}, Options{})

	_, err := s.Move(context.Background(), "notes", "from", "notes", "from/archive")
	assertFSCode(t, err, "path_outside_root")
	assertMutationText(t, root, "from/child.md", "body")
}

func TestMoveAcrossPermittedRoots(t *testing.T) {
	fromRoot, toRoot := t.TempDir(), t.TempDir()
	writeMutationText(t, fromRoot, "from.md", "body")
	s := mutationService(t, []mutationRoot{
		{name: "from", path: fromRoot, allow: []config.Operation{config.OpMove}},
		{name: "to", path: toRoot, allow: []config.Operation{config.OpMove}},
	}, Options{})

	got, err := s.Move(context.Background(), "from", "from.md", "to", "to.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != "to" || got.Path != "to.md" {
		t.Fatalf("Move() result = %#v", got)
	}
	assertMutationText(t, toRoot, "to.md", "body")
	assertMutationMissing(t, filepath.Join(fromRoot, "from.md"))
}

func TestMoveMapsEXDEV(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "from.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove}}}, Options{})
	s.renameAt = func(*os.File, string, *os.File, string) error { return syscall.EXDEV }

	_, err := s.Move(context.Background(), "notes", "from.md", "notes", "to.md")
	assertFSCode(t, err, "cross_filesystem_move_unsupported")
	assertMutationText(t, root, "from.md", "body")
}

func TestTrashMapsEXDEV(t *testing.T) {
	root, trash := t.TempDir(), t.TempDir()
	writeMutationText(t, root, "note.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{TrashDir: trash})
	s.renameAt = func(*os.File, string, *os.File, string) error { return syscall.EXDEV }

	_, err := s.Trash(context.Background(), "notes", "note.md")
	assertFSCode(t, err, "cross_filesystem_move_unsupported")
	assertMutationText(t, root, "note.md", "body")
	assertMutationMissing(t, filepath.Join(trash, "note.md"))
}

func TestTrashUsesExtensionPreservingCollisionName(t *testing.T) {
	root, trash := t.TempDir(), t.TempDir()
	writeMutationText(t, root, "note.md", "body")
	writeMutationText(t, trash, "note.md", "first")
	writeMutationText(t, trash, "note 2.md", "second")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{TrashDir: trash})

	got, err := s.Trash(context.Background(), "notes", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != "notes" || got.Path != "note.md" {
		t.Fatalf("Trash() result = %#v", got)
	}
	assertMutationText(t, trash, "note 3.md", "body")
	assertMutationMissing(t, filepath.Join(root, "note.md"))
}

func TestTrashMovesNonEmptyDirectory(t *testing.T) {
	root, trash := t.TempDir(), t.TempDir()
	writeMutationText(t, root, "folder/child/note.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{TrashDir: trash})

	if _, err := s.Trash(context.Background(), "notes", "folder"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, trash, "folder/child/note.md", "body")
}

func TestTrashPinsConfiguredSymlinkedTrashDirectory(t *testing.T) {
	root, trashA, trashB := t.TempDir(), t.TempDir(), t.TempDir()
	configured := filepath.Join(t.TempDir(), "Trash")
	if err := os.Symlink(trashA, configured); err != nil {
		t.Fatal(err)
	}
	writeMutationText(t, root, "note.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{TrashDir: configured})
	s.beforeTrashRename = func() error {
		if err := os.Remove(configured); err != nil {
			return err
		}
		return os.Symlink(trashB, configured)
	}

	if _, err := s.Trash(context.Background(), "notes", "note.md"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, trashA, "note.md", "body")
	assertMutationMissing(t, filepath.Join(trashB, "note.md"))
}

func TestTrashRejectsRetargetDuringValidation(t *testing.T) {
	root, trashA, trashB := t.TempDir(), t.TempDir(), t.TempDir()
	configured := filepath.Join(t.TempDir(), "Trash")
	if err := os.Symlink(trashA, configured); err != nil {
		t.Fatal(err)
	}
	writeMutationText(t, root, "note.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{TrashDir: configured})
	s.beforeTrashValidate = func() error {
		if err := os.Remove(configured); err != nil {
			return err
		}
		return os.Symlink(trashB, configured)
	}

	_, err := s.Trash(context.Background(), "notes", "note.md")
	assertFSCode(t, err, "filesystem_unavailable")
	assertMutationText(t, root, "note.md", "body")
	assertMutationMissing(t, filepath.Join(trashA, "note.md"))
	assertMutationMissing(t, filepath.Join(trashB, "note.md"))
}

func TestPermanentDeleteRecursivelyRemovesDirectory(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "folder/child/note.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpPermanentDelete}}}, Options{})

	got, err := s.PermanentDelete(context.Background(), "notes", "folder")
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != "notes" || got.Path != "folder" {
		t.Fatalf("PermanentDelete() result = %#v", got)
	}
	assertMutationMissing(t, filepath.Join(root, "folder"))
}

func TestPermanentDeleteRejectsConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "keep.md", "body")
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpPermanentDelete}}}, Options{})

	_, err := s.PermanentDelete(context.Background(), "notes", ".")
	assertFSCode(t, err, "path_outside_root")
	assertMutationText(t, root, "keep.md", "body")
}

func TestMutationRejectsSymlinkEscapeWithoutMutation(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeMutationText(t, outside, "secret.md", "secret")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove, config.OpTrash, config.OpPermanentDelete}}}, Options{TrashDir: t.TempDir()})

	_, err := s.Trash(context.Background(), "notes", "escape.md")
	assertFSCode(t, err, "symlink_escape")
	assertMutationText(t, outside, "secret.md", "secret")
	_, err = s.PermanentDelete(context.Background(), "notes", "escape.md")
	assertFSCode(t, err, "symlink_escape")
	assertMutationText(t, outside, "secret.md", "secret")
}

func TestPermanentDeleteUnlinksInRootSymlinkWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	writeMutationText(t, root, "target.md", "body")
	if err := os.Symlink("target.md", filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpPermanentDelete}}}, Options{})

	if _, err := s.PermanentDelete(context.Background(), "notes", "link.md"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, root, "target.md", "body")
	assertMutationMissing(t, filepath.Join(root, "link.md"))
}

func TestMoveFinalSymlinksDoNotFollowTargets(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeMutationText(t, root, "target.md", "inside")
	writeMutationText(t, outside, "target.md", "outside")
	if err := os.Symlink("target.md", filepath.Join(root, "inside-link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "outside-link.md")); err != nil {
		t.Fatal(err)
	}
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpMove}}}, Options{})

	if _, err := s.Move(context.Background(), "notes", "inside-link.md", "notes", "moved-link.md"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, root, "target.md", "inside")
	info, err := os.Lstat(filepath.Join(root, "moved-link.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("moved path is not a symlink: %v", err)
	}
	_, err = s.Move(context.Background(), "notes", "outside-link.md", "notes", "should-not-exist.md")
	assertFSCode(t, err, "symlink_escape")
	assertMutationText(t, outside, "target.md", "outside")
	assertMutationMissing(t, filepath.Join(root, "should-not-exist.md"))
}

func TestTrashFinalSymlinksDoNotFollowTargets(t *testing.T) {
	root, outside, trash := t.TempDir(), t.TempDir(), t.TempDir()
	writeMutationText(t, root, "target.md", "inside")
	writeMutationText(t, outside, "target.md", "outside")
	if err := os.Symlink("target.md", filepath.Join(root, "inside-link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "outside-link.md")); err != nil {
		t.Fatal(err)
	}
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{TrashDir: trash})

	if _, err := s.Trash(context.Background(), "notes", "inside-link.md"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, root, "target.md", "inside")
	info, err := os.Lstat(filepath.Join(trash, "inside-link.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("trashed path is not a symlink: %v", err)
	}
	_, err = s.Trash(context.Background(), "notes", "outside-link.md")
	assertFSCode(t, err, "symlink_escape")
	assertMutationText(t, outside, "target.md", "outside")
}

func TestMutationDoesNotTouchDiskWhenDestinationAuthorizationFails(t *testing.T) {
	fromRoot, toRoot := t.TempDir(), t.TempDir()
	writeMutationText(t, fromRoot, "from.md", "body")
	s := mutationService(t, []mutationRoot{
		{name: "from", path: fromRoot, allow: []config.Operation{config.OpMove}},
		{name: "to", path: toRoot, allow: nil},
	}, Options{})

	_, err := s.Move(context.Background(), "from", "from.md", "to", "to.md")
	assertFSCode(t, err, "operation_not_allowed")
	assertMutationText(t, fromRoot, "from.md", "body")
	assertMutationMissing(t, filepath.Join(toRoot, "to.md"))
}

type mutationRoot struct {
	name  string
	path  string
	allow []config.Operation
}

func mutationService(t *testing.T, roots []mutationRoot, options Options) *Service {
	t.Helper()
	rules := make([]config.DirectoryRule, 0, len(roots))
	for _, root := range roots {
		rules = append(rules, config.DirectoryRule{Name: root.name, Path: root.path, Allow: root.allow})
	}
	manager, err := access.New(rules)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return New(manager, options)
}

func writeMutationText(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertMutationText(t *testing.T, root, relative, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil || string(got) != want {
		t.Fatalf("file %q = %q, %v; want %q", relative, got, err, want)
	}
}

func assertMutationMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %q still exists or failed unexpectedly: %v", path, err)
	}
}
