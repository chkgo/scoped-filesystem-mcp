package platform

import (
	"errors"
	"io/fs"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsNativeErrorClasses(t *testing.T) {
	for _, test := range []struct{ native, errorClass error }{
		{windows.STATUS_OBJECT_NAME_NOT_FOUND, fs.ErrNotExist},
		{windows.STATUS_OBJECT_PATH_NOT_FOUND, fs.ErrNotExist},
		{windows.STATUS_OBJECT_NAME_COLLISION, fs.ErrExist},
		{windows.STATUS_NOT_SAME_DEVICE, ErrCrossDevice},
		{windows.STATUS_REPARSE_POINT_ENCOUNTERED, ErrSymlink},
		{windows.STATUS_NOT_A_DIRECTORY, ErrNotDirectory},
	} {
		if got := windowsError(test.native); !errors.Is(got, test.errorClass) {
			t.Errorf("%v mapped to %v; want %v", test.native, got, test.errorClass)
		}
	}
	// Ordinary denial must not be promoted to an unsupported capability.
	denied := windowsError(windows.STATUS_ACCESS_DENIED)
	if !errors.Is(denied, windows.ERROR_ACCESS_DENIED) || errors.Is(denied, ErrAtomicRenameUnsupported) {
		t.Fatalf("access denial misclassified: %v", denied)
	}
}
