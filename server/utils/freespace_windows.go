//go:build windows

package utils

import "golang.org/x/sys/windows"

// FreeSpace returns the bytes available to the caller on the volume of path;
// ok is false when it cannot be determined.
func FreeSpace(path string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, nil, nil); err != nil {
		return 0, false
	}
	return avail, true
}
