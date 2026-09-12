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

	"timeline-engine/internal/casefile"
	"timeline-engine/internal/model"
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
		hint := fyne.NewMenuItem("(open or create a case)", nil)
		hint.Disabled = true
		return []*fyne.MenuItem{hint}
	}
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("★ Master timeline", a.showMasterTimeline),
		fyne.NewMenuItem("Add timeline…", a.addTimelineToCase),
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
	a.reloadWith(idx, sess)
	a.rebuildCaseMenu()
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
		headers := []string{"Time", "Timeline", "Tags", "Comment", "Summary"}
		records := make([][]string, len(entries))
		for i, e := range entries {
			tval := e.TimeRaw
			if e.HasTime {
				// time_unix is stored in UTC; render it in UTC so forensic
				// timestamps are not shifted by the viewer's local zone.
				tval = e.Time.UTC().Format("2006-01-02 15:04:05.000")
			}
			records[i] = []string{tval, e.Timeline, strings.Join(e.Tags, ", "), e.Comment, e.Summary}
		}
		idx := model.NewMemoryIndex(headers, records)
		sess := model.NewSession("(master)")
		sess.SetMode(model.ReadOnly)

		a.masterEntries = entries
		a.curTimeline = nil
		a.masterMode = true
		a.annotCols = false
		a.reloadWith(idx, sess)
		a.rebuildCaseMenu()
		if len(entries) == 0 {
			dialog.ShowInformation("Master timeline",
				"No tagged rows yet. Tag rows in a timeline and save to populate the master view.", a.win)
		}
	})
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
