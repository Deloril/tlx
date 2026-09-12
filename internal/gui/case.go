package gui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/casefile"
	"tlx/internal/model"
)

// buildMainMenu assembles the window menu bar. The Case menu is rebuilt from
// the current case state each time this is called.
func (a *App) buildMainMenu() *fyne.MainMenu {
	fileMenu := fyne.NewMenu("File",
		fyne.NewMenuItem("Open CSV…", a.openFile),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("New case…", a.newCase),
		fyne.NewMenuItem("Open case…", a.openCase),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Save", a.save),
		fyne.NewMenuItem("Export view…", a.export),
	)

	a.caseMenu = fyne.NewMenu("Case", a.caseMenuItems()...)

	// Saved views live in the collapsible left sidebar (see views.go), not the
	// menu bar.
	return fyne.NewMainMenu(fileMenu, a.caseMenu)
}

// caseMenuItems lists the master timeline, an add action and every
// registered timeline, or a hint when no case is open.
func (a *App) caseMenuItems() []*fyne.MenuItem {
	if a.cse == nil {
		var items []*fyne.MenuItem
		// A standalone timeline is open: offer to seed a new case from it.
		if a.idx != nil && !a.masterMode {
			items = append(items, fyne.NewMenuItem("Create case with this timeline…", a.createCaseWithCurrent))
			items = append(items, fyne.NewMenuItemSeparator())
		}
		hint := fyne.NewMenuItem("(open or create a case)", nil)
		hint.Disabled = true
		return append(items, hint)
	}
	renameItem := fyne.NewMenuItem("Rename columns…", a.renameColumns)
	renameItem.Disabled = a.curTimeline == nil || a.masterMode
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("★ Master timeline", a.showMasterTimeline),
		fyne.NewMenuItem("Add timeline…", a.addTimelineToCase),
		renameItem,
		fyne.NewMenuItemSeparator(),
	}
	tls, err := a.cse.Timelines()
	if err != nil {
		a.showError(err)
	}
	if len(tls) == 0 {
		empty := fyne.NewMenuItem("(no timelines yet)", nil)
		empty.Disabled = true
		items = append(items, empty)
		return items
	}
	for _, tl := range tls {
		tl := tl
		label := tl.Name
		if a.curTimeline != nil && a.curTimeline.ID == tl.ID && !a.masterMode {
			label = "● " + label
		}
		items = append(items, fyne.NewMenuItem(label, func() { a.openTimeline(tl) }))
	}
	return items
}

// rebuildCaseMenu refreshes the whole menu bar so the Case submenu
// reflects the current case and open timeline.
func (a *App) rebuildCaseMenu() {
	a.win.SetMainMenu(a.buildMainMenu())
}

// clearView tears down any open file/timeline and shows a placeholder. Used when
// a case is created or opened but no timeline is loaded yet, so the grid
// does not keep showing an unrelated file.
func (a *App) clearView() {
	if a.idx != nil {
		a.idx.Close()
	}
	a.idx, a.sess, a.view = nil, nil, nil
	a.curTimeline = nil
	a.masterMode = false
	a.masterEntries = nil
	a.selRow, a.selCol = -1, -1
	a.editing, a.editFocused = false, false
	a.hideTooltip()
	a.hideFilterWindow() // the filter window acts on a.view, which is now nil

	msg := widget.NewLabel("Add a timeline from the Case menu to begin.")
	add := widget.NewButtonWithIcon("Add timeline…", theme.ContentAddIcon(), a.addTimelineToCase)
	a.win.SetContent(container.NewCenter(container.NewVBox(msg, container.NewCenter(add))))
}

// setCase swaps the active case, closing any previous one.
func (a *App) setCase(cse *casefile.Case) {
	if a.cse != nil {
		a.cse.Close()
	}
	a.cse = cse
	a.curTimeline = nil
	a.masterMode = false
	a.rebuildCaseMenu()
}

func (a *App) newCase() {
	a.showSaveFile("case.tlxdb", func(path string) {
		if filepath.Ext(path) == "" {
			path += ".tlxdb"
		}
		cse, err := casefile.Create(path)
		if err != nil {
			a.showError(err)
			return
		}
		a.setCase(cse)
		a.clearView()
	})
}

// createCaseWithCurrent creates a new case, adds the currently open standalone
// timeline to it, and opens it inside the case. Any tags, comments or edits made
// in the standalone session are carried across so switching in loses nothing.
func (a *App) createCaseWithCurrent() {
	if a.idx == nil || a.cse != nil || a.masterMode {
		return
	}
	src := a.idx.Path()
	a.showSaveFile("case.tlxdb", func(path string) {
		if filepath.Ext(path) == "" {
			path += ".tlxdb"
		}
		cse, err := casefile.Create(path)
		if err != nil {
			a.showError(err)
			return
		}
		name := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		tl, err := cse.AddTimeline(name, a.idx, time.Now().Format(time.RFC3339))
		if err != nil {
			cse.Close()
			a.showError(err)
			return
		}
		if err := cse.SaveAnnotations(tl, a.sess.Snapshot(), a.idx); err != nil {
			cse.Close()
			a.showError(err)
			return
		}
		// Re-open the source into a fresh index for the case-backed session; the
		// standalone index is closed by reloadWith inside openTimelineWithIndex.
		idx, err := model.Open(src, nil)
		if err != nil {
			cse.Close()
			a.showError(err)
			return
		}
		a.setCase(cse)
		a.openTimelineWithIndex(tl, idx)
	})
}

func (a *App) openCase() {
	a.showOpenFile(func(path string) {
		cse, err := casefile.Open(path)
		if err != nil {
			a.showError(err)
			return
		}
		a.setCase(cse)
		tls, _ := cse.Timelines()
		if len(tls) == 0 {
			a.clearView()
			return
		}
		a.openTimeline(tls[0])
	})
}

func (a *App) addTimelineToCase() {
	if a.cse == nil {
		a.showError(fmt.Errorf("open or create a case first"))
		return
	}
	a.confirmIfDirty(func() {
		a.showOpenFile(func(path string) {
			idx, err := model.Open(path, nil)
			if err != nil {
				a.showError(err)
				return
			}
			name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			tl, err := a.cse.AddTimeline(name, idx, time.Now().Format(time.RFC3339))
			if err != nil {
				idx.Close()
				a.showError(err)
				return
			}
			// Open it straight away, reusing the index we just indexed.
			a.openTimelineWithIndex(tl, idx)
		})
	})
}

// openTimeline opens a registered timeline by (re)indexing its source file.
func (a *App) openTimeline(tl casefile.TimelineMeta) {
	a.confirmIfDirty(func() {
		idx, err := model.Open(tl.SourcePath, nil)
		if err != nil {
			a.showError(fmt.Errorf("open %s: %w", tl.SourcePath, err))
			return
		}
		a.openTimelineWithIndex(tl, idx)
	})
}

// openTimelineWithIndex loads a timeline's annotations from the case onto an
// already-opened index and shows it. It does not prompt about unsaved changes;
// callers that switch context should have done so already.
func (a *App) openTimelineWithIndex(tl casefile.TimelineMeta, idx *model.Index) {
	sess := model.NewSession(tl.SourcePath)
	if snap, err := a.cse.LoadAnnotations(tl.ID); err != nil {
		a.showError(err)
	} else {
		sess.LoadSnapshot(snap)
	}
	sess.SetMode(a.startMode)
	tlCopy := tl
	a.curTimeline = &tlCopy
	a.masterMode = false
	a.masterEntries = nil
	a.annotCols = true
	a.colNames = tlCopy.DisplayHeaders // renamed columns show under their new names
	a.reloadWith(idx, sess)
	a.rebuildCaseMenu()
}

// renameColumns lets the user give the open timeline's columns display names,
// stored in the case. Matching names across timelines merge into one column in
// the master view.
func (a *App) renameColumns() {
	if a.cse == nil || a.curTimeline == nil || a.masterMode {
		return
	}
	src := a.idx.Headers()
	entries := make([]*widget.Entry, len(src))
	form := container.NewVBox()
	for i, h := range src {
		e := widget.NewEntry()
		cur := h
		if i < len(a.colNames) && a.colNames[i] != "" {
			cur = a.colNames[i]
		}
		e.SetText(cur)
		entries[i] = e
		form.Add(container.NewBorder(nil, nil, widget.NewLabel(h+" →"), nil, e))
	}
	hint := widget.NewLabel("Rename columns so they match across timelines; columns sharing a name merge into one column in the master view. Blank falls back to the original name.")
	hint.Wrapping = fyne.TextWrapWord
	content := container.NewBorder(hint, nil, nil, nil, container.NewVScroll(form))

	d := dialog.NewCustomConfirm("Rename columns — "+a.curTimeline.Name, "Save", "Cancel", content, func(ok bool) {
		if !ok {
			return
		}
		names := make([]string, len(src))
		for i := range src {
			n := strings.TrimSpace(entries[i].Text)
			if n == "" {
				n = src[i]
			}
			names[i] = n
		}
		if err := a.cse.SetColumnNames(a.curTimeline.ID, names); err != nil {
			a.showError(err)
			return
		}
		a.curTimeline.DisplayHeaders = names
		a.colNames = names
		a.applyColumnTitles()
	}, a.win)
	d.Resize(a.dialogSize(560, 620))
	d.Show()
}

// applyColumnTitles updates data-column titles in place from a.colNames,
// preserving each column's width and visibility, and repaints the header.
func (a *App) applyColumnTitles() {
	src := a.idx.Headers()
	for i := range a.cols {
		ref := a.cols[i].ref
		if ref < 0 || int(ref) >= len(src) { // skip virtual annotation columns
			continue
		}
		title := src[int(ref)]
		if int(ref) < len(a.colNames) && a.colNames[int(ref)] != "" {
			title = a.colNames[int(ref)]
		}
		a.cols[i].title = title
	}
	if a.table != nil {
		a.table.Refresh()
	}
	a.refreshStatus()
}

// showMasterTimeline builds a read-only, time-sorted view of every tagged row
// across all timelines in the casefile.
func (a *App) showMasterTimeline() {
	if a.cse == nil {
		return
	}
	a.confirmIfDirty(func() {
		entries, err := a.cse.Master()
		if err != nil {
			a.showError(err)
			return
		}
		headers, records := masterGrid(entries)
		idx := model.NewMemoryIndex(headers, records)
		sess := model.NewSession("(master)")
		sess.SetMode(model.ReadOnly)

		a.masterEntries = entries
		a.curTimeline = nil
		a.masterMode = true
		a.annotCols = false
		a.colNames = nil // master headers are already the canonical names
		a.reloadWith(idx, sess)
		a.rebuildCaseMenu()
		if len(entries) == 0 {
			dialog.ShowInformation("Master timeline",
				"No tagged rows yet. Tag rows in a timeline and save to populate the master view.", a.win)
		}
	})
}

// masterGrid turns the tagged-row entries into the master view's headers and
// records. Leading columns are fixed (Time, Timeline, Tags, Comment); the rest
// is the ordered union of every timeline's display headers, so columns renamed
// to the same name across timelines line up in one column. Entries snapshotted
// before the merge feature carry no cells, so they fall back to a Summary
// column when no canonical columns are available.
func masterGrid(entries []casefile.MasterEntry) (headers []string, records [][]string) {
	fixed := []string{"Time", "Timeline", "Tags", "Comment"}

	// Ordered union of display headers across all entries. Duplicate names within
	// one timeline collapse to a single master column (the later cell wins for
	// that row) — that is the point of merging, so renaming two columns to the
	// same name inside one timeline is lossy by design.
	var canonical []string
	seen := map[string]int{} // name -> index within canonical
	needSummary := false     // some entry has no cell snapshot (pre-merge save)
	for _, e := range entries {
		if len(e.Cells) == 0 {
			needSummary = true
		}
		for _, h := range e.DisplayHeaders {
			if h == "" {
				continue
			}
			if _, ok := seen[h]; !ok {
				seen[h] = len(canonical)
				canonical = append(canonical, h)
			}
		}
	}

	headers = append([]string(nil), fixed...)
	headers = append(headers, canonical...)
	if needSummary {
		// Keep the content of rows snapshotted before the merge feature visible;
		// they carry no cells but do carry a summary. Re-saving each timeline
		// rebuilds its snapshot with cells.
		headers = append(headers, "Summary")
	}

	records = make([][]string, len(entries))
	for i, e := range entries {
		tval := e.TimeRaw
		if e.HasTime {
			// time_unix is stored in UTC; render it in UTC so forensic
			// timestamps are not shifted by the viewer's local zone.
			tval = e.Time.UTC().Format("2006-01-02 15:04:05.000")
		}
		rec := []string{tval, e.Timeline, strings.Join(e.Tags, ", "), e.Comment}
		// Place each cell under its canonical column via this entry's own
		// display headers.
		cells := make([]string, len(canonical))
		for j, h := range e.DisplayHeaders {
			if j >= len(e.Cells) {
				break
			}
			if ci, ok := seen[h]; ok {
				cells[ci] = e.Cells[j]
			}
		}
		rec = append(rec, cells...)
		if needSummary {
			rec = append(rec, e.Summary)
		}
		records[i] = rec
	}
	return headers, records
}

// openMasterSource opens the timeline behind a master-view row at that row.
func (a *App) openMasterSource(viewRow int) {
	if !a.masterMode || a.cse == nil {
		return
	}
	if viewRow < 0 || viewRow >= a.view.Len() {
		return
	}
	master := a.view.Master(viewRow)
	if master < 0 || master >= len(a.masterEntries) {
		return
	}
	e := a.masterEntries[master]
	tl, err := a.cse.Timeline(e.TimelineID)
	if err != nil {
		a.showError(err)
		return
	}
	targetRow := e.Row
	a.openTimeline(tl)
	// Scroll to the originating row once the timeline is shown.
	if !a.masterMode {
		a.selectMasterRow(targetRow)
	}
}

// selectMasterRow scrolls to and selects the view position of a master row in
// the currently open timeline (best effort; the row may be filtered out).
func (a *App) selectMasterRow(master int) {
	for pos := 0; pos < a.view.Len(); pos++ {
		if a.view.Master(pos) == master {
			id := widget.TableCellID{Row: pos, Col: 0}
			a.table.ScrollTo(id)
			a.table.Select(id)
			return
		}
	}
}
