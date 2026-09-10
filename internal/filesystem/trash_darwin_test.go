//go:build darwin

package filesystem

import (
	"context"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestTrashDefaultPinsDestinationAndFailsClosed(t *testing.T) {
	for _, scenario := range []string{"permission denied", "retarget before rename", "retarget during validation", "collision"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			trash := filepath.Join(home, ".Trash")
			if err := os.Mkdir(trash, 0o700); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeMutationText(t, root, "note.md", "body")
			s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{})
			redirected := t.TempDir()
			original := filepath.Join(home, "original-trash")
			retarget := func() error {
				if err := os.Rename(trash, original); err != nil {
					return err
				}
				return os.Symlink(redirected, trash)
			}
			switch scenario {
			case "permission denied":
				s.beforeTrashValidate = func() error { return syscall.EPERM }
			case "retarget before rename":
				s.beforeTrashRename = retarget
			case "retarget during validation":
				s.beforeTrashValidate = retarget
			case "collision":
				writeMutationText(t, trash, "note.md", "existing")
			}
			_, err := s.Trash(context.Background(), "notes", "note.md")
			if scenario == "permission denied" || scenario == "retarget during validation" {
				assertFSCode(t, err, "filesystem_unavailable")
				assertMutationText(t, root, "note.md", "body")
				assertMutationMissing(t, filepath.Join(trash, "note.md"))
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertMutationMissing(t, filepath.Join(root, "note.md"))
				if scenario == "collision" {
					assertMutationText(t, trash, "note.md", "existing")
					assertMutationText(t, trash, "note 2.md", "body")
				} else {
					assertMutationText(t, original, "note.md", "body")
				}
			}
			assertMutationMissing(t, filepath.Join(redirected, "note.md"))
		})
	}
}
