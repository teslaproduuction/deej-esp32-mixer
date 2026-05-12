//go:build windows

package audio

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")

	ownPID = uint32(os.Getpid())
)

// GetForegroundProcessName returns the executable basename (lower-case)
// of the process that currently owns the foreground window — e.g.
// "doom.exe" or "chrome.exe". Returns "" if:
//   - there is no foreground window (lock screen, etc.)
//   - the foreground window is the calling process itself (we never
//     want to route the "game" slider onto the mixer's own GUI)
//   - the process cannot be opened (most often a system/elevated proc)
func GetForegroundProcessName() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 || pid == ownPID {
		return ""
	}
	return processName(pid)
}
