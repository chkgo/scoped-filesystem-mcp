//go:build darwin || linux

package platform

import (
	"encoding/binary"
	"errors"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const SupportsAtomicEdit = true
const SupportsNativeTrash = true

func normalizeError(err error) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return errors.Join(ErrSymlink, err)
	case errors.Is(err, unix.ENOTDIR):
		return errors.Join(ErrNotDirectory, err)
	case errors.Is(err, unix.EXDEV):
		return errors.Join(ErrCrossDevice, err)
	default:
		return err
	}
}

func OpenRoot(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, normalizeError(err)
	}
	return os.NewFile(uintptr(fd), path), nil
}
func OpenDirectoryAt(parent *os.File, name string) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, normalizeError(err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
func OpenFileAt(parent *os.File, name string, flags int, mode fs.FileMode) (*os.File, error) {
	if err := validName(name); err != nil || name == "." {
		return nil, fs.ErrInvalid
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, normalizeError(err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
func MkdirAt(parent *os.File, name string, mode fs.FileMode) error {
	if err := validName(name); err != nil || name == "." {
		return fs.ErrInvalid
	}
	return normalizeError(unix.Mkdirat(int(parent.Fd()), name, uint32(mode.Perm())))
}
func RemoveAt(parent *os.File, name string, directory bool) error {
	if err := validName(name); err != nil || name == "." {
		return fs.ErrInvalid
	}
	flags := 0
	if directory {
		flags = unix.AT_REMOVEDIR
	}
	return normalizeError(unix.Unlinkat(int(parent.Fd()), name, flags))
}
func PreservePermissions(source, destination *os.File) error {
	info, err := source.Stat()
	if err != nil {
		return err
	}
	return destination.Chmod(info.Mode().Perm())
}
func unixMode(raw uint32) fs.FileMode {
	mode := fs.FileMode(raw & 0777)
	switch raw & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	case unix.S_IFREG:
	default:
		mode |= fs.ModeIrregular
	}
	if raw&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if raw&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if raw&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}
func unixIdentity(device, inode uint64) Identity {
	id := Identity{Volume: device}
	binary.LittleEndian.PutUint64(id.FileID[:8], inode)
	return id
}
func CleanRelative(path string) (string, error) {
	if filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", fs.ErrPermission
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", fs.ErrPermission
		}
	}
	return filepath.Clean(path), nil
}
