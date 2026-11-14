//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// getFreeDiskSpace returns available free bytes on Windows using GetDiskFreeSpaceExW
func getFreeDiskSpace(path string) (uint64, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}

	checkDir := absPath
	for {
		if _, err := os.Stat(checkDir); err == nil {
			break
		}
		parent := filepath.Dir(checkDir)
		if parent == checkDir {
			break
		}
		checkDir = parent
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	lpDirectoryName, err := syscall.UTF16PtrFromString(checkDir)
	if err != nil {
		return 0, err
	}

	var freeBytesAvailable uint64
	var totalNumberOfBytes uint64
	var totalNumberOfFreeBytes uint64

	r1, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(lpDirectoryName)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalNumberOfBytes)),
		uintptr(unsafe.Pointer(&totalNumberOfFreeBytes)),
	)
	if r1 == 0 {
		return 0, callErr
	}
	return freeBytesAvailable, nil
}

// syncDiskBuffers is a no-op on Windows as file handles are closed directly
func syncDiskBuffers() {}
