//go:build !linux && !darwin && !freebsd && !windows

package utils

// FreeSpace is unknown on this platform.
func FreeSpace(path string) (uint64, bool) {
	return 0, false
}
