package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

// Trash moves a file or directory to the configured macOS Trash directory.
func (s *Service) Trash(ctx context.Context, root, path string) (MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return MutationResult{}, newError(root, path, config.OpTrash, "filesystem_unavailable", err)
	}
	target, err := s.access.Resolve(root, path, config.OpTrash, access.Existing)
	if err != nil {
		return MutationResult{}, MapError(root, path, config.OpTrash, err)
	}
	if target.Relative == "." {
		return MutationResult{}, newError(root, path, config.OpTrash, "path_outside_root", nil)
	}
	parent, name, err := mutationParent(target)
	if err != nil {
		return MutationResult{}, MapError(root, path, config.OpTrash, err)
	}
	defer parent.Close()
	if _, err := statAt(parent, name); err != nil {
		return MutationResult{}, MapError(root, path, config.OpTrash, err)
	}
	trash, err := s.openTrash()
	if err != nil {
		return MutationResult{}, MapError(root, path, config.OpTrash, err)
	}
	defer trash.Close()
	if s.beforeTrashRename != nil {
		if err := s.beforeTrashRename(); err != nil {
			return MutationResult{}, MapError(root, path, config.OpTrash, err)
		}
	}

	for number := 1; ; number++ {
		candidate := trashName(name, number)
		if err := requireMissing(trash, candidate); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return MutationResult{}, MapError(root, path, config.OpTrash, err)
		}
		if err := ctx.Err(); err != nil {
			return MutationResult{}, newError(root, path, config.OpTrash, "filesystem_unavailable", err)
		}
		if err := s.rename(parent, name, trash, candidate); err != nil {
			switch {
			case errors.Is(err, unix.EEXIST):
				continue
			case errors.Is(err, syscall.EXDEV):
				return MutationResult{}, newError(root, path, config.OpTrash, "cross_filesystem_move_unsupported", err)
			default:
				return MutationResult{}, MapError(root, path, config.OpTrash, err)
			}
		}
		return MutationResult{Root: root, Path: path}, nil
	}
}

func (s *Service) openTrash() (*os.File, error) {
	trashPath := s.trashDir
	var defaultIdentity os.FileInfo
	if trashPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		trashPath = filepath.Join(home, ".Trash")
		defaultIdentity, err = os.Lstat(trashPath)
		if err != nil {
			return nil, err
		}
		if !defaultIdentity.IsDir() || defaultIdentity.Mode()&os.ModeSymlink != 0 {
			return nil, unix.EPERM
		}
	}
	if !filepath.IsAbs(trashPath) {
		return nil, unix.EPERM
	}
	configured := filepath.Clean(trashPath)
	canonical, err := filepath.EvalSymlinks(configured)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(canonical, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	trash := os.NewFile(uintptr(fd), canonical)
	info, err := trash.Stat()
	if err != nil {
		trash.Close()
		return nil, err
	}
	// Pin the default destination too. If privacy controls deny opening it,
	// fail before moving the source rather than using a path-based fallback.
	if defaultIdentity != nil && !os.SameFile(defaultIdentity, info) {
		trash.Close()
		return nil, unix.ESTALE
	}
	if !info.IsDir() {
		trash.Close()
		return nil, unix.ENOTDIR
	}
	if s.beforeTrashValidate != nil {
		if err := s.beforeTrashValidate(); err != nil {
			trash.Close()
			return nil, err
		}
	}
	// A configured Trash symlink is permitted only while it still resolves to
	// the opened directory. The descriptor remains pinned if it changes later.
	current, err := filepath.EvalSymlinks(configured)
	if err != nil || current != canonical {
		trash.Close()
		if err != nil {
			return nil, err
		}
		return nil, unix.ESTALE
	}
	currentInfo, err := os.Stat(current)
	if err != nil || !os.SameFile(info, currentInfo) {
		trash.Close()
		if err != nil {
			return nil, err
		}
		return nil, unix.ESTALE
	}
	return trash, nil
}

func trashName(name string, number int) string {
	if number == 1 {
		return name
	}
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	if stem == "" {
		stem, extension = name, ""
	}
	return stem + " " + strconv.Itoa(number) + extension
}

// PermanentDelete recursively unlinks a file or directory without following
// symlinks. Server-side elicitation is intentionally performed before calling
// this primitive.
func (s *Service) PermanentDelete(ctx context.Context, root, path string) (MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return MutationResult{}, newError(root, path, config.OpPermanentDelete, "filesystem_unavailable", err)
	}
	target, err := s.access.Resolve(root, path, config.OpPermanentDelete, access.Existing)
	if err != nil {
		return MutationResult{}, MapError(root, path, config.OpPermanentDelete, err)
	}
	if target.Relative == "." {
		return MutationResult{}, newError(root, path, config.OpPermanentDelete, "path_outside_root", nil)
	}
	parent, name, err := mutationParent(target)
	if err != nil {
		return MutationResult{}, MapError(root, path, config.OpPermanentDelete, err)
	}
	defer parent.Close()
	if err := ctx.Err(); err != nil {
		return MutationResult{}, newError(root, path, config.OpPermanentDelete, "filesystem_unavailable", err)
	}
	removed, err := s.removeAt(ctx, parent, name)
	if err != nil {
		if removed {
			return MutationResult{}, newError(root, path, config.OpPermanentDelete, "partial_delete", err)
		}
		return MutationResult{}, MapError(root, path, config.OpPermanentDelete, err)
	}
	return MutationResult{Root: root, Path: path}, nil
}

func (s *Service) removeAt(ctx context.Context, parent *os.File, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	stat, err := statAt(parent, name)
	if err != nil {
		return false, err
	}
	if !isDirectory(stat) {
		if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
			return false, err
		}
		return true, nil
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	directory := os.NewFile(uintptr(fd), name)
	removedAny := false
	var readErr error
	for readErr == nil {
		if err := ctx.Err(); err != nil {
			readErr = err
			break
		}
		entries, err := directory.ReadDir(128)
		if errors.Is(err, os.ErrClosed) {
			readErr = err
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			readErr = err
			break
		}
		for _, entry := range entries {
			if ctxErr := ctx.Err(); ctxErr != nil {
				readErr = ctxErr
				break
			}
			if s.beforeDeleteEntry != nil {
				if hookErr := s.beforeDeleteEntry(entry.Name()); hookErr != nil {
					readErr = hookErr
					break
				}
			}
			removed, childErr := s.removeAt(ctx, directory, entry.Name())
			removedAny = removedAny || removed
			if childErr != nil {
				readErr = childErr
				break
			}
		}
		if readErr != nil || errors.Is(err, io.EOF) {
			break
		}
	}
	closeErr := directory.Close()
	if readErr != nil {
		return removedAny, readErr
	}
	if closeErr != nil {
		return removedAny, closeErr
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return removedAny, err
	}
	return true, nil
}
