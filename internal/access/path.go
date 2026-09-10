package access

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"

	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

// Intent specifies whether the requested path must already exist.
type Intent uint8

const (
	Existing Intent = iota
	ParentExisting
)

// Path identifies both the configured display path and canonical path.
type Path struct {
	RootName  string
	Relative  string
	Display   string
	Canonical string

	root           *root
	secureRelative string
}

// Resolve validates a root-relative request and returns its authorized paths.
func (m *Manager) Resolve(rootName, relative string, op config.Operation, intent Intent) (Path, error) {
	r, exists := m.roots[rootName]
	if !exists {
		return Path{}, accessError("unknown_root", nil)
	}
	if !r.rule.Allows(op) {
		return Path{}, accessError("operation_not_allowed", nil)
	}

	cleanRelative, err := cleanRelative(relative)
	if err != nil {
		return Path{}, accessError("path_outside_root", err)
	}
	if err := r.verifyTarget(); err != nil {
		return Path{}, err
	}

	canonical, exists, err := resolveComponents(r.canonical, cleanRelative)
	if err != nil {
		return Path{}, err
	}
	if !contained(r.canonical, canonical) {
		return Path{}, accessError("symlink_escape", nil)
	}
	if intent == Existing && !exists {
		return Path{}, accessError("path_not_found", nil)
	}
	if intent != Existing && intent != ParentExisting {
		return Path{}, accessError("invalid_intent", nil)
	}
	secureRelative, err := filepath.Rel(r.canonical, canonical)
	if err != nil || !contained(r.canonical, canonical) {
		return Path{}, accessError("symlink_escape", err)
	}

	return Path{
		RootName:       rootName,
		Relative:       cleanRelative,
		Display:        filepath.Join(r.configured, cleanRelative),
		Canonical:      canonical,
		root:           r,
		secureRelative: secureRelative,
	}, nil
}

func (r *root) verifyTarget() error {
	current, err := filepath.EvalSymlinks(r.configured)
	if err != nil {
		return accessError("root_target_changed", err)
	}
	file, err := platform.OpenRoot(current)
	if err != nil {
		return accessError("root_target_changed", err)
	}
	defer file.Close()
	metadata, err := platform.MetadataForFile(file)
	if err != nil || r.identity != metadata.Identity {
		return accessError("root_target_changed", err)
	}
	return nil
}

// Open opens an existing path, or creates its final component when its parent
// exists, using descriptor-relative no-follow operations below the pinned root.
func (p Path) Open(flags int, perm fs.FileMode) (*os.File, error) {
	return p.open(flags, perm, false)
}

// OpenDirectory rejects non-directory entries in the native open itself.
func (p Path) OpenDirectory() (*os.File, error) {
	return p.open(os.O_RDONLY, 0, true)
}

func (p Path) open(flags int, perm fs.FileMode, directory bool) (*os.File, error) {
	parent, remaining, err := p.OpenParent()
	if err != nil {
		return nil, err
	}
	if len(remaining) == 0 {
		return parent, nil
	}
	defer parent.Close()
	if len(remaining) != 1 {
		return nil, accessError("path_not_found", nil)
	}
	// O_NONBLOCK prevents an attacker-controlled FIFO or device from hanging the
	// server before callers can validate the descriptor type with fstat.
	var file *os.File
	if directory {
		file, err = platform.OpenDirectoryAt(parent, remaining[0])
	} else {
		file, err = platform.OpenFileAt(parent, remaining[0], flags, perm)
	}
	if err != nil {
		return nil, openAccessError(err)
	}
	return file, nil
}

// CanonicalRelative is the authorized root-relative path after resolving
// in-root symlinks. It is safe to use when reporting descriptor-relative
// recovery artifacts created beside the resolved target.
func (p Path) CanonicalRelative() string { return filepath.ToSlash(p.secureRelative) }

// OpenParent returns the nearest existing parent directory pinned below the
// configured root and the remaining clean path components below that directory.
// Callers that create missing components must use descriptor-relative, no-follow
// operations on the returned directory one component at a time.
func (p Path) OpenParent() (*os.File, []string, error) {
	return p.openParent(p.secureRelative)
}

// OpenRequestedParent opens the parent addressed by the original relative
// request. It is for destructive operations that must act on a final symlink
// itself and reject symlinks in an intermediate path component.
func (p Path) OpenRequestedParent() (*os.File, []string, error) {
	return p.openParent(p.Relative)
}

func (p Path) openParent(relative string) (*os.File, []string, error) {
	if p.root == nil || p.root.file == nil {
		return nil, nil, accessError("filesystem_unavailable", nil)
	}
	if err := p.root.verifyTarget(); err != nil {
		return nil, nil, err
	}
	// Opening "." below the pinned root creates a distinct open-file
	// description. unix.Dup would share the directory read offset, causing one
	// listing or search to make a later root-level listing appear empty.
	current, err := platform.OpenDirectoryAt(p.root.file, ".")
	if err != nil {
		return nil, nil, openAccessError(err)
	}
	if relative == "." {
		return current, nil, nil
	}

	parts := strings.Split(filepath.FromSlash(relative), string(os.PathSeparator))
	for i, part := range parts[:len(parts)-1] {
		next, err := platform.OpenDirectoryAt(current, part)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return current, append([]string(nil), parts[i:]...), nil
			}
			current.Close()
			return nil, nil, openAccessError(err)
		}
		current.Close()
		current = next
	}
	return current, []string{parts[len(parts)-1]}, nil
}

func openAccessError(err error) error {
	if errors.Is(err, platform.ErrSymlink) {
		return accessError("symlink_escape", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		return accessError("path_not_found", err)
	}
	return accessError("filesystem_unavailable", err)
}

func cleanRelative(relative string) (string, error) { return platform.CleanRelative(relative) }

// resolveComponents follows each existing symlink. Once it encounters a
// missing component, the remaining clean components are appended unchanged.
func resolveComponents(rootPath, relative string) (string, bool, error) {
	if relative == "." {
		return rootPath, true, nil
	}

	current := rootPath
	parts := strings.Split(filepath.FromSlash(relative), string(os.PathSeparator))
	for i, part := range parts {
		next := filepath.Join(current, part)
		_, err := os.Lstat(next)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return filepath.Join(append([]string{current}, parts[i:]...)...), false, nil
			}
			return "", false, accessError("filesystem_unavailable", err)
		}

		resolved, err := filepath.EvalSymlinks(next)
		if err != nil {
			return "", false, accessError("symlink_escape", err)
		}
		current = resolved
	}
	return current, true, nil
}

func contained(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
