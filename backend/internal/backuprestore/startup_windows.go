package backuprestore

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// On Windows, offline restore remains unavailable. Hold a shared or exclusive
// byte-range lock during startup so separate processes cannot rotate credentials.
func portableStartupLock(w Workspace, exclusive bool) (func(), error) {
	path, err := windows.UTF16PtrFromString(filepath.Join(w.root, ".nastool-restore.lock"))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 || info.FileSizeLow != 0 || info.FileSizeHigh != 0 {
		windows.CloseHandle(h)
		return nil, ErrState
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err = windows.LockFileEx(h, flags, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, &windows.Overlapped{})
		_ = windows.CloseHandle(h)
	}, nil
}
