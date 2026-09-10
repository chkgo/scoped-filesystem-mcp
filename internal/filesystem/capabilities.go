package filesystem

import (
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
)

// OperationError describes implementation availability, not authorization or
// the capabilities of a particular mounted filesystem. Native calls still
// check those capabilities when they run.
func (s *Service) OperationError(op config.Operation) error {
	if op == config.OpEdit && !platform.SupportsAtomicEdit {
		return platform.ErrAtomicReplaceUnsupported
	}
	if op == config.OpTrash && s.trashDir == "" && !platform.SupportsNativeTrash {
		return platform.ErrTrashUnsupported
	}
	return nil
}
