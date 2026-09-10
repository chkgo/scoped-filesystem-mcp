//go:build darwin || linux

package filesystem

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestEditTextPreservesExistingMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.md")
	writeBytes(t, path, []byte("old\n"))
	if err := os.Chmod(path, 0o750); err != nil {
		t.Fatal(err)
	}

	_, conflict, err := writeService(t, root).EditText(context.Background(), "notes", "private.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if err != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %v", conflict, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("mode after edit = %v, %v", info.Mode(), err)
	}
}

func TestEditTextRestoresModeFilteredByUmask(t *testing.T) {
	oldUmask := unix.Umask(0o077)
	t.Cleanup(func() { unix.Umask(oldUmask) })

	root := t.TempDir()
	path := filepath.Join(root, "shared.md")
	writeBytes(t, path, []byte("old\n"))
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}

	_, conflict, err := writeService(t, root).EditText(context.Background(), "notes", "shared.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if err != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %v", conflict, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("mode after umask-filtered edit = %v, %v", info.Mode(), err)
	}
}
