package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
)

// wmClass is the application id used for window-manager matching. It equals the
// Fyne UniqueID (the Wayland app_id) and the installed desktop file's basename
// and StartupWMClass, so the running window resolves to that .desktop and shows
// its icon in the taskbar.
const wmClass = "nz.timeline.explorer"

// applyWMClass sets the window's X11 WM_CLASS to wmClass. Fyne/GLFW never put
// the app id on the window, so a desktop environment can't tie the window to
// packaging/linux/nz.timeline.explorer.desktop and shows a generic icon. It is
// a no-op on Wayland (app_id already comes from the UniqueID) and on platforms
// without an implementation.
func applyWMClass(w fyne.Window) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return
	}
	nw.RunNative(func(ctx any) { nativeSetWMClass(ctx, wmClass) })
}
