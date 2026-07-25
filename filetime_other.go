//go:build !windows

package main

import "time"

// setCreationTime is a no-op on platforms where a file's filesystem
// creation time can't be changed after the fact (e.g. Linux ext4/xfs have
// no syscall to set birth time). os.Chtimes already covers mtime/atime,
// which is the closest equivalent these platforms expose.
func setCreationTime(path string, t time.Time) error {
	return nil
}
