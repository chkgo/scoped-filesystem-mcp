package platform

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsCleanRelativeRejectsAmbiguousPaths(t *testing.T) {
	for _, name := range []string{`C:foo`, `C:\foo`, `\foo`, `\\server\share`, `\\?\C:\foo`, `a:stream`, `a\..\b`, `a/../b`, `CON`, `nul.txt`, `COM1`, `LPT².txt`, `name.`, `name `, "a\x00b", `a?b`, `a|b`, strings.Repeat("a", 2049)} {
		if _, err := CleanRelative(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for input, want := range map[string]string{"": ".", ".": ".", `one\two.txt`: "one/two.txt", `one//./two.txt`: "one/two.txt", `日本語\café.txt`: "日本語/café.txt", `COM12.txt`: "COM12.txt", `LPT123`: "LPT123"} {
		got, err := CleanRelative(input)
		if err != nil || got != want {
			t.Errorf("CleanRelative(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func windowsRoot(t *testing.T) (*os.File, string) {
	t.Helper()
	path := t.TempDir()
	root, err := OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, path
}
func windowsWrite(t *testing.T, parent *os.File, name, value string) {
	t.Helper()
	f, err := OpenFileAt(parent, name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(value); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
func windowsRead(t *testing.T, parent *os.File, name string) string {
	t.Helper()
	f, err := OpenFileAt(parent, name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWindowsNativeNoReplaceAndIdentity(t *testing.T) {
	root, _ := windowsRoot(t)
	windowsWrite(t, root, "source", "one")
	windowsWrite(t, root, "target", "two")
	before, err := LstatAt(root, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err = RenameNoReplace(root, "source", root, "target"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("collision = %v", err)
	}
	if got := windowsRead(t, root, "target"); got != "two" {
		t.Fatal(got)
	}
	if err = RenameNoReplace(root, "source", root, "moved"); err != nil {
		t.Fatal(err)
	}
	after, err := LstatAt(root, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if before.Identity != after.Identity {
		t.Fatal("rename changed identity")
	}
	if after.Size != 3 || after.CreatedAt == nil || after.ModifiedAt.IsZero() {
		t.Fatalf("metadata = %+v", after)
	}
	if _, err = LstatAt(root, "source"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old name = %v", err)
	}
}

func TestWindowsWriteOnlyHandleSupportsMetadataWithoutReadingContent(t *testing.T) {
	root, path := windowsRoot(t)
	file, err := OpenFileAt(root, "write-only", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open write-only file with safety metadata access: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString("content"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	metadata, err := MetadataForFile(file)
	if err != nil || metadata.Size != 7 || !metadata.Mode.IsRegular() {
		t.Fatalf("metadata on write-only handle = %+v, %v", metadata, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Read(make([]byte, 1)); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("write-only handle read = %v, want permission denied", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(path, "write-only")); err != nil || string(content) != "content" {
		t.Fatalf("written content = %q, %v", content, err)
	}
}

func TestWindowsPinnedParentAndFreshDirectoryIteration(t *testing.T) {
	root, path := windowsRoot(t)
	if err := MkdirAt(root, "child", 0700); err != nil {
		t.Fatal(err)
	}
	child, err := OpenDirectoryAt(root, "child")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := os.Rename(filepath.Join(path, "child"), filepath.Join(path, "pinned")); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAt(root, "child", 0700); err != nil {
		t.Fatal(err)
	}
	windowsWrite(t, child, "inside", "safe")
	if _, err := os.Stat(filepath.Join(path, "child", "inside")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replacement parent used: %v", err)
	}
	for i := 0; i < 2; i++ {
		fresh, err := OpenDirectoryAt(child, ".")
		if err != nil {
			t.Fatal(err)
		}
		names, err := fresh.Readdirnames(-1)
		fresh.Close()
		if err != nil || len(names) != 1 || names[0] != "inside" {
			t.Fatalf("iteration %d = %v, %v", i, names, err)
		}
	}
}

func TestWindowsNoFollowAndFinalLinkMutation(t *testing.T) {
	root, path := windowsRoot(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "keep")
	if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	// This is an acceptance test: the runner must enable developer mode or grant
	// symlink creation privilege; skipping would hide missing confinement coverage.
	if err := os.Symlink(outside, filepath.Join(path, "junction")); err != nil {
		t.Fatalf("Windows runner must support symlink creation: %v", err)
	}
	if _, err := OpenDirectoryAt(root, "junction"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("followed directory reparse: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileAt(root, "link", os.O_WRONLY|os.O_TRUNC, 0600); !errors.Is(err, ErrSymlink) {
		t.Fatalf("followed/truncated link: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "outside" {
		t.Fatalf("outside target changed: %q %v", got, err)
	}
	info, err := LstatAt(root, "link")
	if err != nil || info.Mode&fs.ModeSymlink == 0 {
		t.Fatalf("lstat link = %+v %v", info, err)
	}
	if err := RenameNoReplace(root, "link", root, "renamed-link"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAt(root, "renamed-link", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsUnsupportedExchangePreservesBothFiles(t *testing.T) {
	root, _ := windowsRoot(t)
	windowsWrite(t, root, "a", "a")
	windowsWrite(t, root, "b", "b")
	if err := Exchange(root, "a", "b"); !errors.Is(err, ErrAtomicReplaceUnsupported) {
		t.Fatal(err)
	}
	if windowsRead(t, root, "a") != "a" || windowsRead(t, root, "b") != "b" {
		t.Fatal("unsupported exchange mutated files")
	}
}

func TestWindowsDeletionWithOpenHandleAndReadonly(t *testing.T) {
	root, path := windowsRoot(t)
	windowsWrite(t, root, "open", "data")
	held, err := OpenFileAt(root, "open", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := RemoveAt(root, "open", false); err != nil {
		t.Fatal(err)
	}
	if _, err := LstatAt(root, "open"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("successful deletion left name accessible: %v", err)
	}
	data, err := io.ReadAll(held)
	if err != nil || string(data) != "data" {
		t.Fatalf("held object unreadable: %q %v", data, err)
	}
	windowsWrite(t, root, "readonly", "retain")
	if err := os.Chmod(filepath.Join(path, "readonly"), 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(path, "readonly"), 0600)
	if err := RemoveAt(root, "readonly", false); err == nil {
		t.Fatal("removed readonly file")
	}
	if _, err := LstatAt(root, "readonly"); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsLongDevicePrefixIsOrdinaryName(t *testing.T) {
	for _, name := range []string{"COM12.txt", "LPT123", "COM0", "LPT0"} {
		if got, err := CleanRelative(name); err != nil || got != name {
			t.Errorf("rejected ordinary name %q: %q %v", name, got, err)
		}
	}
}

func TestWindowsCollisionRaceKeepsLosingSource(t *testing.T) {
	root, _ := windowsRoot(t)
	windowsWrite(t, root, "one", "first")
	windowsWrite(t, root, "two", "second")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func(name string) { <-start; results <- RenameNoReplace(root, name, root, "winner") }(name)
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("exactly one rename must succeed: %v, %v", first, second)
	}
	loser := first
	if loser == nil {
		loser = second
	}
	if !errors.Is(loser, fs.ErrExist) {
		t.Fatalf("loser error = %v", loser)
	}
	result := windowsRead(t, root, "winner")
	if result == "first" {
		if windowsRead(t, root, "two") != "second" {
			t.Fatal("loser lost")
		}
	} else if result == "second" {
		if windowsRead(t, root, "one") != "first" {
			t.Fatal("loser lost")
		}
	} else {
		t.Fatal(result)
	}
}

func TestWindowsLongPathsAndUnicodeNames(t *testing.T) {
	root, _ := windowsRoot(t)
	parent := root
	for i := 0; i < 5; i++ {
		name := strings.Repeat("a", 80)
		if err := MkdirAt(parent, name, 0700); err != nil {
			t.Fatal(err)
		}
		child, err := OpenDirectoryAt(parent, name)
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		parent = child
	}
	windowsWrite(t, parent, "日本語-café.txt", "data")
	if got := windowsRead(t, parent, "日本語-café.txt"); got != "data" {
		t.Fatal(got)
	}
}

func TestWindowsCaseAliasesHaveSameIdentity(t *testing.T) {
	root, _ := windowsRoot(t)
	windowsWrite(t, root, "MixedCase.txt", "data")
	first, err := LstatAt(root, "MixedCase.txt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LstatAt(root, "mixedcase.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity != second.Identity {
		t.Fatal("case alias changed identity")
	}
	if err := RenameNoReplace(root, "MixedCase.txt", root, "mixedcase.TXT"); err == nil {
		// Windows may treat a same-object case-only rename as successful. It must
		// not duplicate or replace another object, and identity must remain stable.
		after, err := LstatAt(root, "mixedcase.TXT")
		if err != nil || after.Identity != first.Identity {
			t.Fatalf("case rename identity: %+v %v", after, err)
		}
	} else if !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
}
