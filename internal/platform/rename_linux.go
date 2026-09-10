package platform

import (
	"errors"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
)

func renameUnsupported(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP)
}
func RenameNoReplace(a *os.File, from string, b *os.File, to string) error {
	if validName(from) != nil || validName(to) != nil || from == "." || to == "." {
		return fs.ErrInvalid
	}
	err := unix.Renameat2(int(a.Fd()), from, int(b.Fd()), to, unix.RENAME_NOREPLACE)
	if renameUnsupported(err) {
		return errors.Join(ErrAtomicRenameUnsupported, err)
	}
	return normalizeError(err)
}
func Exchange(parent *os.File, first, second string) error {
	if validName(first) != nil || validName(second) != nil || first == "." || second == "." {
		return fs.ErrInvalid
	}
	err := unix.Renameat2(int(parent.Fd()), first, int(parent.Fd()), second, unix.RENAME_EXCHANGE)
	if renameUnsupported(err) || errors.Is(err, unix.EINVAL) {
		return errors.Join(ErrAtomicReplaceUnsupported, err)
	}
	return normalizeError(err)
}
