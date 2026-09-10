package filesystem

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
)

// Error is a stable, user-facing filesystem failure. Its message never includes
// the canonical absolute path; the configured root and requested relative path
// are safe diagnostic context.
type Error struct {
	Code      string
	Root      string
	Path      string
	Operation config.Operation
	err       error
}

func (e *Error) Error() string {
	if e.Root == "" && e.Path == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s %s", e.Code, e.Operation, e.Root+"/"+e.Path)
}
func (e *Error) Unwrap() error { return e.err }

// SafeDetail exposes only a stable errno description, never an OS error string
// that may contain an absolute pathname.
func (e *Error) SafeDetail() string {
	var errno syscall.Errno
	if errors.As(e.err, &errno) {
		return errno.Error()
	}
	return ""
}

// MapError translates OS and authorization failures into stable filesystem errors.
func MapError(root, path string, operation config.Operation, err error) error {
	if err == nil {
		return nil
	}
	code := "filesystem_unavailable"
	var ae *access.Error
	if errors.As(err, &ae) {
		code = ae.Code
	}
	if errors.Is(err, syscall.ESTALE) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ENXIO) {
		code = "filesystem_unavailable"
	}
	if errors.Is(err, os.ErrNotExist) {
		code = "path_not_found"
	}
	if errors.Is(err, platform.ErrCrossDevice) || errors.Is(err, syscall.EXDEV) {
		code = "cross_filesystem_move_unsupported"
	}
	if errors.Is(err, platform.ErrAtomicRenameUnsupported) {
		code = "atomic_rename_unsupported"
	}
	if errors.Is(err, platform.ErrAtomicReplaceUnsupported) {
		code = "atomic_replace_unsupported"
	}
	if errors.Is(err, platform.ErrTrashUnsupported) {
		code = "trash_unsupported"
	}
	return &Error{Code: code, Root: root, Path: path, Operation: operation, err: err}
}

func newError(root, path string, operation config.Operation, code string, err error) error {
	return &Error{Code: code, Root: root, Path: path, Operation: operation, err: err}
}
