//go:build linux || darwin || freebsd

package utils

import "golang.org/x/sys/unix"

// FreeSpace returns the bytes available to the process on the file system of
// path; ok is false when it cannot be determined.
func FreeSpace(path string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	// Bavail is signed on some platforms
	avail := int64(st.Bavail)
	if avail < 0 {
		avail = 0
	}
	return uint64(avail) * uint64(st.Bsize), true
}
