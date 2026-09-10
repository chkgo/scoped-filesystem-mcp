package filesystem

import (
	"context"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
)

// Windows Shell recycling is not enabled until it can satisfy the same pinned
// source and never-permanently-delete guarantees as the native Unix backends.
func (s *Service) trashNative(ctx context.Context, root, path string) (MutationResult, error) {
	return MutationResult{}, MapError(root, path, config.OpTrash, platform.ErrTrashUnsupported)
}
