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

	"tlx/internal/model"
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

func (a *App) applySearch() {
	if a.view == nil {
		return
	}
	spec := model.FilterSpec{
		Column:     model.ColAll,
		Expr:       strings.TrimSpace(a.search.Text),
		Cased:      a.caseChk.Checked,
		TaggedOnly: a.taggedChk.Checked,
		Tags:       a.selectedTagNames(),
		Conds:      a.conds,
		CondsAny:   a.condsAny,
		ColFilters: a.colFilter,
	}
	if err := a.view.Apply(spec); err != nil {
		// A malformed query is reported inline in the status bar rather than in a
		// modal, so the user can keep editing. The previous view is left intact.
		if a.statusFilter != nil {
			a.statusFilter.SetText("Filter: " + err.Error())
		}
		return
	}
	a.hl = a.view.BuildHighlighter(spec) // highlight what the filter matched
	a.clearSelection()
	a.table.ScrollTo(widget.TableCellID{Row: 0, Col: 0})
	a.refreshTable()
}

func (a *App) clearFilter() {
	// Reset the persistent filter widgets without letting their change callbacks
	// re-apply mid-clear.
	a.suppressFilter = true
	a.search.SetText("")
	a.taggedChk.SetChecked(false)
	a.caseChk.SetChecked(false)
	a.suppressFilter = false
	a.conds = nil
	a.condsAny = false
	a.colFilter = map[model.ColumnRef]string{}
	a.tagFilter = map[string]bool{}
	a.hl = nil
	if a.view == nil { // view torn down (e.g. between cases); nothing to filter
		return
	}
	a.view.Apply(model.FilterSpec{})
	a.clearSelection()
	a.refreshTable()
	a.rebuildFilterPanel() // rebuild the docked filter panel to the empty state
}

// Tagging.

func (a *App) tagSelected() {
	master := a.selectedMaster()
	if master < 0 {
		a.showError(fmt.Errorf("select a row first"))
		return
	}
	a.addTagDialog([]int{master})
}

// bulkTag adds one tag to every selected row.
func (a *App) bulkTag() {
	masters := a.selectedMasters()
	if len(masters) == 0 {
		a.showError(fmt.Errorf("select one or more rows first"))
		return
	}
	a.addTagDialog(masters)
}

// addTagDialog shows the tag picker and applies the chosen tag to every master
// row given. With one row it shows that row's current tags; with several it says
// how many rows the tag will be added to.
func (a *App) addTagDialog(masters []int) {
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

	var header string
	if len(masters) == 1 {
		header = "Current: " + strings.Join(a.sess.Tags(masters[0]), ", ")
	} else {
		header = fmt.Sprintf("Adding to %s", plural(len(masters), "row"))
	}
	body := container.NewVBox(
		widget.NewLabel(header),
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
		for _, m := range masters {
			if err := a.sess.AddTag(m, name); err != nil {
				a.showError(err)
				return
			}
		}
		if a.selectedMaster() >= 0 {
			a.showDetail(a.selectedMaster())
		}
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

// bulkComment sets the same comment on every selected row, replacing whatever
// each row had.
func (a *App) bulkComment() {
	masters := a.selectedMasters()
	if len(masters) == 0 {
		a.showError(fmt.Errorf("select one or more rows first"))
		return
	}
	entry := widget.NewMultiLineEntry()
	entry.SetMinRowsVisible(4)
	// Seed with the common comment if every selected row already shares one.
	first := a.sess.Comment(masters[0])
	same := true
	for _, m := range masters[1:] {
		if a.sess.Comment(m) != first {
			same = false
			break
		}
	}
	if same {
		entry.SetText(first)
	}
	title := fmt.Sprintf("Comment %s (replaces existing)", plural(len(masters), "row"))
	dialog.ShowCustomConfirm(title, "Save", "Cancel", entry, func(ok bool) {
		if !ok {
			return
		}
		for _, m := range masters {
			if err := a.sess.SetComment(m, entry.Text); err != nil {
				a.showError(err)
				return
			}
		}
		if a.selectedMaster() >= 0 {
			a.showDetail(a.selectedMaster())
		}
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
			items = append(items, widget.NewSeparator(),
				widget.NewLabelWithStyle(h, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				newSelectableLabel(a, val))
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

	// One field per data column. Adopted tag/comment columns are shown as the
	// Tags and Comment blocks above, not repeated here.
	for i, h := range a.idx.Headers() {
		i := i
		if a.annotCols && a.adopted.Has(i) {
			continue
		}
		val := a.valueOf(master, model.ColumnRef(i))
		items = append(items, widget.NewSeparator(),
			widget.NewLabelWithStyle(h, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
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
			items = append(items, e)
		} else {
			items = append(items, newSelectableLabel(a, val))
		}
	}

	a.detail.Objects = items
	a.detail.Refresh()
}

// File operations.

func (a *App) openFile() {
	do := func() {
		a.showOpenFile(func(path string) {
			idx, err := model.Open(path, nil)
			if err != nil {
				a.showError(err)
				return
			}
			sess := model.NewSession(path)
			if err := sess.Load(); err != nil {
				a.showError(err)
			}
			// A fresh file (no sidecar) with existing tag/comment columns seeds
			// its annotations from them.
			if !sess.Loaded() {
				if err := sess.SeedFromColumns(idx, model.DetectAnnotationColumns(idx.Headers())); err != nil {
					a.showError(err)
				}
			}
			sess.SetMode(a.startMode)
			a.cse, a.curTimeline, a.masterMode = nil, nil, false
			a.annotCols = true
			a.colNames = nil
			a.reloadWith(idx, sess)
			a.rebuildCaseMenu()
		})
	}
	a.confirmIfDirty(do)
}

// save writes an annotated CSV (every row, in original order, with the Tags and
// Comment columns appended and cell edits applied) next to the source. The
// source CSV is never modified.
func (a *App) save() {
	// Inside a case, annotations persist to the case database rather
	// than to a CSV sidecar.
	if a.cse != nil {
		switch {
		case a.masterMode:
			dialog.ShowInformation("Master timeline",
				"The master timeline is a read-only summary. Save annotations from within each timeline.", a.win)
		case a.curTimeline != nil:
			if err := a.cse.SaveAnnotations(*a.curTimeline, a.sess.Snapshot(), a.idx); err != nil {
				a.showError(err)
				return
			}
			a.sess.MarkSaved()
			a.refreshStatus()
			dialog.ShowInformation("Saved",
				fmt.Sprintf("Annotations for %q saved to the case.", a.curTimeline.Name), a.win)
		default:
			dialog.ShowInformation("No timeline open",
				"Open a timeline from the Case menu, then save.", a.win)
		}
		return
	}

	if a.idx == nil {
		dialog.ShowInformation("Nothing to save", "Open a CSV or a timeline first.", a.win)
		return
	}

	// Persist annotations to the sidecar (the canonical store reloaded on open),
	// so an explicit save leaves the same on-disk state autosave would have.
	if err := a.sess.Save(); err != nil {
		a.showError(err)
		return
	}
	dest := annotatedPath(a.idx.Path())
	full := model.NewView(a.idx, a.sess) // all rows, natural order
	if err := model.ExportOmitting(full, a.sess, dest, a.omitCols()); err != nil {
		a.showError(err)
		return
	}
	a.refreshStatus()
	dialog.ShowInformation("Saved", fmt.Sprintf("%d rows written to\n%s", a.idx.RowCount(), dest), a.win)
}

// annotatedPath derives "name.csv" -> "name.annotated.csv".
func annotatedPath(src string) string {
	ext := filepath.Ext(src)
	return strings.TrimSuffix(src, ext) + ".annotated.csv"
}

func (a *App) export() {
	a.showSaveFile("", func(path string) {
		if err := model.ExportOmittingWithComment(a.view, a.sess, path, a.omitCols(), a.currentTimelineComment()); err != nil {
			a.showError(err)
			return
		}
		dialog.ShowInformation("Exported", fmt.Sprintf("%d rows written to\n%s", a.view.Len(), path), a.win)
	})
}

// omitCols is the set of source data columns to drop from an export: the
// adopted tag/comment columns, whose content is written as the appended
// Tags/Comment columns instead. Nil when nothing is adopted.
func (a *App) omitCols() map[int]bool {
	if !a.annotCols || !a.adopted.Any() {
		return nil
	}
	m := map[int]bool{}
	if a.adopted.Tag >= 0 {
		m[a.adopted.Tag] = true
	}
	if a.adopted.Comment >= 0 {
		m[a.adopted.Comment] = true
	}
	return m
}

// confirmIfDirty runs then() once any unsaved annotations are dealt with. With
// autosave on, it flushes first, so switching context saves rather than
// discards; the discard prompt only appears if that save failed (e.g. a write
// error), as a last resort before losing work.
func (a *App) confirmIfDirty(then func()) {
	if a.sess == nil || !a.sess.Dirty() {
		then()
		return
	}
	a.flushAutosave()
	if !a.sess.Dirty() { // saved cleanly
		then()
		return
	}
	dialog.ShowConfirm("Save failed",
		"Annotations could not be autosaved. Discard them and continue?", func(ok bool) {
			if ok {
				then()
			}
		}, a.win)
}

func (a *App) onClose() {
	a.autosaveMu.Lock()
	a.closing = true
	if a.autosaveTimer != nil {
		a.autosaveTimer.Stop()
	}
	a.autosaveMu.Unlock()
	a.confirmIfDirty(func() {
		for _, p := range a.rightPanels {
			if p != nil && p.win != nil {
				p.win.Close()
			}
		}
		if a.idx != nil {
			a.idx.Close()
		}
		if a.cse != nil {
			a.cse.Close() // flushes and checkpoints the WAL
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
// The needle is taken from the search box as plain text; for a field=value
// query it uses the value, so a quick F3 still works alongside the query syntax.
func (a *App) findNext() {
	q := findNeedle(a.search.Text)
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

// findNeedle extracts a plain substring to use for F3 find-next from whatever
// is in the search box. It takes the value side of the last field=value term
// and unwraps /regex/ or "quoted" delimiters, best effort.
func findNeedle(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.LastIndex(q, "="); i >= 0 {
		q = strings.TrimSpace(q[i+1:])
	}
	if len(q) >= 2 {
		if (q[0] == '/' && q[len(q)-1] == '/') || (q[0] == '"' && q[len(q)-1] == '"') {
			q = q[1 : len(q)-1]
		}
	}
	return q
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
	// A single left-aligned column reads as a checklist. A grid stretches each
	// cell to fill the dialog width, so short column names end up flung apart
	// with large gaps between them.
	list := container.NewVBox()
	for i := range a.cols {
		c := widget.NewCheck(a.cols[i].title, nil)
		c.SetChecked(a.cols[i].visible)
		boxes[i] = c
		list.Add(c)
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
	content := container.NewBorder(tools, nil, nil, nil, container.NewVScroll(list))

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
	d.Resize(a.dialogSize(380, 560))
	d.Show()
}

func (a *App) showHelp() {
	help := `Timeline explorer

Modes
  Read-only      view and search only
  Investigator   also add tags and comments (sidecar, original file untouched)
  World-write    also edit any cell value

Keyboard
  /        open filter panel   Ctrl+F  open filter panel
  Enter    apply filter        Esc     clear filter
  F3       find next match     Ctrl+G  go to row
  Ctrl+S   save                Ctrl+E  export view
  Ctrl+B   toggle right dock   Ctrl+L  toggle left sidebar
  t        tag selected row    c       comment row
  Ctrl+Shift+F  toggle per-column filter row
  Click a header to sort; click again to reverse. The Filter row button
  (or Ctrl+Shift+F) shows a box under each header to filter that column;
  press Enter to apply. It is off by default because it makes rows taller.
  The Clear filters button (toolbar) drops every filter at once, same as Esc.

Selecting rows
  Click        select one row
  Shift+click  select every row between the last click and this one
  Ctrl/Cmd+click  add or remove one row from the selection
  Right-click  menu to tag or comment every selected row at once.
               Right-click a timestamp cell for "Filter ±5 min around
               this time", which narrows to that column within 5 minutes.
               A timestamp cell also offers "Add time to notes"; any other
               cell offers "Add … as artifact", both feeding the
               Investigator's notes panel.

Tags and colours
  Rows are highlighted by their tag's colour; Bad is red, Suspicious
  yellow, Good green. The highest-priority tag on a row wins. Add your
  own tags with a chosen colour in the Tag dialog. Click a row's Tags
  cell to pick tags from a drop-down; the trash icon beside a tag there
  deletes it from the palette and strips it from every row (across every
  timeline, in a case).

Editing cells
  Click an editable cell to type into it directly. In Investigator mode
  that is the Comment column; in World-write mode it is any cell. Enter
  or click away commits, Esc cancels.

Filtering
  Filtering lives in the Filter panel of the right dock (the Filter button,
  / or Ctrl+F). Apply a filter, then keep working the grid and refine it as
  you find things. Pop the panel out to its own window with the panel's
  float button if you want it on a second monitor; "Dock to right" (or
  closing the window) puts it back.

  A freetext query. The simplest is a word, which matches any column. You can
  also write field comparisons and combine them:

    Summary=derp                 Summary contains "derp"
    Host!=ws1                    Host does not contain "ws1"
    tag=bad OR tag=suspicious    either tag present
    Summary=derp AND (tag=bad OR tag=suspicious)
    Host=/^dc-\d+$/              regex: value wrapped in /…/
    Summary="a b c"              quote values with spaces
    NOT tag=benign               negate a term

  Field is a column name (case-insensitive), or one of tag, comment, row.
  AND/OR/NOT and parentheses group terms; AND binds tighter than OR.
  Matching is case-insensitive unless "Case sensitive" is ticked. A bad
  query is reported in the status bar and leaves the current view intact.

  Timestamps. A column can be compared with before, after or between:

    Timestamp before 2020-06-01        rows earlier than that day
    Timestamp after 2021               rows after all of 2021
    Timestamp between 2020 and 2021    within 2020 up to the end of 2021

  A bare year, month or day covers the whole period: "2020" is all of 2020,
  "before 2020" is anything earlier, "after 2020" is 2021 onward. You can
  shift a time by a duration with + or - (spaces required):

    Timestamp after now - 7d           the last week (now = the current time)
    Timestamp before 2024-01-01 + 12h

  Durations use s, m, h, d, w (second, minute, hour, day, week) and combine,
  e.g. 1d12h. now and time both mean the current time.

  Structured per-column conditions sit above the query, each with multiple
  values and its own regex/all-values options, combined with AND or OR. The
  structured conditions and the freetext query combine together. Apply commits
  both; Enter in the query box does the same. The "#" column keeps each row's
  original CSV line number even after filtering or sorting.

  Tags drop-down: next to the query checkboxes, the Tags button opens a
  checklist of every tag. Tick one or more to keep rows carrying any of them;
  it combines (AND) with the rest of the filter. This is the same as writing
  tag=x OR tag=y in the query, but without typing.

  Per-column boxes: the Filter row button (or Ctrl+Shift+F) reveals a small
  filter box under each header's sort button. Type a substring and press Enter
  to narrow that one column. A lone * keeps only rows where that column is
  non-empty. Under the Tags column the box is a drop-down instead: the same tag
  checklist as the Filter panel. These boxes combine (AND) with the query and
  structured conditions, and matches are highlighted in the grid and hover
  tooltip. The filter row is off by default because it makes the grid rows
  taller.

Existing tag/comment columns
  If a timeline already has a Tags column (Tag/Tags) or a comment column
  (Comment/Comments/Note/Notes), those become the annotation columns rather
  than adding new ones: their values seed the row tags and comments, so they
  are coloured, filter with tag=, feed the master view, and edits write back
  to them on save. Tag cells split on commas and semicolons.

Cases (File and Case menus)
  A case groups several timelines in one database (.tlxdb). Creating a
  case makes a folder to hold the database and its timelines. Add a CSV
  with Case > Add timeline and it is copied into the case folder (unless
  it already lives there); open any timeline from the Case menu. Inside a
  case, Save writes your
  tags, comments and edits back to the case database, not to a CSV.
  Case > Master timeline shows every tagged row from all timelines in
  one time-sorted view; click a row's "Open in…" button to jump to it in
  its own timeline. The timestamp column of each timeline is detected
  automatically.

  Case > Rename columns gives the open timeline's columns display names.
  Columns renamed to the same name across timelines merge into one column
  in the master view, so timelines with different field names line up. The
  master view shows those merged columns plus Time, Timeline, Tags and
  Comment.

IOC lists (left sidebar, or File > IOC lists)
  A timeline can carry several named IOC lists, each a set of indicators one
  per line. The IOC lists section of the left sidebar lists them: click a
  name to edit its indicators, the play button to run it, the trash to delete
  it. New list adds one. Every case starts with a default list named after
  the case. For a standalone timeline the lists are saved per file; inside a
  case they belong to the case and run automatically whenever you import a new
  timeline. Run all runs every list at once.

  Save & run matches a list against the open timeline and tags every hit with
  that list's own tag, ioc:<list name>, so the grid shows which list matched.

  Each line is one of a plain string (matched against every column), a regex
  wrapped in /…/, or a filter query. A filter query starts with a backtick and
  then uses the same syntax as the search box, including before/after/between on
  timestamp columns:

    evil.exe                     plain string, any column
    /T[0-9]{4}/                  regex
    Summary=psexec AND Host=dc1  filter query (prefixed with a backtick)

  Blank lines and lines starting with # are ignored. Matching is
  case-insensitive. Lines that fail to compile are listed after a run; the
  rest still apply.

Right-hand dock (toggle with Ctrl+B or the Panels button)
  The right side stacks collapsible panels: Filter, Details, Investigator's
  notes and Timeline comments. Tap a panel's title to collapse or expand it.
  The float button on a panel pops it out into its own window; "Dock to
  right" (or closing the window) returns it. If every panel is floated out
  the right dock hides itself.

Investigator's notes (right dock)
  Two per-timeline lists, Artifacts and Times, for things worth coming back
  to. Right-click a timestamp cell and choose "Add time to notes"; right-click
  any other cell and choose "Add … as artifact". Each entry has a checkbox
  that strikes it through once you're done with it, a search button to filter
  the view to rows containing it, a delete button, and — inside a case — a
  plus button to copy it into the case's default IOC list. These notes belong
  to one timeline and are not shared with the others.

Timeline comments (right dock)
  A free-text field per timeline for running notes towards a write-up, saved
  as you type. When it is not blank, an export writes it as the first row of
  the CSV ("Timeline comments:" then the text) before the header and events.

Saved views (left sidebar, toggle with Ctrl+L or the Views button)
  Save current view stores the filter query, column conditions, sort and
  case-sensitivity as a named view. Click a view in the sidebar to apply it;
  the trash icon deletes it. Saved views are application-wide, so they apply
  to any file, timeline or the master view. Columns are matched by name, so a
  view carries across timelines with the same fields.

Hover a truncated cell to see its full contents in a pop-up box.
Use the palette button to switch between light and dark themes.

Autosave. Tags, comments and edits are written shortly after each change
on their own — to the <file>.tlx.json sidecar for a standalone timeline,
or to the case database inside a case. You don't need to save by hand; the
status bar shows "saved" once a change is on disk.

Save (Ctrl+S) also writes every row to <file>.annotated.csv for a
standalone CSV (data plus Tags and Comment, with cell edits applied); the
source CSV is never modified. Inside a case, Save persists to the case
database. Export writes the current filtered, sorted view to a CSV.`
	lbl := widget.NewLabel(help)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	d := dialog.NewCustom("Help", "Close", container.NewVScroll(lbl), a.win)
	d.Resize(a.dialogSize(680, 620))
	d.Show()
}
