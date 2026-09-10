package filesystem

import (
	"context"
	"errors"

	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

// Trash uses the native system trash, or an explicitly supplied isolated sink.
func (s *Service) Trash(ctx context.Context, root, path string) (MutationResult, error) {
	if s.trashDir != "" {
		return s.trashToDirectory(ctx, root, path, s.trashDir, false)
	}
	return s.trashNative(ctx, root, path)
}

func (s *Service) trashToDirectory(ctx context.Context, root, path, trashPath string, requireRealDir bool) (MutationResult, error) {
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
	trash, err := s.openTrash(trashPath, requireRealDir)
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
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return MutationResult{}, MapError(root, path, config.OpTrash, err)
		}
		if err := ctx.Err(); err != nil {
			return MutationResult{}, newError(root, path, config.OpTrash, "filesystem_unavailable", err)
		}
		if err := s.rename(parent, name, trash, candidate); err != nil {
			switch {
			case errors.Is(err, os.ErrExist):
				continue
			case errors.Is(err, platform.ErrCrossDevice):
				return MutationResult{}, newError(root, path, config.OpTrash, "cross_filesystem_move_unsupported", err)
			default:
				return MutationResult{}, MapError(root, path, config.OpTrash, err)
			}
		}
		return MutationResult{Root: root, Path: path}, nil
	}
}

func (s *Service) openTrash(trashPath string, requireRealDir bool) (*os.File, error) {
	var defaultIdentity os.FileInfo
	if requireRealDir {
		var err error
		defaultIdentity, err = os.Lstat(trashPath)
		if err != nil {
			return nil, err
		}
		if !defaultIdentity.IsDir() || defaultIdentity.Mode()&os.ModeSymlink != 0 {
			return nil, os.ErrPermission
		}
	}
	if !filepath.IsAbs(trashPath) {
		return nil, os.ErrPermission
	}
	configured := filepath.Clean(trashPath)
	canonical, err := filepath.EvalSymlinks(configured)
	if err != nil {
		return nil, err
	}
	trash, err := platform.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	info, err := trash.Stat()
	if err != nil {
		trash.Close()
		return nil, err
	}
	// Pin the default destination too. If privacy controls deny opening it,
	// fail before moving the source rather than using a path-based fallback.
	if defaultIdentity != nil && !os.SameFile(defaultIdentity, info) {
		trash.Close()
		return nil, errors.New("trash target changed")
	}
	if !info.IsDir() {
		trash.Close()
		return nil, platform.ErrNotDirectory
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
		return nil, errors.New("trash target changed")
	}
	currentInfo, err := os.Stat(current)
	if err != nil || !os.SameFile(info, currentInfo) {
		trash.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("trash target changed")
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
