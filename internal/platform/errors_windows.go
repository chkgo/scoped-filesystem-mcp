package platform

import (
	"errors"
	"golang.org/x/sys/windows"
	"io/fs"
)

func windowsError(err error) error {
	if err == nil {
		return nil
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		switch status {
		case windows.STATUS_REPARSE_POINT_ENCOUNTERED:
			return ErrSymlink
		case windows.STATUS_NOT_A_DIRECTORY:
			return ErrNotDirectory
		}
		err = status.Errno()
	}
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return errors.Join(fs.ErrNotExist, err)
	case errors.Is(err, windows.ERROR_FILE_EXISTS), errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		return errors.Join(fs.ErrExist, err)
	case errors.Is(err, windows.ERROR_NOT_SAME_DEVICE):
		return errors.Join(ErrCrossDevice, err)
	case errors.Is(err, windows.ERROR_DIRECTORY):
		return errors.Join(ErrNotDirectory, err)
	default:
		return err
	}
}
