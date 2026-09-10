package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

// MutationResult identifies a completed mutation using only configured root
// names and root-relative paths. It never includes a canonical absolute path.
type MutationResult struct {
	Root string
	Path string
}

// Move renames a file or directory. It never falls back to copy and delete.
func (s *Service) Move(ctx context.Context, sourceRoot, sourcePath, destinationRoot, destinationPath string) (MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return MutationResult{}, newError(sourceRoot, sourcePath, config.OpMove, "filesystem_unavailable", err)
	}
	source, err := s.access.Resolve(sourceRoot, sourcePath, config.OpMove, access.Existing)
	if err != nil {
		return MutationResult{}, MapError(sourceRoot, sourcePath, config.OpMove, err)
	}
	if source.Relative == "." {
		return MutationResult{}, newError(sourceRoot, sourcePath, config.OpMove, "path_outside_root", nil)
	}
	destination, err := s.access.Resolve(destinationRoot, destinationPath, config.OpMove, access.ParentExisting)
	if err != nil {
		return MutationResult{}, MapError(destinationRoot, destinationPath, config.OpMove, err)
	}
	if destination.Relative == "." {
		return MutationResult{}, newError(destinationRoot, destinationPath, config.OpMove, "path_outside_root", nil)
	}
	sourceParent, sourceName, err := mutationParent(source)
	if err != nil {
		return MutationResult{}, MapError(sourceRoot, sourcePath, config.OpMove, err)
	}
	defer sourceParent.Close()
	destinationParent, destinationName, err := mutationParent(destination)
	if err != nil {
		return MutationResult{}, MapError(destinationRoot, destinationPath, config.OpMove, err)
	}
	defer destinationParent.Close()

	sourceInfo, err := statAt(sourceParent, sourceName)
	if err != nil {
		return MutationResult{}, MapError(sourceRoot, sourcePath, config.OpMove, err)
	}
	if err := requireMissing(destinationParent, destinationName); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return MutationResult{}, newError(destinationRoot, destinationPath, config.OpMove, "destination_exists", err)
		}
		return MutationResult{}, MapError(destinationRoot, destinationPath, config.OpMove, err)
	}
	if isDirectory(sourceInfo) && containedPath(source.Canonical, destination.Canonical) {
		return MutationResult{}, newError(destinationRoot, destinationPath, config.OpMove, "path_outside_root", nil)
	}
	if err := ctx.Err(); err != nil {
		return MutationResult{}, newError(sourceRoot, sourcePath, config.OpMove, "filesystem_unavailable", err)
	}
	if err := s.rename(sourceParent, sourceName, destinationParent, destinationName); err != nil {
		switch {
		case errors.Is(err, syscall.EXDEV):
			return MutationResult{}, newError(destinationRoot, destinationPath, config.OpMove, "cross_filesystem_move_unsupported", err)
		case errors.Is(err, unix.EEXIST):
			return MutationResult{}, newError(destinationRoot, destinationPath, config.OpMove, "destination_exists", err)
		default:
			return MutationResult{}, MapError(destinationRoot, destinationPath, config.OpMove, err)
		}
	}
	return MutationResult{Root: destinationRoot, Path: destinationPath}, nil
}

func (s *Service) rename(sourceParent *os.File, sourceName string, destinationParent *os.File, destinationName string) error {
	if s.renameAt != nil {
		return s.renameAt(int(sourceParent.Fd()), sourceName, int(destinationParent.Fd()), destinationName)
	}
	// RENAME_EXCL preserves the promised missing-destination rule even if a
	// concurrent process creates the name after the descriptor-relative check.
	return unix.RenameatxNp(int(sourceParent.Fd()), sourceName, int(destinationParent.Fd()), destinationName, unix.RENAME_EXCL)
}

func mutationParent(path access.Path) (*os.File, string, error) {
	parent, remaining, err := path.OpenRequestedParent()
	if err != nil {
		return nil, "", err
	}
	if len(remaining) != 1 || remaining[0] == "." {
		parent.Close()
		return nil, "", unix.ENOENT
	}
	return parent, remaining[0], nil
}

func statAt(parent *os.File, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	return stat, nil
}

func requireMissing(parent *os.File, name string) error {
	_, err := statAt(parent, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err == nil {
		return unix.EEXIST
	}
	return err
}

func isDirectory(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFDIR
}

func containedPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
