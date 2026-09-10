// Package platform supplies build-selected, handle-relative native operations.
// It does not authorize requests; callers must obtain parents through access.
package platform

import (
	"errors"
	"io/fs"
	"path/filepath"
	"time"
)

var (
	ErrSymlink                  = errors.New("symbolic link traversal denied")
	ErrNotDirectory             = errors.New("not a directory")
	ErrCrossDevice              = errors.New("cross filesystem move unsupported")
	ErrAtomicReplaceUnsupported = errors.New("atomic replacement unsupported")
	ErrAtomicRenameUnsupported  = errors.New("atomic no-replace rename unsupported")
	ErrTrashUnsupported         = errors.New("system trash unsupported")
)

type Identity struct {
	Volume uint64
	FileID [16]byte
}
type Metadata struct {
	Mode       fs.FileMode
	Size       int64
	ModifiedAt time.Time
	CreatedAt  *time.Time
	Identity   Identity
}

// validName accepts a single entry beneath a previously pinned directory.
func validName(name string) error {
	clean, err := CleanRelative(name)
	if err != nil || name == "" || name == ".." || clean != name || filepath.Base(name) != name {
		return fs.ErrInvalid
	}
	return nil
}
