package gui

import (
	native "github.com/sqweek/dialog"
)

// The OS-native file pickers come from sqweek/dialog (cgo: GTK on Linux, Cocoa
// on macOS, comdlg32 on Windows). These calls are blocking and must run on the
// UI thread, which is where Fyne invokes menu/button handlers, so they are
// called synchronously from those handlers rather than from a goroutine.

// showOpenFile shows the native open dialog and calls cb with the chosen path.
// A cancelled dialog is a no-op; any other error is surfaced to the user.
func (a *App) showOpenFile(cb func(path string)) {
	path, err := native.File().Title("Open").Load()
	if err != nil {
		if err != native.ErrCancelled {
			a.showError(err)
		}
		return
	}
	cb(path)
}

// showSaveFile shows the native save dialog with an optional default name.
func (a *App) showSaveFile(defaultName string, cb func(path string)) {
	b := native.File().Title("Save")
	if defaultName != "" {
		b = b.SetStartFile(defaultName)
	}
	path, err := b.Save()
	if err != nil {
		if err != native.ErrCancelled {
			a.showError(err)
		}
		return
	}
	cb(path)
}
