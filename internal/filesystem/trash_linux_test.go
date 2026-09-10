package filesystem

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
)

func nativeLinuxTrashService(t *testing.T) (*Service, string, string) {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpTrash}}}, Options{})
	return s, root, filepath.Join(data, "Trash")
}

func TestLinuxTrashLayoutAndRestorationMetadata(t *testing.T) {
	for _, name := range []string{"note.md", "space percent% юникод\n.md", strings.Repeat("x", 255)} {
		t.Run(name, func(t *testing.T) {
			s, root, trash := nativeLinuxTrashService(t)
			writeMutationText(t, root, name, "body")
			if _, err := s.Trash(context.Background(), "notes", name); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(filepath.Join(trash, "files"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("files = %v, %v", entries, err)
			}
			candidate := entries[0].Name()
			assertMutationText(t, filepath.Join(trash, "files"), candidate, "body")
			assertMutationMissing(t, filepath.Join(root, name))
			metadata, err := os.ReadFile(filepath.Join(trash, "info", candidate+".trashinfo"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(metadata), "\n")
			if len(lines) != 4 || lines[0] != "[Trash Info]" || !strings.HasPrefix(lines[1], "Path=") || !strings.HasPrefix(lines[2], "DeletionDate=") {
				t.Fatalf("metadata = %q", metadata)
			}
			restored, err := url.PathUnescape(strings.TrimPrefix(lines[1], "Path="))
			if err != nil || restored != filepath.Join(root, name) {
				t.Fatalf("restore path = %q, %v", restored, err)
			}
			if _, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimPrefix(lines[2], "DeletionDate="), time.Local); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{trash, filepath.Join(trash, "files"), filepath.Join(trash, "info")} {
				info, err := os.Stat(dir)
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("private directory %s: %v, %v", dir, info, err)
				}
			}
			// Restoration uses only the stored path and paired files entry.
			if err := os.Rename(filepath.Join(trash, "files", candidate), restored); err != nil {
				t.Fatal(err)
			}
			assertMutationText(t, root, name, "body")
		})
	}
}

func TestLinuxTrashUsesFallbackForUnsetOrRelativeXDG(t *testing.T) {
	for _, xdg := range []string{"", "relative/data"} {
		t.Run(xdg, func(t *testing.T) {
			s, root, _ := nativeLinuxTrashService(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", xdg)
			writeMutationText(t, root, "note", "body")
			if _, err := s.Trash(context.Background(), "notes", "note"); err != nil {
				t.Fatal(err)
			}
			assertMutationText(t, filepath.Join(home, ".local/share/Trash/files"), "note", "body")
		})
	}
}

func TestLinuxTrashCollisionsInBothDirectories(t *testing.T) {
	s, root, trash := nativeLinuxTrashService(t)
	writeMutationText(t, root, "note.md", "body")
	writeMutationText(t, trash, "files/note.md", "existing file")
	writeMutationText(t, trash, "info/note 2.md.trashinfo", "existing info")
	if _, err := s.Trash(context.Background(), "notes", "note.md"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, trash, "files/note.md", "existing file")
	assertMutationText(t, trash, "info/note 2.md.trashinfo", "existing info")
	assertMutationText(t, trash, "files/note 3.md", "body")
	assertMutationMissing(t, filepath.Join(trash, "info/note.md.trashinfo"))
}

func TestLinuxTrashDirectoriesAndFinalSymlinks(t *testing.T) {
	s, root, trash := nativeLinuxTrashService(t)
	writeMutationText(t, root, "folder/child", "body")
	writeMutationText(t, root, "target", "target")
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"folder", "link"} {
		if _, err := s.Trash(context.Background(), "notes", name); err != nil {
			t.Fatal(err)
		}
	}
	assertMutationText(t, trash, "files/folder/child", "body")
	assertMutationText(t, root, "target", "target")
	link, err := os.Readlink(filepath.Join(trash, "files/link"))
	if err != nil || link != "target" {
		t.Fatalf("link = %q, %v", link, err)
	}
}

func TestLinuxTrashFailuresPreserveSourceAndCleanOnlyReservedMetadata(t *testing.T) {
	for _, failure := range []string{"cancel", "permission", "cross-device", "rename-permission", "metadata-collision"} {
		t.Run(failure, func(t *testing.T) {
			s, root, trash := nativeLinuxTrashService(t)
			writeMutationText(t, root, "note", "body")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			code := "filesystem_unavailable"
			s.beforeTrashRename = func() error {
				metadata, err := os.ReadFile(filepath.Join(trash, "info/note.trashinfo"))
				if err != nil || !strings.HasPrefix(string(metadata), "[Trash Info]\n") {
					t.Fatalf("metadata absent before move: %q, %v", metadata, err)
				}
				if failure == "cancel" {
					cancel()
				}
				if failure == "permission" {
					return syscall.EACCES
				}
				return nil
			}
			switch failure {
			case "cross-device":
				code = "cross_filesystem_move_unsupported"
				s.renameAt = func(*os.File, string, *os.File, string) error { return syscall.EXDEV }
			case "rename-permission":
				s.renameAt = func(*os.File, string, *os.File, string) error { return syscall.EACCES }
			case "metadata-collision":
				// A non-directory metadata path must fail before source mutation.
				writeMutationText(t, trash, "info", "unrelated")
				s.beforeTrashRename = nil
			}
			_, err := s.Trash(ctx, "notes", "note")
			assertFSCode(t, err, code)
			assertMutationText(t, root, "note", "body")
			assertMutationMissing(t, filepath.Join(trash, "files/note"))
			if failure != "metadata-collision" {
				assertMutationMissing(t, filepath.Join(trash, "info/note.trashinfo"))
			} else {
				assertMutationText(t, trash, "info", "unrelated")
			}
		})
	}
}

func TestLinuxTrashUncertainRenameRetainsMetadata(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-move", true: "after-move"}[moved], func(t *testing.T) {
			s, root, trash := nativeLinuxTrashService(t)
			writeMutationText(t, root, "note", "body")
			s.renameAt = func(from *os.File, name string, to *os.File, candidate string) error {
				if moved {
					if err := platform.RenameNoReplace(from, name, to, candidate); err != nil {
						return err
					}
				}
				return syscall.EIO
			}
			_, err := s.Trash(context.Background(), "notes", "note")
			assertFSCode(t, err, "trash_outcome_uncertain")
			if _, err := os.Stat(filepath.Join(trash, "info/note.trashinfo")); err != nil {
				t.Fatal(err)
			}
			if moved {
				assertMutationText(t, trash, "files/note", "body")
			} else {
				assertMutationText(t, root, "note", "body")
			}
		})
	}
}

func TestLinuxTrashRejectsSymlinkAndInsecureDestinations(t *testing.T) {
	for _, scenario := range []string{"data-symlink", "trash-symlink", "files-symlink", "info-symlink", "insecure-trash", "insecure-data"} {
		t.Run(scenario, func(t *testing.T) {
			s, root, trash := nativeLinuxTrashService(t)
			writeMutationText(t, root, "note", "body")
			if err := os.MkdirAll(trash, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			switch scenario {
			case "data-symlink":
				link := filepath.Join(t.TempDir(), "data")
				if err := os.Symlink(filepath.Dir(trash), link); err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_DATA_HOME", link)
			case "trash-symlink":
				if err := os.Remove(trash); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, trash); err != nil {
					t.Fatal(err)
				}
			case "files-symlink", "info-symlink":
				if err := os.Symlink(outside, filepath.Join(trash, strings.TrimSuffix(scenario, "-symlink"))); err != nil {
					t.Fatal(err)
				}
			case "insecure-trash":
				if err := os.Chmod(trash, 0o777); err != nil {
					t.Fatal(err)
				}
			case "insecure-data":
				if err := os.Chmod(filepath.Dir(trash), 0o777); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Trash(context.Background(), "notes", "note"); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			assertMutationText(t, root, "note", "body")
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside modified: %v, %v", entries, err)
			}
		})
	}
}

func TestLinuxTrashPinsDestinationAndRejectsValidationRetarget(t *testing.T) {
	for _, beforeValidation := range []bool{true, false} {
		t.Run(map[bool]string{true: "during-validation", false: "before-move"}[beforeValidation], func(t *testing.T) {
			s, root, trash := nativeLinuxTrashService(t)
			writeMutationText(t, root, "note", "body")
			original, redirected := trash+"-original", t.TempDir()
			retarget := func() error {
				if err := os.Rename(trash, original); err != nil {
					return err
				}
				return os.Symlink(redirected, trash)
			}
			if beforeValidation {
				s.beforeTrashValidate = retarget
			} else {
				s.beforeTrashRename = retarget
			}
			_, err := s.Trash(context.Background(), "notes", "note")
			if beforeValidation {
				if err == nil {
					t.Fatal("retarget accepted")
				}
				assertMutationText(t, root, "note", "body")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertMutationText(t, original, "files/note", "body")
				if _, err := os.Stat(filepath.Join(original, "info/note.trashinfo")); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(redirected)
			if err != nil || len(entries) != 0 {
				t.Fatalf("redirected modified: %v, %v", entries, err)
			}
		})
	}
}

func TestLinuxTrashMetadataWriteFailure(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "info")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeLinuxTrashInfo(f, "/original", time.Now()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed metadata write = %v", err)
	}
}

func TestLinuxTrashCollisionAppearingAtMoveDoesNotReplace(t *testing.T) {
	s, root, trash := nativeLinuxTrashService(t)
	writeMutationText(t, root, "note", "body")
	first := true
	s.beforeTrashRename = func() error {
		if first {
			first = false
			writeMutationText(t, trash, "files/note", "racing writer")
		}
		return nil
	}
	if _, err := s.Trash(context.Background(), "notes", "note"); err != nil {
		t.Fatal(err)
	}
	assertMutationText(t, trash, "files/note", "racing writer")
	assertMutationText(t, trash, "files/note 2", "body")
	assertMutationMissing(t, filepath.Join(trash, "info/note.trashinfo"))
}

func TestLinuxTrashPreservesReplacedMetadataOnCleanup(t *testing.T) {
	s, root, trash := nativeLinuxTrashService(t)
	writeMutationText(t, root, "note", "body")
	s.beforeTrashRename = func() error {
		metadata := filepath.Join(trash, "info/note.trashinfo")
		if err := os.Rename(metadata, metadata+"-original"); err != nil {
			return err
		}
		writeMutationText(t, trash, "info/note.trashinfo", "another writer")
		return syscall.EACCES
	}
	_, err := s.Trash(context.Background(), "notes", "note")
	assertFSCode(t, err, "trash_metadata_cleanup_failed")
	assertMutationText(t, root, "note", "body")
	assertMutationText(t, trash, "info/note.trashinfo", "another writer")
}

func TestLinuxTrashRemoteRetryFailureRetainsMetadata(t *testing.T) {
	s, root, trash := nativeLinuxTrashService(t)
	writeMutationText(t, root, "note", "body")
	s.renameAt = func(from *os.File, name string, to *os.File, candidate string) error {
		if err := platform.RenameNoReplace(from, name, to, candidate); err != nil {
			return err
		}
		return syscall.ENOENT
	}
	_, err := s.Trash(context.Background(), "notes", "note")
	assertFSCode(t, err, "trash_outcome_uncertain")
	assertMutationText(t, trash, "files/note", "body")
	if _, err := os.Stat(filepath.Join(trash, "info/note.trashinfo")); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxTrashRejectsRealCrossFilesystemMove(t *testing.T) {
	sourceRoot, err := os.MkdirTemp("/dev/shm", "scopedfs-trash-")
	if err != nil {
		t.Skipf("second filesystem unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceRoot) })
	data := t.TempDir()
	sourceInfo, err := os.Stat(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if sourceInfo.Sys().(*syscall.Stat_t).Dev == dataInfo.Sys().(*syscall.Stat_t).Dev {
		t.Skip("/dev/shm and temporary root are on the same filesystem")
	}
	t.Setenv("XDG_DATA_HOME", data)
	s := mutationService(t, []mutationRoot{{name: "notes", path: sourceRoot, allow: []config.Operation{config.OpTrash}}}, Options{})
	writeMutationText(t, sourceRoot, "note", "body")
	_, err = s.Trash(context.Background(), "notes", "note")
	assertFSCode(t, err, "cross_filesystem_move_unsupported")
	assertMutationText(t, sourceRoot, "note", "body")
	for _, directory := range []string{"files", "info"} {
		entries, err := os.ReadDir(filepath.Join(data, "Trash", directory))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed move left %s entries: %v, %v", directory, entries, err)
		}
	}
}

func TestLinuxTrashRejectsOtherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership fixture requires permission to chown a disposable directory")
	}
	s, root, trash := nativeLinuxTrashService(t)
	writeMutationText(t, root, "note", "body")
	if err := os.Mkdir(trash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(trash, 65534, -1); err != nil {
		t.Fatal(err)
	}
	_, err := s.Trash(context.Background(), "notes", "note")
	assertFSCode(t, err, "filesystem_unavailable")
	assertMutationText(t, root, "note", "body")
}
