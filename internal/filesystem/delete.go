package filesystem

import (
	"context"
	"errors"
	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
	"io"
	"os"
)

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
		if err := platform.RemoveAt(parent, name, false); err != nil {
			return false, err
		}
		return true, nil
	}
	directory, err := platform.OpenDirectoryAt(parent, name)
	if err != nil {
		return false, err
	}
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
	if err := platform.RemoveAt(parent, name, true); err != nil {
		return removedAny, err
	}
	return true, nil
}
