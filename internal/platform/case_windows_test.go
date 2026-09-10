package platform

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsCaseSensitiveDirectoryPreservesDistinctFiles(t *testing.T) {
	root, path := windowsRoot(t)
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(ptr, windows.FILE_WRITE_ATTRIBUTES, windowsShareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	flags := uint32(windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR)
	err = windows.SetFileInformationByHandle(h, windows.FileCaseSensitiveInfo, (*byte)(unsafe.Pointer(&flags)), 4)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatalf("Windows acceptance runner must support case-sensitive NTFS directories: %v", err)
	}
	windowsWrite(t, root, "a", "lower")
	windowsWrite(t, root, "A", "upper")
	if windowsRead(t, root, "a") != "lower" || windowsRead(t, root, "A") != "upper" {
		t.Fatal("case-sensitive names aliased")
	}
	lower, err := LstatAt(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	upper, err := LstatAt(root, "A")
	if err != nil {
		t.Fatal(err)
	}
	if lower.Identity == upper.Identity {
		t.Fatal("distinct files have same identity")
	}
}

func TestWindowsActualJunctionDoesNotTraverse(t *testing.T) {
	root, path := windowsRoot(t)
	outside := t.TempDir()
	junction := filepath.Join(path, "junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
	if _, err := OpenDirectoryAt(root, "junction"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("junction traversed: %v", err)
	}
	if err := RemoveAt(root, "junction", false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(outside); err != nil || !info.IsDir() {
		t.Fatalf("junction target changed: %v", err)
	}
}

func TestWindowsRenameAcrossVolumesPreservesBothSides(t *testing.T) {
	second := os.Getenv("SCOPEDFS_WINDOWS_SECOND_VOLUME")
	if second == "" {
		t.Skip("set SCOPEDFS_WINDOWS_SECOND_VOLUME to a writable second NTFS volume for cross-volume acceptance")
	}
	destination, err := os.MkdirTemp(second, "scopedfs-volume-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(destination)
	target, err := OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	source, _ := windowsRoot(t)
	sourceInfo, err := MetadataForFile(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := MetadataForFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if sourceInfo.Identity.Volume == targetInfo.Identity.Volume {
		t.Fatal("SCOPEDFS_WINDOWS_SECOND_VOLUME is not a distinct volume")
	}
	windowsWrite(t, source, "keep", "data")
	if err := RenameNoReplace(source, "keep", target, "moved"); !errors.Is(err, ErrCrossDevice) {
		t.Fatalf("cross-volume error = %v", err)
	}
	if windowsRead(t, source, "keep") != "data" {
		t.Fatal("source lost")
	}
	if _, err := LstatAt(target, "moved"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("destination appeared: %v", err)
	}
}
