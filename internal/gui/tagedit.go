package gui

import (
	"errors"
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// buildColorPicker returns a swatch grid (the presets plus a struck "No
// highlight" option) and getter/setter for the chosen value. The chosen value
// is a "#RRGGBB" hex, or model.NoHighlightColor for the transparent option.
func buildColorPicker(initial string) (obj fyne.CanvasObject, get func() string, set func(string)) {
	chosen := initial
	if chosen == "" {
		chosen = palettePresets[0]
	}

	type option struct {
		hex string
		sw  *swatch
	}
	var opts []*option

	refresh := func() {
		for _, o := range opts {
			o.sw.setSelected(o.hex == chosen)
		}
	}
	grid := container.NewGridWrap(fyne.NewSize(34, 26))
	add := func(hex string, fill color.Color, none bool) {
		o := &option{hex: hex}
		o.sw = newSwatch(fill, nil)
		o.sw.none = none
		o.sw.onTap = func() {
			chosen = hex
			refresh()
		}
		opts = append(opts, o)
		grid.Add(o.sw)
	}
	for _, hex := range palettePresets {
		add(hex, parseHex(hex), false)
	}
	// "No highlight": a neutral square struck through, so a tag can carry meaning
	// without washing its rows.
	add(model.NoHighlightColor, parseHex(noHighlightFill), true)
	refresh()

	body := container.NewVBox(
		grid,
		widget.NewLabelWithStyle("last swatch = no highlight", fyne.TextAlignLeading,
			fyne.TextStyle{Italic: true}),
	)
	get = func() string { return chosen }
	set = func(hex string) {
		if hex == "" {
			hex = palettePresets[0]
		}
		chosen = hex
		refresh()
	}
	return body, get, set
}

// tagSwatchIcon is a small non-interactive swatch for a tag's colour, drawn with
// the "no highlight" strike when the tag paints no row background.
func tagSwatchIcon(hex string) fyne.CanvasObject {
	if hex == model.NoHighlightColor {
		sw := newSwatch(parseHex(noHighlightFill), nil)
		sw.none = true
		return sw
	}
	return colorSquare(hex)
}

// manageTagsDialog lists every palette tag with edit and delete controls. Edit
// and delete need a writable mode; in read-only they are disabled.
func (a *App) manageTagsDialog() {
	if a.sess == nil {
		return
	}
	readOnly := a.sess.Mode() == model.ReadOnly

	list := container.NewVBox()
	var rebuild func()
	rebuild = func() {
		list.Objects = list.Objects[:0]
		defs := a.sess.TagDefs()
		if len(defs) == 0 {
			list.Add(widget.NewLabel("No tags defined yet."))
		}
		for _, def := range defs {
			def := def
			edit := widget.NewButtonWithIcon("", theme.DocumentCreateIcon(), func() {
				a.editTagDialog(def.Name, rebuild)
			})
			del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
				dialog.ShowConfirm("Delete tag",
					"Remove tag \""+def.Name+"\" from the palette and every row it is on?",
					func(ok bool) {
						if !ok {
							return
						}
						if err := a.sess.DeleteTag(def.Name); err != nil {
							a.showError(err)
							return
						}
						a.setTagFilter(def.Name, false)
						rebuild()
						a.afterTagEdit()
					}, a.win)
			})
			if readOnly {
				edit.Disable()
				del.Disable()
			}
			name := container.NewHBox(tagSwatchIcon(def.Color), widget.NewLabel(def.Name))
			list.Add(container.NewBorder(nil, nil, name, container.NewHBox(edit, del)))
		}
		list.Refresh()
	}
	rebuild()

	d := dialog.NewCustom("Manage tags", "Close", container.NewVScroll(list), a.win)
	d.Resize(fyne.NewSize(400, 440))
	d.Show()
}

// editTagDialog edits one tag's name and colour. onDone, if set, runs after a
// successful save (used to refresh the manage-tags list). Recolouring works in
// any mode; renaming needs a writable mode and is reported if it fails.
func (a *App) editTagDialog(name string, onDone func()) {
	if a.sess == nil {
		return
	}
	cur, _ := a.sess.TagColor(name)

	entry := widget.NewEntry()
	entry.SetText(name)
	picker, getColor, _ := buildColorPicker(cur)

	body := container.NewVBox(
		widget.NewLabel("Tag name:"), entry,
		widget.NewLabel("Colour:"), picker,
	)
	d := dialog.NewCustomConfirm("Edit tag", "Save", "Cancel",
		container.NewVScroll(body), func(ok bool) {
			if !ok {
				return
			}
			newName := strings.TrimSpace(entry.Text)
			if newName == "" {
				a.showError(errors.New("tag name cannot be empty"))
				return
			}
			// Colour first (allowed in any mode), then rename if it changed.
			if err := a.sess.DefineTag(name, getColor()); err != nil {
				a.showError(err)
				return
			}
			if newName != name {
				if err := a.sess.RenameTag(name, newName); err != nil {
					a.showError(err)
					return
				}
				// Carry any active tag-filter selection over to the new name.
				if a.tagFilter[name] {
					a.setTagFilter(name, false)
					a.setTagFilter(newName, true)
				}
			}
			a.afterTagEdit()
			if onDone != nil {
				onDone()
			}
		}, a.win)
	d.Resize(fyne.NewSize(380, 340))
	d.Show()
}

// afterTagEdit repaints the grid and the open detail pane so a rename or
// recolour shows immediately.
func (a *App) afterTagEdit() {
	a.refreshTable()
	if m := a.selectedMaster(); m >= 0 {
		a.showDetail(m)
	}
}
