package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

func (a *App) registerShortcuts() {
	c := a.win.Canvas()

	add := func(key fyne.KeyName, mod fyne.KeyModifier, fn func()) {
		c.AddShortcut(&desktop.CustomShortcut{KeyName: key, Modifier: mod},
			func(fyne.Shortcut) { fn() })
	}

	add(fyne.KeyF, fyne.KeyModifierControl, func() { a.win.Canvas().Focus(a.search) })
	add(fyne.KeyG, fyne.KeyModifierControl, a.gotoLine)
	add(fyne.KeyS, fyne.KeyModifierControl, a.save)
	add(fyne.KeyE, fyne.KeyModifierControl, a.export)

	// Bare-key shortcuts fire only when the search box is not focused, so typing
	// a filter never triggers them.
	c.SetOnTypedKey(func(ev *fyne.KeyEvent) {
		if a.searchFocused() {
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
