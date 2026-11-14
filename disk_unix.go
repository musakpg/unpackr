//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// getFreeDiskSpace returns available free bytes on Unix-like systems (Linux, Android, macOS)
func getFreeDiskSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
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

	if err := syscall.Statfs(checkDir, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// syncDiskBuffers flushes OS filesystem write caches to physical storage
func syncDiskBuffers() {
	syscall.Sync()
}
