//go:build windows

package main

import (
	"context"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidTaskbarList = guid{0x56FDF344, 0xFD6D, 0x11d0, [8]byte{0x95, 0x8A, 0x00, 0x60, 0x97, 0xC9, 0xA0, 0x90}}
	iidITaskbarList3 = guid{0xEA1AFB91, 0x9E28, 0x4B86, [8]byte{0x90, 0xE9, 0x9E, 0x9F, 0x8A, 0x5E, 0xEF, 0xAF}}
)

const (
	tbpfNoProgress    = 0x00000000
	tbpfIndeterminate = 0x00000001
	tbpfNormal        = 0x00000002
	tbpfError         = 0x00000004
	tbpfPaused        = 0x00000008

	clsctxInprocServer = 0x1
)

var (
	modOle32             = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = modOle32.NewProc("CoInitializeEx")
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")

	modUser32           = syscall.NewLazyDLL("user32.dll")
	procEnumWindows     = modUser32.NewProc("EnumWindows")
	procGetWindowTextW  = modUser32.NewProc("GetWindowTextW")
	procGetClassNameW   = modUser32.NewProc("GetClassNameW")
	procIsWindowVisible = modUser32.NewProc("IsWindowVisible")
	procIsWindow        = modUser32.NewProc("IsWindow")

	modShell32                                  = syscall.NewLazyDLL("shell32.dll")
	procSetCurrentProcessExplicitAppUserModelID = modShell32.NewProc("SetCurrentProcessExplicitAppUserModelID")

	modKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecutionState = modKernel32.NewProc("SetThreadExecutionState")
)

const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
)

// startWindowsIdleInhibitor uses SetThreadExecutionState to prevent system sleep during extraction.
func startWindowsIdleInhibitor(ctx context.Context) {
	go func() {
		_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired | esDisplayRequired))
		<-ctx.Done()
		_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous))
	}()
}

func findDirectCrateWindow() uintptr {
	var targetHWND uintptr
	cb := syscall.NewCallback(func(hwnd, lParam uintptr) uintptr {
		ret, _, _ := procIsWindowVisible.Call(hwnd)
		if ret == 0 {
			return 1
		}
		var textBuf [256]uint16
		procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&textBuf[0])), uintptr(len(textBuf)))
		title := syscall.UTF16ToString(textBuf[:])

		var classBuf [256]uint16
		procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&classBuf[0])), uintptr(len(classBuf)))
		className := syscall.UTF16ToString(classBuf[:])

		if strings.Contains(strings.ToLower(title), "directcrate") || strings.Contains(strings.ToLower(className), "directcrate") {
			targetHWND = hwnd
			return 0 // Stop enumeration
		}
		return 1 // Continue enumeration
	})
	procEnumWindows.Call(cb, 0)
	return targetHWND
}

// startWindowsTaskbarProgressMonitor begins a 1-second ticker polling active extraction progress
// and updating the Windows taskbar progress bar via ITaskbarList3 on the standalone DirectCrate window.
func startWindowsTaskbarProgressMonitor() {
	appID, _ := syscall.UTF16PtrFromString("DirectCrate.App")
	_, _, _ = procSetCurrentProcessExplicitAppUserModelID.Call(uintptr(unsafe.Pointer(appID)))

	go func() {
		// Initialize COM on the background monitor thread
		_, _, _ = procCoInitializeEx.Call(0, 0x2) // COINIT_APARTMENTTHREADED

		var pTaskbarList uintptr
		hr, _, _ := procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&clsidTaskbarList)),
			0,
			clsctxInprocServer,
			uintptr(unsafe.Pointer(&iidITaskbarList3)),
			uintptr(unsafe.Pointer(&pTaskbarList)),
		)
		if hr != 0 || pTaskbarList == 0 {
			return
		}

		vtable := *(*uintptr)(unsafe.Pointer(pTaskbarList))
		// HrInit is at index 3 in ITaskbarList vtable
		hrInit := *(*uintptr)(unsafe.Pointer(vtable + 3*unsafe.Sizeof(uintptr(0))))
		_, _, _ = syscall.SyscallN(hrInit, pTaskbarList)

		// SetProgressValue is at index 9, SetProgressState is at index 10 in ITaskbarList3 vtable
		setProgressValue := *(*uintptr)(unsafe.Pointer(vtable + 9*unsafe.Sizeof(uintptr(0))))
		setProgressState := *(*uintptr)(unsafe.Pointer(vtable + 10*unsafe.Sizeof(uintptr(0))))

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var lastHWND uintptr
		wasActive := false

		for range ticker.C {
			sum := getActiveProgressSummary()

			if sum.Active && sum.LiveProgressNotifications {
				hwnd := lastHWND
				if hwnd == 0 {
					hwnd = findDirectCrateWindow()
					lastHWND = hwnd
				} else {
					ret, _, _ := procIsWindow.Call(hwnd)
					if ret == 0 {
						hwnd = findDirectCrateWindow()
						lastHWND = hwnd
					}
				}

				if hwnd != 0 {
					wasActive = true
					state := uintptr(tbpfNormal)
					if sum.Paused {
						state = uintptr(tbpfPaused)
					}
					_, _, _ = syscall.SyscallN(setProgressState, pTaskbarList, hwnd, state)
					_, _, _ = syscall.SyscallN(setProgressValue, pTaskbarList, hwnd, uintptr(uint64(sum.Percent)), uintptr(100))
				}
			} else if wasActive {
				wasActive = false
				if lastHWND != 0 {
					ret, _, _ := procIsWindow.Call(lastHWND)
					if ret != 0 {
						_, _, _ = syscall.SyscallN(setProgressState, pTaskbarList, lastHWND, uintptr(tbpfNoProgress))
					}
				}
			}
		}
	}()
}

