//go:build !windows

package main

import "context"

// startWindowsTaskbarProgressMonitor is a no-op on non-Windows platforms.
func startWindowsTaskbarProgressMonitor() {
}

// startWindowsIdleInhibitor is a no-op on non-Windows platforms.
func startWindowsIdleInhibitor(ctx context.Context) {
}

