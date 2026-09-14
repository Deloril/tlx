package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

// Chrome for the cell pop-out windows. They are borderless (no OS title bar or
// min/max/close buttons); the title, an always-on-top toggle and a close button
// live in the content instead, and a dragBar lets the strip move the window.

// newPopout makes a borderless window for pop-outs. Falls back to a normal
// decorated window if the driver is not a desktop one (so nothing breaks on an
// unexpected backend).
func (a *App) newPopout(title string) fyne.Window {
	if dd, ok := a.fyne.Driver().(desktop.Driver); ok {
		w := dd.CreateSplashWindow()
		w.SetTitle(title)
		return w
	}
	return a.fyne.NewWindow(title)
}

// beginWindowMove asks the OS to start an interactive move of w, as if its title
// bar had been grabbed. It runs on a primary press on a dragBar, so a borderless
// pop-out can still be dragged. No-op where the platform is unsupported (e.g.
// Wayland), matching applyAlwaysOnTop.
func beginWindowMove(w fyne.Window) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return
	}
	nw.RunNative(func(ctx any) { nativeBeginMove(ctx) })
}

// dragBar starts a window move when pressed on empty space. Children that handle
// their own mouse events (the buttons) sit on top and are unaffected; a press on
// the bar's label or padding falls through to here, since neither a Label nor a
// Container consumes mouse events.
type dragBar struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onDown  func()
}

func newDragBar(content fyne.CanvasObject, onDown func()) *dragBar {
	b := &dragBar{content: content, onDown: onDown}
	b.ExtendBaseWidget(b)
	return b
}

func (b *dragBar) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(b.content)
}

func (b *dragBar) MouseDown(e *desktop.MouseEvent) {
	if e.Button == desktop.MouseButtonPrimary && b.onDown != nil {
		b.onDown()
	}
}

func (b *dragBar) MouseUp(*desktop.MouseEvent) {}
