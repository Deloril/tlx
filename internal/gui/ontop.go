package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Always-on-top for the secondary windows (cell pop-outs and floated dock
// panels). Fyne has no portable API for this, so it goes through the window's
// native handle (driver.NativeWindow.RunNative); the per-platform work lives in
// ontop_<goos>.go, with a no-op fallback for anything unhandled.

// applyAlwaysOnTop asks the OS to pin w above other windows, or release it. It is
// a no-op if the window has no native backing or the platform is unsupported.
func applyAlwaysOnTop(w fyne.Window, on bool) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return
	}
	nw.RunNative(func(ctx any) { nativeSetOnTop(ctx, on) })
}

// newAlwaysOnTopButton returns an icon toggle that pins w on top. The caller owns
// the state bool so it survives content rebuilds; the OS window keeps its level
// across a SetContent, so only fresh windows start unpinned.
func newAlwaysOnTopButton(w fyne.Window, state *bool) *widget.Button {
	var btn *widget.Button
	reflect := func() {
		if *state {
			btn.Importance = widget.HighImportance
		} else {
			btn.Importance = widget.LowImportance
		}
		btn.Refresh()
	}
	btn = widget.NewButtonWithIcon("", theme.MoveUpIcon(), func() {
		*state = !*state
		applyAlwaysOnTop(w, *state)
		reflect()
	})
	reflect()
	return btn
}
