package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

func (a *App) registerShortcuts() {
	c := a.win.Canvas()

	add := func(key fyne.KeyName, mod fyne.KeyModifier, fn func()) {
		c.AddShortcut(&desktop.CustomShortcut{KeyName: key, Modifier: mod},
			func(fyne.Shortcut) {
				if a.idx == nil { // no file loaded yet
					return
				}
				fn()
			})
	}

	add(fyne.KeyF, fyne.KeyModifierControl, func() { a.win.Canvas().Focus(a.search) })
	add(fyne.KeyG, fyne.KeyModifierControl, a.gotoLine)
	add(fyne.KeyS, fyne.KeyModifierControl, a.save)
	add(fyne.KeyE, fyne.KeyModifierControl, a.export)
	add(fyne.KeyB, fyne.KeyModifierControl, a.toggleSidebar)

	// Bare-key shortcuts fire only when no text field is focused, so typing a
	// filter or editing a cell never triggers them.
	c.SetOnTypedKey(func(ev *fyne.KeyEvent) {
		if a.idx == nil || a.typingFocused() {
			return
		}
		switch ev.Name {
		case fyne.KeyF3:
			a.findNext()
		case fyne.KeyEscape:
			a.clearFilter()
		case fyne.KeyT:
			a.tagSelected()
		case fyne.KeyC:
			a.commentSelected()
		case fyne.KeyM:
			a.cycleMode()
		}
	})
}

func (a *App) searchFocused() bool {
	return a.win.Canvas().Focused() == a.search
}

// typingFocused reports whether a text field currently has focus, so bare-key
// shortcuts can stand down while the user is typing.
func (a *App) typingFocused() bool {
	if a.editing {
		return true
	}
	switch a.win.Canvas().Focused().(type) {
	case *inlineEntry, *widget.Entry, *widget.SelectEntry:
		return true
	}
	return false
}
