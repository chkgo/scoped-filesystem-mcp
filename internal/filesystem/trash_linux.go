package filesystem

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"
)

// trashNative implements the freedesktop home Trash. Every destination
// component is opened without following links, and mutations use pinned
// directories. Other volumes are deliberately unsupported: there is no
// copy/delete or permanent-delete fallback.
func (s *Service) trashNative(ctx context.Context, root, path string) (MutationResult, error) {
	fail := func(err error) (MutationResult, error) {
		return MutationResult{}, MapError(root, path, config.OpTrash, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	target, err := s.access.Resolve(root, path, config.OpTrash, access.Existing)
	if err != nil {
		return fail(err)
	}
	if target.Relative == "." {
		return MutationResult{}, newError(root, path, config.OpTrash, "path_outside_root", nil)
	}
	parent, name, err := mutationParent(target)
	if err != nil {
		return fail(err)
	}
	defer parent.Close()
	if _, err := statAt(parent, name); err != nil {
		return fail(err)
	}
	trash, err := s.openLinuxTrash()
	if err != nil {
		return fail(err)
	}
	defer trash.close()

	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		candidate := linuxTrashName(name, number)
		infoName := candidate + ".trashinfo"
		metadata, err := platform.OpenFileAt(trash.info, infoName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		identity, statErr := platform.MetadataForFile(metadata)
		if statErr != nil {
			metadata.Close()
			// Without its identity, this attempt cannot safely unlink the name.
			return fail(statErr)
		}
		cleanup := func() error {
			current, err := platform.LstatAt(trash.info, infoName)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if current.Identity != identity.Identity {
				return syscall.ESTALE
			}
			return platform.RemoveAt(trash.info, infoName, false)
		}
		cleanupFailure := func(cause error) (MutationResult, error) {
			if cleanupErr := cleanup(); cleanupErr != nil {
				return MutationResult{}, newError(root, path, config.OpTrash, "trash_metadata_cleanup_failed", errors.Join(cause, cleanupErr))
			}
			return fail(cause)
		}
		err = writeLinuxTrashInfo(metadata, target.Display, time.Now())
		closeErr := metadata.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = trash.info.Sync()
		}
		if err != nil {
			return cleanupFailure(err)
		}
		if s.beforeTrashRename != nil {
			if err := s.beforeTrashRename(); err != nil {
				return cleanupFailure(err)
			}
		}
		if err := ctx.Err(); err != nil {
			return cleanupFailure(err)
		}
		beforeMove, err := platform.LstatAt(parent, name)
		if err != nil {
			return cleanupFailure(err)
		}
		if err := s.rename(parent, name, trash.files, candidate); err != nil {
			// A failed remote-filesystem rename can have already happened. Only
			// known pre-mutation errors permit deleting restoration metadata.
			if !linuxTrashMoveDefinitelyFailed(err) || !linuxTrashSourceRemains(parent, name, trash.files, candidate, beforeMove.Identity) {
				return MutationResult{}, newError(root, path, config.OpTrash, "trash_outcome_uncertain", err)
			}
			if cleanupErr := cleanup(); cleanupErr != nil {
				return MutationResult{}, newError(root, path, config.OpTrash, "trash_metadata_cleanup_failed", errors.Join(err, cleanupErr))
			}
			if os.IsExist(err) {
				continue
			}
			if errors.Is(err, platform.ErrCrossDevice) || errors.Is(err, syscall.EXDEV) {
				return MutationResult{}, newError(root, path, config.OpTrash, "cross_filesystem_move_unsupported", err)
			}
			return fail(err)
		}
		return MutationResult{Root: root, Path: path}, nil
	}
}

type linuxTrash struct {
	base, files, info *os.File
}

func (t *linuxTrash) close() {
	for _, directory := range []*os.File{t.info, t.files, t.base} {
		if directory != nil {
			directory.Close()
		}
	}
}

func (s *Service) openLinuxTrash() (*linuxTrash, error) {
	dataPath := os.Getenv("XDG_DATA_HOME")
	// The XDG specification requires absolute paths; relative values are
	// ignored, exactly like an unset variable.
	if !filepath.IsAbs(dataPath) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(home) {
			return nil, fs.ErrPermission
		}
		dataPath = filepath.Join(home, ".local", "share")
	}
	dataPath = filepath.Clean(dataPath)
	data, err := openLinuxTrashData(dataPath, true)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	trash := &linuxTrash{}
	fail := func(err error) (*linuxTrash, error) { trash.close(); return nil, err }
	trash.base, err = openLinuxPrivateDirectory(data, "Trash", true)
	if err != nil {
		return fail(err)
	}
	trash.files, err = openLinuxPrivateDirectory(trash.base, "files", true)
	if err != nil {
		return fail(err)
	}
	trash.info, err = openLinuxPrivateDirectory(trash.base, "info", true)
	if err != nil {
		return fail(err)
	}
	if s.beforeTrashValidate != nil {
		if err := s.beforeTrashValidate(); err != nil {
			return fail(err)
		}
	}
	// Reopen without creating anything and check every pinned destination.
	// Later renames still use the original handles if a name is retargeted.
	currentData, err := openLinuxTrashData(dataPath, false)
	if err != nil {
		return fail(err)
	}
	defer currentData.Close()
	currentBase, err := openLinuxPrivateDirectory(currentData, "Trash", false)
	if err != nil {
		return fail(err)
	}
	defer currentBase.Close()
	currentFiles, err := openLinuxPrivateDirectory(currentBase, "files", false)
	if err != nil {
		return fail(err)
	}
	defer currentFiles.Close()
	currentInfo, err := openLinuxPrivateDirectory(currentBase, "info", false)
	if err != nil {
		return fail(err)
	}
	defer currentInfo.Close()
	for _, pair := range [][2]*os.File{{data, currentData}, {trash.base, currentBase}, {trash.files, currentFiles}, {trash.info, currentInfo}} {
		before, err := platform.MetadataForFile(pair[0])
		if err != nil {
			return fail(err)
		}
		after, err := platform.MetadataForFile(pair[1])
		if err != nil {
			return fail(err)
		}
		if before.Identity != after.Identity {
			return fail(syscall.ESTALE)
		}
	}
	return trash, nil
}

// openLinuxTrashData walks from a pinned filesystem root, rejecting symlinks
// and directories that another user can replace. Root-owned sticky ancestors
// (not the data directory itself) permit conventional private /tmp test roots.
func openLinuxTrashData(path string, create bool) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fs.ErrPermission
	}
	current, err := platform.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			current.Close()
			return nil, fs.ErrPermission
		}
		if err := validateLinuxTrashDirectory(current, false); err != nil {
			current.Close()
			return nil, err
		}
		next, err := platform.OpenDirectoryAt(current, component)
		if os.IsNotExist(err) && create {
			err = platform.MkdirAt(current, component, 0o700)
			if err == nil || os.IsExist(err) {
				next, err = platform.OpenDirectoryAt(current, component)
			}
		}
		current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	if err := validateLinuxTrashDirectory(current, true); err != nil {
		current.Close()
		return nil, err
	}
	return current, nil
}

func openLinuxPrivateDirectory(parent *os.File, name string, create bool) (*os.File, error) {
	directory, err := platform.OpenDirectoryAt(parent, name)
	if os.IsNotExist(err) && create {
		err = platform.MkdirAt(parent, name, 0o700)
		if err == nil || os.IsExist(err) {
			directory, err = platform.OpenDirectoryAt(parent, name)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := validateLinuxTrashDirectory(directory, true); err != nil {
		directory.Close()
		return nil, err
	}
	info, err := directory.Stat()
	if err != nil {
		directory.Close()
		return nil, err
	}
	if info.Mode().Perm() != 0o700 {
		directory.Close()
		return nil, fs.ErrPermission
	}
	return directory, nil
}

func validateLinuxTrashDirectory(directory *os.File, userOwned bool) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return fs.ErrPermission
	}
	uid := uint32(os.Geteuid())
	if userOwned {
		if stat.Uid != uid || info.Mode().Perm()&0o022 != 0 {
			return fs.ErrPermission
		}
	} else {
		if stat.Uid != uid && stat.Uid != 0 {
			return fs.ErrPermission
		}
		if info.Mode().Perm()&0o022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return fs.ErrPermission
		}
	}
	return nil
}

func writeLinuxTrashInfo(file *os.File, original string, deletion time.Time) error {
	// Encode bytes, including percent, line breaks and non-ASCII names. Slash
	// remains a path separator; the result always occupies exactly one line.
	const hex = "0123456789ABCDEF"
	var escaped strings.Builder
	for i := 0; i < len(original); i++ {
		c := original[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/-._~", rune(c)) {
			escaped.WriteByte(c)
		} else {
			escaped.WriteByte('%')
			escaped.WriteByte(hex[c>>4])
			escaped.WriteByte(hex[c&15])
		}
	}
	if _, err := fmt.Fprintf(file, "[Trash Info]\nPath=%s\nDeletionDate=%s\n", escaped.String(), deletion.Local().Format("2006-01-02T15:04:05")); err != nil {
		return err
	}
	return file.Sync()
}

func linuxTrashName(name string, number int) string {
	candidate := trashName(name, number)
	// Reserve room for the .trashinfo suffix on NAME_MAX=255 filesystems.
	const maximum = 255 - len(".trashinfo")
	if len(candidate) <= maximum {
		return candidate
	}
	hash := sha256.Sum256([]byte(candidate))
	return candidate[:maximum-17] + "-" + fmt.Sprintf("%x", hash[:8])
}

func linuxTrashMoveDefinitelyFailed(err error) bool {
	if errors.Is(err, platform.ErrCrossDevice) || errors.Is(err, platform.ErrAtomicRenameUnsupported) {
		return true
	}
	for _, definite := range []error{syscall.EEXIST, syscall.EXDEV, syscall.EACCES, syscall.EPERM, syscall.ENOENT, syscall.ENOTDIR, syscall.EISDIR, syscall.ENOTEMPTY, syscall.EINVAL, syscall.EROFS, syscall.ENOSPC, syscall.EDQUOT, syscall.ENAMETOOLONG, syscall.ELOOP, syscall.ENOSYS, syscall.EOPNOTSUPP} {
		if errors.Is(err, definite) {
			return true
		}
	}
	return false
}

// Even an otherwise ordinary errno can follow a completed rename on a remote
// filesystem (for example an RPC retry returning ENOENT). Confirm that the
// inspected source object remains and was not published under the Trash name
// before deciding its restoration metadata is disposable.
func linuxTrashSourceRemains(parent *os.File, name string, files *os.File, candidate string, identity platform.Identity) bool {
	source, err := platform.LstatAt(parent, name)
	if err != nil || source.Identity != identity {
		return false
	}
	destination, err := platform.LstatAt(files, candidate)
	return os.IsNotExist(err) || err == nil && destination.Identity != identity
}
