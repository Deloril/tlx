package gui

import (
	"fmt"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"timeline-engine/internal/model"
)

func (a *App) showError(err error) {
	if err != nil {
		dialog.ShowError(err, a.win)
	}
}

func (a *App) onModeChange(s string) {
	switch s {
	case model.Investigator.String():
		a.sess.SetMode(model.Investigator)
	case model.WorldWrite.String():
		a.sess.SetMode(model.WorldWrite)
	default:
		a.sess.SetMode(model.ReadOnly)
	}
	a.refreshStatus()
	if a.selectedMaster() >= 0 {
		a.showDetail(a.selectedMaster()) // re-render detail with correct editability
	}
}

// cycleMode steps Read-only -> Investigator -> World-write -> Read-only.
func (a *App) cycleMode() {
	next := (a.sess.Mode() + 1) % 3
	a.sess.SetMode(next)
	a.modeSelect.SetSelected(next.String())
}

func (a *App) applySearch() {
	q := strings.TrimSpace(a.search.Text)
	spec := model.FilterSpec{Column: model.ColAll, TaggedOnly: a.taggedChk.Checked}
	if strings.HasPrefix(q, "/") && len(q) > 1 {
		spec.Query = q[1:]
		spec.Regexp = true
	} else {
		spec.Query = q
	}
	if err := a.view.Apply(spec); err != nil {
		a.showError(err)
		return
	}
	a.clearSelection()
	a.table.ScrollTo(widget.TableCellID{Row: 0, Col: 0})
	a.refreshTable()
}

func (a *App) clearFilter() {
	a.search.SetText("")
	a.taggedChk.SetChecked(false)
	a.view.Apply(model.FilterSpec{})
	a.clearSelection()
	a.refreshTable()
}

// Tagging.

func (a *App) tagSelected() {
	master := a.selectedMaster()
	if master < 0 {
		a.showError(fmt.Errorf("select a row first"))
		return
	}
	entry := widget.NewEntry()
	entry.SetPlaceHolder("tag name, e.g. lateral-movement")
	known := a.sess.KnownTags()
	var chips fyne.CanvasObject = widget.NewLabel("(no tags yet)")
	if len(known) > 0 {
		chips = a.tagChoices(known, entry)
	}
	current := widget.NewLabel("Current: " + strings.Join(a.sess.Tags(master), ", "))
	body := container.NewVBox(current, entry, widget.NewLabel("Reuse:"), chips)
	dialog.ShowCustomConfirm("Add tag", "Add", "Cancel", body, func(ok bool) {
		if !ok {
			return
		}
		if err := a.sess.AddTag(master, strings.TrimSpace(entry.Text)); err != nil {
			a.showError(err)
			return
		}
		a.showDetail(master)
		a.refreshTable()
	}, a.win)
}

func (a *App) tagChoices(known []string, entry *widget.Entry) fyne.CanvasObject {
	box := container.NewGridWrap(fyne.NewSize(140, 32))
	for _, t := range known {
		t := t
		box.Add(widget.NewButton(t, func() { entry.SetText(t) }))
	}
	return box
}

func (a *App) commentSelected() {
	master := a.selectedMaster()
	if master < 0 {
		a.showError(fmt.Errorf("select a row first"))
		return
	}
	entry := widget.NewMultiLineEntry()
	entry.SetText(a.sess.Comment(master))
	entry.SetMinRowsVisible(4)
	dialog.ShowCustomConfirm("Comment", "Save", "Cancel", entry, func(ok bool) {
		if !ok {
			return
		}
		if err := a.sess.SetComment(master, entry.Text); err != nil {
			a.showError(err)
			return
		}
		a.showDetail(master)
		a.refreshTable()
	}, a.win)
}

// Detail pane: full row, tags, comment; editable per mode.

func (a *App) showDetail(master int) {
	if _, err := a.idx.Row(master); err != nil {
		a.detail.Objects = []fyne.CanvasObject{widget.NewLabel("error: " + err.Error())}
		a.detail.Refresh()
		return
	}
	mode := a.sess.Mode()
	items := []fyne.CanvasObject{
		widget.NewLabelWithStyle(fmt.Sprintf("Row %d", master+1), fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	}

	// Tags block.
	tagsRow := container.NewHBox(widget.NewLabel("Tags:"))
	for _, t := range a.sess.Tags(master) {
		t := t
		if mode != model.ReadOnly {
			tagsRow.Add(widget.NewButtonWithIcon(t, theme.CancelIcon(), func() {
				a.sess.RemoveTag(master, t)
				a.showDetail(master)
				a.refreshTable()
			}))
		} else {
			tagsRow.Add(widget.NewLabel("[" + t + "]"))
		}
	}
	if mode != model.ReadOnly {
		tagsRow.Add(widget.NewButtonWithIcon("", theme.ContentAddIcon(), a.tagSelected))
	}
	items = append(items, tagsRow)

	// Comment block.
	if mode == model.ReadOnly {
		items = append(items, widget.NewLabel("Comment: "+a.sess.Comment(master)))
	} else {
		ce := widget.NewMultiLineEntry()
		ce.SetText(a.sess.Comment(master))
		ce.SetMinRowsVisible(3)
		ce.OnSubmitted = func(s string) { a.sess.SetComment(master, s); a.refreshTable() }
		items = append(items,
			widget.NewLabel("Comment (Shift+Enter for newline, Enter to save):"), ce)
	}

	items = append(items, widget.NewSeparator())

	// One field per data column.
	for i, h := range a.idx.Headers() {
		i := i
		val := a.valueOf(master, model.ColumnRef(i))
		if mode == model.WorldWrite {
			e := widget.NewMultiLineEntry()
			e.SetText(val)
			e.SetMinRowsVisible(1)
			e.OnSubmitted = func(s string) {
				if err := a.sess.SetCell(master, i, s); err != nil {
					a.showError(err)
					return
				}
				a.refreshTable()
			}
			items = append(items, widget.NewLabelWithStyle(h, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), e)
		} else {
			lbl := widget.NewLabel(val)
			lbl.Wrapping = fyne.TextWrapWord
			items = append(items, widget.NewLabelWithStyle(h, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), lbl)
		}
	}

	a.detail.Objects = items
	a.detail.Refresh()
}

// File operations.

func (a *App) openFile() {
	do := func() {
		dialog.ShowFileOpen(func(r fyne.URIReadCloser, err error) {
			if err != nil || r == nil {
				return
			}
			path := r.URI().Path()
			r.Close()
			idx, err := model.Open(path, nil)
			if err != nil {
				a.showError(err)
				return
			}
			sess := model.NewSession(path)
			if err := sess.Load(); err != nil {
				a.showError(err)
			}
			sess.SetMode(a.startMode)
			a.reloadWith(idx, sess)
		}, a.win)
	}
	a.confirmIfDirty(do)
}

func (a *App) save() {
	if err := a.sess.Save(); err != nil {
		a.showError(err)
		return
	}
	a.refreshStatus()
	dialog.ShowInformation("Saved", "Annotations written to\n"+a.sess.SidecarPath(), a.win)
}

func (a *App) export() {
	dialog.ShowFileSave(func(w fyne.URIWriteCloser, err error) {
		if err != nil || w == nil {
			return
		}
		path := w.URI().Path()
		w.Close() // Export opens its own handle
		if err := model.Export(a.view, a.sess, path); err != nil {
			a.showError(err)
			return
		}
		dialog.ShowInformation("Exported", fmt.Sprintf("%d rows written to\n%s", a.view.Len(), path), a.win)
	}, a.win)
}

func (a *App) confirmIfDirty(then func()) {
	if a.sess == nil || !a.sess.Dirty() {
		then()
		return
	}
	dialog.ShowConfirm("Unsaved changes",
		"Discard unsaved annotations?", func(ok bool) {
			if ok {
				then()
			}
		}, a.win)
}

func (a *App) onClose() {
	a.confirmIfDirty(func() { a.win.Close() })
}

// Navigation.

func (a *App) gotoLine() {
	entry := widget.NewEntry()
	entry.SetPlaceHolder(fmt.Sprintf("1 – %d", a.view.Len()))
	dialog.ShowCustomConfirm("Go to row", "Go", "Cancel", entry, func(ok bool) {
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(entry.Text))
		if err != nil || n < 1 || n > a.view.Len() {
			a.showError(fmt.Errorf("row must be between 1 and %d", a.view.Len()))
			return
		}
		id := widget.TableCellID{Row: n - 1, Col: 0}
		a.table.ScrollTo(id)
		a.table.Select(id)
	}, a.win)
}

// findNext scrolls to the next row after the current selection whose any column
// contains the search text (case-insensitive). It does not change the filter.
func (a *App) findNext() {
	q := strings.TrimSpace(a.search.Text)
	q = strings.TrimPrefix(q, "/")
	if q == "" {
		return
	}
	ql := strings.ToLower(q)
	start := a.selRow + 1
	if start < 0 {
		start = 0
	}
	for r := start; r < a.view.Len(); r++ {
		if a.rowContains(a.view.Master(r), ql) {
			id := widget.TableCellID{Row: r, Col: 0}
			a.table.ScrollTo(id)
			a.table.Select(id)
			return
		}
	}
	dialog.ShowInformation("Find", "No further matches.", a.win)
}

func (a *App) rowContains(master int, needleLower string) bool {
	if strings.Contains(strings.ToLower(strings.Join(a.sess.Tags(master), " ")), needleLower) {
		return true
	}
	if strings.Contains(strings.ToLower(a.sess.Comment(master)), needleLower) {
		return true
	}
	rec, err := a.idx.Row(master)
	if err != nil {
		return false
	}
	for i, cell := range rec {
		if v, ok := a.sess.CellOverride(master, i); ok {
			cell = v
		}
		if strings.Contains(strings.ToLower(cell), needleLower) {
			return true
		}
	}
	return false
}

// Column visibility.

func (a *App) columnPicker() {
	checks := container.NewVBox()
	boxes := make([]*widget.Check, len(a.cols))
	for i := range a.cols {
		i := i
		c := widget.NewCheck(a.cols[i].title, nil)
		c.SetChecked(a.cols[i].visible)
		boxes[i] = c
		checks.Add(c)
	}
	dialog.ShowCustomConfirm("Columns", "Apply", "Cancel",
		container.NewVScroll(checks), func(ok bool) {
			if !ok {
				return
			}
			any := false
			for i := range a.cols {
				a.cols[i].visible = boxes[i].Checked
				any = any || boxes[i].Checked
			}
			if !any { // never hide everything
				a.cols[0].visible = true
			}
			a.rebuildVisible()
			for i, ci := range a.visible {
				a.table.SetColumnWidth(i, a.cols[ci].width)
			}
			a.clearSelection()
			a.refreshTable()
		}, a.win)
}

func (a *App) showHelp() {
	help := `Timeline explorer

Modes
  Read-only      view and search only
  Investigator   also add tags and comments (sidecar, original file untouched)
  World-write    also edit any cell value

Keyboard
  Ctrl+F   focus filter        Enter   apply filter
  F3       find next match     Esc     clear filter
  Ctrl+G   go to row           Ctrl+S  save annotations
  Ctrl+E   export view         t       tag selected row
  c        comment row         m       cycle mode
  Click a header to sort; click again to reverse.

Filter box: plain text matches any column. Prefix with / for a regex.
Annotations save to <file>.tlx.json and never modify the source CSV.
Export writes the current (filtered, sorted) view with Tags and Comment columns.`
	lbl := widget.NewLabel(help)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	dialog.ShowCustom("Help", "Close", container.NewVScroll(lbl), a.win)
}
