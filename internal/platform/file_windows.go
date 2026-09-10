package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const SupportsAtomicEdit = false
const SupportsNativeTrash = false
const windowsShareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

func OpenRoot(path string) (*os.File, error) {
	// Configured roots are native absolute paths, never tool-relative paths.
	if !filepath.IsAbs(path) {
		return nil, fs.ErrInvalid
	}
	native := filepath.Clean(path)
	if !strings.HasPrefix(native, `\\?\`) {
		if strings.HasPrefix(native, `\\`) {
			native = `\\?\UNC\` + strings.TrimPrefix(native, `\\`)
		} else {
			native = `\\?\` + native
		}
	}
	ptr, err := windows.UTF16PtrFromString(native)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ, windowsShareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, windowsError(err)
	}
	f := os.NewFile(uintptr(handle), path)
	if err = checkWindowsObject(f, true); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// windowsOpenAt accepts one component and always opens the final reparse object
// itself. Callers inspect that pinned handle before reading or truncating it.
func windowsOpenAt(parent *os.File, name string, access, disposition, options, attributes uint32, allowDot bool) (*os.File, error) {
	if name != "." || !allowDot {
		if err := windowsComponent(name); err != nil {
			return nil, err
		}
	}
	native := name
	if native == "." {
		native = ""
	}
	ntname, err := windows.NewNTUnicodeString(native)
	if err != nil {
		return nil, err
	}
	// Respect explicitly case-sensitive directories without guessing identity from
	// spelling. Unknown query failures remain errors, not an insensitive fallback.
	var caseFlags uint32
	err = windows.GetFileInformationByHandleEx(windows.Handle(parent.Fd()), windows.FileCaseSensitiveInfo, (*byte)(unsafe.Pointer(&caseFlags)), 4)
	if err != nil {
		return nil, windowsError(err)
	}
	var objectFlags uint32
	if caseFlags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR == 0 {
		objectFlags = windows.OBJ_CASE_INSENSITIVE
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: ntname, Attributes: objectFlags}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access|windows.SYNCHRONIZE, &oa, &windows.IO_STATUS_BLOCK{}, nil, attributes, windowsShareAll, disposition, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, windowsError(err)
	}
	return os.NewFile(uintptr(handle), name), nil
}

func checkWindowsObject(file *os.File, directory bool) error {
	var attributes windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &attributes)
	runtime.KeepAlive(file)
	if err != nil {
		return windowsError(err)
	}
	if attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrSymlink
	}
	if directory && attributes.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrNotDirectory
	}
	return nil
}

func OpenDirectoryAt(parent *os.File, name string) (*os.File, error) {
	// Open without FILE_DIRECTORY_FILE so a directory reparse point is detected
	// explicitly rather than being misclassified as a missing directory.
	f, err := windowsOpenAt(parent, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, 0, windows.FILE_ATTRIBUTE_NORMAL, true)
	if err != nil {
		return nil, err
	}
	if err = checkWindowsObject(f, true); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func OpenFileAt(parent *os.File, name string, flags int, mode fs.FileMode) (*os.File, error) {
	const accepted = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL | os.O_SYNC | os.O_TRUNC
	if flags&^accepted != 0 || flags&(os.O_WRONLY|os.O_RDWR) == os.O_WRONLY|os.O_RDWR {
		return nil, fs.ErrInvalid
	}
	access := uint32(windows.FILE_GENERIC_READ)
	switch flags & (os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		// The pinned handle must support the reparse/metadata check below even
		// when file contents are write-only. GENERIC_WRITE omits this right.
		access = windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES
	case os.O_RDWR:
		access = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
	}
	if flags&os.O_APPEND != 0 {
		access |= windows.FILE_APPEND_DATA
		access &^= windows.FILE_WRITE_DATA
	}
	disposition := uint32(windows.FILE_OPEN)
	if flags&os.O_CREATE != 0 {
		disposition = windows.FILE_OPEN_IF
		if flags&os.O_EXCL != 0 {
			disposition = windows.FILE_CREATE
		}
	}
	options := uint32(0)
	if flags&os.O_SYNC != 0 {
		options |= windows.FILE_WRITE_THROUGH
	}
	attributes := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if mode.Perm()&0200 == 0 {
		attributes = windows.FILE_ATTRIBUTE_READONLY
	}
	f, err := windowsOpenAt(parent, name, access, disposition, options, attributes, false)
	if err != nil {
		return nil, err
	}
	if err = checkWindowsObject(f, false); err != nil {
		f.Close()
		return nil, err
	}
	if flags&os.O_TRUNC != 0 {
		if err = f.Truncate(0); err != nil {
			f.Close()
			return nil, windowsError(err)
		}
	}
	return f, nil
}

func MkdirAt(parent *os.File, name string, mode fs.FileMode) error {
	f, err := windowsOpenAt(parent, name, windows.FILE_GENERIC_READ, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, false)
	if err != nil {
		return err
	}
	return f.Close()
}

func RemoveAt(parent *os.File, name string, directory bool) error {
	f, err := windowsOpenAt(parent, name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, windows.FILE_ATTRIBUTE_NORMAL, false)
	if err != nil {
		return err
	}
	defer f.Close()
	var attributes windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &attributes); err != nil {
		return windowsError(err)
	}
	isDirectory := attributes.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	reparse := attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	if directory && (!isDirectory || reparse) {
		return ErrNotDirectory
	}
	if !directory && isDirectory && !reparse {
		return fs.ErrInvalid
	}
	// Require immediate name removal; never fall back to deletion pending until
	// the last external handle closes, and never clear a readonly attribute.
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_FORCE_IMAGE_SECTION_CHECK)
	err = windows.NtSetInformationFile(windows.Handle(f.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&flags)), 4, windows.FileDispositionInformationEx)
	runtime.KeepAlive(f)
	if err != nil {
		return windowsError(err)
	}
	return windowsError(f.Close())
}

func PreservePermissions(source, destination *os.File) error { return ErrAtomicReplaceUnsupported }
