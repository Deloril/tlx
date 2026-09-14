//go:build windows

package gui

import (
	"syscall"

	"fyne.io/fyne/v2/driver"
)

// SetWindowPos with HWND_TOPMOST / HWND_NOTOPMOST pins or releases a window. No
// cgo needed — user32 is reachable via syscall.
var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSetWindowPos = user32.NewProc("SetWindowPos")
)

const (
	hwndTopmost   = ^uintptr(0) // (HWND)-1
	hwndNoTopmost = ^uintptr(1) // (HWND)-2
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010
)

func nativeSetOnTop(ctx any, on bool) {
	c, ok := ctx.(driver.WindowsWindowContext)
	if !ok || c.HWND == 0 {
		return
	}
	insertAfter := hwndNoTopmost
	if on {
		insertAfter = hwndTopmost
	}
	procSetWindowPos.Call(c.HWND, insertAfter, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
}
