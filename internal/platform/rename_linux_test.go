package platform

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxInvalidHierarchyIsNotMissingRenameCapability(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "directory", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	child, err := OpenRoot(filepath.Join(root, "directory", "child"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	err = RenameNoReplace(parent, "directory", child, "nested")
	if !errors.Is(err, unix.EINVAL) || errors.Is(err, ErrAtomicRenameUnsupported) {
		t.Fatalf("invalid hierarchy misclassified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "directory", "child")); err != nil {
		t.Fatal(err)
	}
}
