package platform

import (
	"io/fs"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func LstatAt(parent *os.File, name string) (Metadata, error) {
	f, err := windowsOpenAt(parent, name, windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, windows.FILE_ATTRIBUTE_NORMAL, true)
	if err != nil {
		return Metadata{}, err
	}
	defer f.Close()
	return MetadataForFile(f)
}

func MetadataForFile(file *os.File) (Metadata, error) {
	defer runtime.KeepAlive(file)
	handle := windows.Handle(file.Fd())
	var native windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &native); err != nil {
		return Metadata{}, windowsError(err)
	}
	// FILE_ID_INFO carries the complete 128-bit ID and 64-bit volume serial.
	var id struct {
		Volume uint64
		FileID [16]byte
	}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		return Metadata{}, windowsError(err)
	}
	mode := fs.FileMode(0666)
	if native.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		mode = 0444
	}
	if native.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		mode |= fs.ModeDir | 0111
	}
	if native.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		var tag struct {
			Attributes uint32
			Tag        uint32
		}
		if err := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
			return Metadata{}, windowsError(err)
		}
		mode &^= fs.ModeDir
		switch tag.Tag {
		case windows.IO_REPARSE_TAG_SYMLINK, windows.IO_REPARSE_TAG_MOUNT_POINT:
			mode |= fs.ModeSymlink
		default:
			mode |= fs.ModeIrregular
		}
	}
	created := time.Unix(0, native.CreationTime.Nanoseconds())
	return Metadata{Mode: mode, Size: int64(uint64(native.FileSizeHigh)<<32 | uint64(native.FileSizeLow)), ModifiedAt: time.Unix(0, native.LastWriteTime.Nanoseconds()), CreatedAt: &created, Identity: Identity{Volume: id.Volume, FileID: id.FileID}}, nil
}
