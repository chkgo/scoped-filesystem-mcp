package platform

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func RenameNoReplace(fromParent *os.File, from string, toParent *os.File, to string) error {
	if err := windowsComponent(to); err != nil {
		return err
	}
	source, err := windowsOpenAt(fromParent, from, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, windows.FILE_ATTRIBUTE_NORMAL, false)
	if err != nil {
		return err
	}
	defer source.Close()
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	// Native alignment is essential: RootDirectory is pointer aligned, followed
	// by the byte length and UTF-16 filename, with no encoded terminating NUL.
	var info struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [256]uint16
	}
	info.RootDirectory = windows.Handle(toParent.Fd())
	info.FileNameLength = uint32((len(name) - 1) * 2)
	copy(info.FileName[:], name)
	err = windows.NtSetInformationFile(windows.Handle(source.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Offsetof(info.FileName))+info.FileNameLength, windows.FileRenameInformation)
	runtime.KeepAlive(source)
	runtime.KeepAlive(toParent)
	switch err {
	case windows.STATUS_INVALID_INFO_CLASS, windows.STATUS_NOT_SUPPORTED, windows.STATUS_NOT_IMPLEMENTED:
		return errors.Join(ErrAtomicRenameUnsupported, windowsError(err))
	}
	return windowsError(err)
}

// Windows offers no proven handle-relative two-name atomic exchange here.
// ReplaceFileW takes paths and has partial failure states; it is not a fallback.
func Exchange(parent *os.File, first, second string) error { return ErrAtomicReplaceUnsupported }
