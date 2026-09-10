package platform

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNeverReplacesExistingEntry(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{"source": "original", "target": "concurrent"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := RenameNoReplace(parent, "source", parent, "target"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision: %v", err)
	}
	for name, want := range map[string]string{"source": "original", "target": "concurrent"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}

func TestParentHandleSurvivesRename(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "original")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := OpenRoot(original)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Rename(original, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFileAt(parent, "note", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "pinned"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(original, "note")); !os.IsNotExist(err) {
		t.Fatalf("replacement directory accessed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, "moved", "note"))
	if err != nil || string(got) != "pinned" {
		t.Fatalf("pinned write: %q %v", got, err)
	}
}

func TestNativeHelpersRejectMultipleComponents(t *testing.T) {
	parent, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for _, name := range []string{"../escape", "child/file", "..", ""} {
		if f, err := OpenFileAt(parent, name, os.O_CREATE|os.O_WRONLY, 0600); err == nil {
			f.Close()
			t.Fatalf("accepted %q", name)
		}
	}
}
