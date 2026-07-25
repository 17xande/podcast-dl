//go:build windows

package main

import (
	"syscall"
	"time"
)

// setCreationTime sets a file's NTFS creation time ("Date Created" in
// Explorer), which os.Chtimes cannot touch since it only sets mtime/atime.
func setCreationTime(path string, t time.Time) error {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}

	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)

	creationTime := syscall.NsecToFiletime(t.UnixNano())
	return syscall.SetFileTime(handle, &creationTime, nil, nil)
}
