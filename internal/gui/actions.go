package gui

import (
	"fmt"
	"path/filepath"
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
	// The master timeline is a read-only summary; ignore attempts to leave
	// read-only there so edits can't be made against its throwaway session.
	if a.masterMode {
		if a.sess != nil {
			a.sess.SetMode(model.ReadOnly)
		}
		if a.modeSelect != nil && s != model.ReadOnly.String() {
			a.modeSelect.SetSelected(model.ReadOnly.String())
		}
		return
	}
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
	spec := model.FilterSpec{
		Column:     model.ColAll,
		TaggedOnly: a.taggedChk.Checked,
		Conds:      a.conds,
		CondsAny:   a.condsAny,
	}
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
	a.conds = nil
	a.condsAny = false
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

	// Colour picker: preset swatches, single selection. Default to the first
	// preset; picking an existing tag below adopts its colour.
	chosen := palettePresets[0]
	var swatches []*swatch
	selectColor := func(hex string) {
		chosen = hex
		for _, sw := range swatches {
			sw.setSelected(colorEq(sw.fill, parseHex(hex)))
		}
	}
	swBox := container.NewGridWrap(fyne.NewSize(34, 26))
	for _, hex := range palettePresets {
		hex := hex
		sw := newSwatch(parseHex(hex), func() { selectColor(hex) })
		swatches = append(swatches, sw)
		swBox.Add(sw)
	}
	selectColor(chosen)

	// Reuse existing tags: tapping fills the name and adopts its colour.
	defs := a.sess.TagDefs()
	var reuse fyne.CanvasObject = widget.NewLabel("(none yet)")
	if len(defs) > 0 {
		chipBox := container.NewGridWrap(fyne.NewSize(150, 30))
		for _, d := range defs {
			d := d
			chipBox.Add(newTagChip(d.Name, d.Color, func() {
				entry.SetText(d.Name)
				selectColor(d.Color)
			}))
		}
		reuse = chipBox
	}

	current := widget.NewLabel("Current: " + strings.Join(a.sess.Tags(master), ", "))
	body := container.NewVBox(
		current,
		widget.NewLabel("Tag name:"), entry,
		widget.NewLabel("Colour (new tags only):"), swBox,
		widget.NewLabel("Reuse:"), reuse,
	)
	d := dialog.NewCustomConfirm("Add tag", "Add", "Cancel", container.NewVScroll(body), func(ok bool) {
		if !ok {
			return
		}
		name := strings.TrimSpace(entry.Text)
		if name == "" {
			return
		}
		// Register colour for a new tag; leave existing tags' colours alone.
		if _, known := a.sess.TagColor(name); !known {
			if err := a.sess.DefineTag(name, chosen); err != nil {
				a.showError(err)
				return
			}
		}
		if err := a.sess.AddTag(master, name); err != nil {
			a.showError(err)
			return
		}
		a.showDetail(master)
		a.refreshTable()
	}, a.win)
	d.Resize(a.dialogSize(560, 620))
	d.Show()
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

	// Master view: the row is a tagged entry from another timeline. Offer to
	// jump to it in its own timeline, and skip the annotation controls below
	// (they belong to a real session, not this read-only summary).
	if a.masterMode && master >= 0 && master < len(a.masterEntries) {
		e := a.masterEntries[master]
		items := []fyne.CanvasObject{
			widget.NewLabelWithStyle(e.Timeline+" — row "+fmt.Sprint(e.Row+1),
				fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			widget.NewButtonWithIcon("Open in "+e.Timeline, theme.NavigateNextIcon(),
				func() { a.openMasterSource(a.selRow) }),
			widget.NewSeparator(),
		}
		for i, h := range a.idx.Headers() {
			val := a.valueOf(master, model.ColumnRef(i))
			lbl := widget.NewLabel(val)
			lbl.Wrapping = fyne.TextWrapWord
			items = append(items, widget.NewLabelWithStyle(h, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), lbl)
		}
		a.detail.Objects = items
		a.detail.Refresh()
		return
	}

	items := []fyne.CanvasObject{
		widget.NewLabelWithStyle(fmt.Sprintf("Row %d", master+1), fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	}

	// Tags block, each chip prefixed with its colour.
	tagsRow := container.NewHBox(widget.NewLabel("Tags:"))
	for _, t := range a.sess.Tags(master) {
		t := t
		hex, _ := a.sess.TagColor(t)
		tagsRow.Add(colorSquare(hex))
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
			a.inc, a.curTimeline, a.masterMode = nil, nil, false
			a.annotCols = true
			a.reloadWith(idx, sess)
			a.rebuildIncidentMenu()
		}, a.win)
	}
	a.confirmIfDirty(do)
}

// save writes an annotated CSV (every row, in original order, with the Tags and
// Comment columns appended and cell edits applied) next to the source. The
// source CSV is never modified.
func (a *App) save() {
	// Inside an incident, annotations persist to the incident database rather
	// than to a CSV sidecar.
	if a.inc != nil {
		switch {
		case a.masterMode:
			dialog.ShowInformation("Master timeline",
				"The master timeline is a read-only summary. Save annotations from within each timeline.", a.win)
		case a.curTimeline != nil:
			if err := a.inc.SaveAnnotations(*a.curTimeline, a.sess.Snapshot(), a.idx); err != nil {
				a.showError(err)
				return
			}
			a.sess.MarkSaved()
			a.refreshStatus()
			dialog.ShowInformation("Saved",
				fmt.Sprintf("Annotations for %q saved to the incident.", a.curTimeline.Name), a.win)
		default:
			dialog.ShowInformation("No timeline open",
				"Open a timeline from the Incident menu, then save.", a.win)
		}
		return
	}

	if a.idx == nil {
		dialog.ShowInformation("Nothing to save", "Open a CSV or a timeline first.", a.win)
		return
	}

	dest := annotatedPath(a.idx.Path())
	full := model.NewView(a.idx, a.sess) // all rows, natural order
	if err := model.Export(full, a.sess, dest); err != nil {
		a.showError(err)
		return
	}
	a.sess.MarkSaved()
	a.refreshStatus()
	dialog.ShowInformation("Saved", fmt.Sprintf("%d rows written to\n%s", a.idx.RowCount(), dest), a.win)
}

// annotatedPath derives "name.csv" -> "name.annotated.csv".
func annotatedPath(src string) string {
	ext := filepath.Ext(src)
	return strings.TrimSuffix(src, ext) + ".annotated.csv"
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
	a.confirmIfDirty(func() {
		if a.idx != nil {
			a.idx.Close()
		}
		if a.inc != nil {
			a.inc.Close() // flushes and checkpoints the WAL
		}
		a.win.Close()
	})
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
	boxes := make([]*widget.Check, len(a.cols))
	grid := container.NewGridWithColumns(2)
	for i := range a.cols {
		c := widget.NewCheck(a.cols[i].title, nil)
		c.SetChecked(a.cols[i].visible)
		boxes[i] = c
		grid.Add(c)
	}
	setAll := func(v bool) {
		for _, c := range boxes {
			c.SetChecked(v)
		}
	}
	tools := container.NewHBox(
		widget.NewButton("Select all", func() { setAll(true) }),
		widget.NewButton("Select none", func() { setAll(false) }),
	)
	content := container.NewBorder(tools, nil, nil, nil, container.NewVScroll(grid))

	d := dialog.NewCustomConfirm("Columns", "Apply", "Cancel", content, func(ok bool) {
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
	d.Resize(a.dialogSize(700, 640))
	d.Show()
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
  Ctrl+G   go to row           Ctrl+S  save annotated CSV
  Ctrl+E   export view         Ctrl+B  toggle sidebar
  t        tag selected row    c       comment row
  m        cycle mode
  Click a header to sort; click again to reverse.

Tags and colours
  Rows are highlighted by their tag's colour; Bad is red, Suspicious
  yellow, Good green. The highest-priority tag on a row wins. Add your
  own tags with a chosen colour in the Tag dialog. Click a row's Tags
  cell to pick tags from a drop-down.

Editing cells
  Click an editable cell to type into it directly. In Investigator mode
  that is the Comment column; in World-write mode it is any cell. Enter
  or click away commits, Esc cancels.

Filtering
  The search box matches any column (prefix / for a regex). The Filter
  button builds per-column conditions with multiple values each, combined
  with AND or OR. The "#" column keeps each row's original CSV line
  number even after filtering or sorting.

Incidents (File and Incident menus)
  An incident groups several timelines in one database (.tlxdb), chosen
  when you create it. Add a CSV with Incident > Add timeline; open any
  timeline from the Incident menu. Inside an incident, Save writes your
  tags, comments and edits back to the incident database, not to a CSV.
  Incident > Master timeline shows every tagged row from all timelines in
  one time-sorted view; click a row's "Open in…" button to jump to it in
  its own timeline. The timestamp column of each timeline is detected
  automatically.

Saved views (Views menu)
  Save the current filter and sort as a named view. Saved views are
  application-wide, so they apply to any file, timeline or the master
  view. Columns are matched by name, so a view carries across timelines
  with the same fields.

Hover a truncated cell to see its full contents in a pop-up box.
Use the palette button to switch between light and dark themes.

Save writes every row to <file>.annotated.csv for a standalone CSV (data
plus Tags and Comment, with cell edits applied); the source CSV is never
modified. Inside an incident, Save persists to the incident database.
Export writes the current filtered, sorted view to a CSV.`
	lbl := widget.NewLabel(help)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	d := dialog.NewCustom("Help", "Close", container.NewVScroll(lbl), a.win)
	d.Resize(a.dialogSize(680, 620))
	d.Show()
}
