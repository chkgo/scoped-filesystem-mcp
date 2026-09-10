package filesystem

import (
	"context"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"os"
	"path/filepath"
)

func (s *Service) trashNative(ctx context.Context, root, path string) (MutationResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return MutationResult{}, MapError(root, path, config.OpTrash, err)
	}
	return s.trashToDirectory(ctx, root, path, filepath.Join(home, ".Trash"), true)
}
