package gui

import (
	"fmt"
	"sort"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// The tag filter is a drop-down in the filter window: a checkbox per palette
// tag. Ticking one or more keeps rows carrying any of them (OR). It composes
// with the query and column conditions like every other filter part.

// selectedTagNames returns the ticked tag names in a stable order, dropping
// unticked ones. Empty means the tag filter imposes no restriction.
func (a *App) selectedTagNames() []string {
	if len(a.tagFilter) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.tagFilter))
	for name, on := range a.tagFilter {
		if on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// setTagFilter records (or clears) whether a tag is part of the tag filter.
func (a *App) setTagFilter(name string, on bool) {
	if a.tagFilter == nil {
		a.tagFilter = map[string]bool{}
	}
	if on {
		a.tagFilter[name] = true
	} else {
		delete(a.tagFilter, name)
	}
}

// tagFilterLabel is the filter-window drop-down button's text, showing how many
// tags are selected.
func (a *App) tagFilterLabel() string {
	n := len(a.selectedTagNames())
	if n == 0 {
		return "Tags: any ▾"
	}
	return fmt.Sprintf("Tags: %d ▾", n)
}

// tagFilterHeaderLabel is the compact label for the Tags-column filter-row
// drop-down, sized to fit under the narrow header.
func (a *App) tagFilterHeaderLabel() string {
	n := len(a.selectedTagNames())
	if n == 0 {
		return "tags ▾"
	}
	return plural(n, "tag") + " ▾"
}

// tagFilterButton builds the filter-window drop-down button that opens the tag
// checklist.
func (a *App) tagFilterButton() *widget.Button {
	btn := widget.NewButton(a.tagFilterLabel(), nil)
	btn.OnTapped = func() { a.showTagFilterPopup(btn, a.filterWin.Canvas(), a.tagFilterLabel) }
	return btn
}

// showTagFilterPopup opens the tag checklist anchored under the drop-down
// button, on the given canvas. Ticking a tag re-applies the filter live and
// keeps the popup open so several tags can be toggled in one go. The same popup
// backs both the filter window's Tags button and the Tags-column header
// drop-down.
func (a *App) showTagFilterPopup(anchor *widget.Button, canvas fyne.Canvas, label func() string) {
	if canvas == nil || a.sess == nil {
		return
	}
	defs := a.sess.TagDefs()
	box := container.NewVBox(widget.NewLabelWithStyle(
		"Keep rows with any ticked tag", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))

	if len(defs) == 0 {
		box.Add(widget.NewLabel("(no tags defined yet)"))
	}

	var pop *widget.PopUp
	reapply := func() {
		anchor.SetText(label())
		a.commitFilter()
	}
	for _, d := range defs {
		d := d
		chk := widget.NewCheck(d.Name, func(on bool) {
			a.setTagFilter(d.Name, on)
			reapply()
		})
		chk.SetChecked(a.tagFilter[d.Name])
		box.Add(container.NewHBox(colorSquare(d.Color), chk))
	}

	box.Add(widget.NewSeparator())
	box.Add(container.NewHBox(
		widget.NewButton("Clear", func() {
			a.tagFilter = map[string]bool{}
			reapply()
			if pop != nil {
				pop.Hide()
			}
		}),
		widget.NewButton("Close", func() {
			if pop != nil {
				pop.Hide()
			}
		}),
	))

	h := float32(90 + len(defs)*30)
	if h > 380 {
		h = 380
	}
	pop = widget.NewPopUp(container.NewVScroll(box), canvas)
	pop.Resize(fyne.NewSize(240, h))
	at := a.fyne.Driver().AbsolutePositionForObject(anchor)
	pop.ShowAtPosition(at.Add(fyne.NewPos(0, anchor.Size().Height)))
}
