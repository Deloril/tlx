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

	"timeline-engine/internal/incident"
	"timeline-engine/internal/model"
)

// buildMainMenu assembles the window menu bar. The Incident menu is rebuilt from
// the current incident state each time this is called.
func (a *App) buildMainMenu() *fyne.MainMenu {
	fileMenu := fyne.NewMenu("File",
		fyne.NewMenuItem("Open CSV…", a.openFile),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("New incident…", a.newIncident),
		fyne.NewMenuItem("Open incident…", a.openIncident),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Save", a.save),
		fyne.NewMenuItem("Export view…", a.export),
	)

	a.incidentMenu = fyne.NewMenu("Incident", a.incidentMenuItems()...)

	viewsMenu := fyne.NewMenu("Views", a.viewsMenuItems()...)

	return fyne.NewMainMenu(fileMenu, a.incidentMenu, viewsMenu)
}

// incidentMenuItems lists the master timeline, an add action and every
// registered timeline, or a hint when no incident is open.
func (a *App) incidentMenuItems() []*fyne.MenuItem {
	if a.inc == nil {
		hint := fyne.NewMenuItem("(open or create an incident)", nil)
		hint.Disabled = true
		return []*fyne.MenuItem{hint}
	}
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("★ Master timeline", a.showMasterTimeline),
		fyne.NewMenuItem("Add timeline…", a.addTimelineToIncident),
		fyne.NewMenuItemSeparator(),
	}
	tls, err := a.inc.Timelines()
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

// rebuildIncidentMenu refreshes the whole menu bar so the Incident submenu
// reflects the current incident and open timeline.
func (a *App) rebuildIncidentMenu() {
	a.win.SetMainMenu(a.buildMainMenu())
}

// clearView tears down any open file/timeline and shows a placeholder. Used when
// an incident is created or opened but no timeline is loaded yet, so the grid
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

	msg := widget.NewLabel("Add a timeline from the Incident menu to begin.")
	add := widget.NewButtonWithIcon("Add timeline…", theme.ContentAddIcon(), a.addTimelineToIncident)
	a.win.SetContent(container.NewCenter(container.NewVBox(msg, container.NewCenter(add))))
}

// setIncident swaps the active incident, closing any previous one.
func (a *App) setIncident(inc *incident.Incident) {
	if a.inc != nil {
		a.inc.Close()
	}
	a.inc = inc
	a.curTimeline = nil
	a.masterMode = false
	a.rebuildIncidentMenu()
}

func (a *App) newIncident() {
	d := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
		if err != nil || w == nil {
			return
		}
		path := w.URI().Path()
		w.Close() // Create opens its own handle; an empty file is a valid empty DB
		if filepath.Ext(path) == "" {
			path += ".tlxdb"
		}
		inc, err := incident.Create(path)
		if err != nil {
			a.showError(err)
			return
		}
		a.setIncident(inc)
		a.clearView()
	}, a.win)
	d.SetFileName("incident.tlxdb")
	d.Show()
}

func (a *App) openIncident() {
	dialog.ShowFileOpen(func(r fyne.URIReadCloser, err error) {
		if err != nil || r == nil {
			return
		}
		path := r.URI().Path()
		r.Close()
		inc, err := incident.Open(path)
		if err != nil {
			a.showError(err)
			return
		}
		a.setIncident(inc)
		tls, _ := inc.Timelines()
		if len(tls) == 0 {
			a.clearView()
			return
		}
		a.openTimeline(tls[0])
	}, a.win)
}

func (a *App) addTimelineToIncident() {
	if a.inc == nil {
		a.showError(fmt.Errorf("open or create an incident first"))
		return
	}
	a.confirmIfDirty(func() {
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
			name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			tl, err := a.inc.AddTimeline(name, idx, time.Now().Format(time.RFC3339))
			if err != nil {
				idx.Close()
				a.showError(err)
				return
			}
			// Open it straight away, reusing the index we just indexed.
			a.openTimelineWithIndex(tl, idx)
		}, a.win)
	})
}

// openTimeline opens a registered timeline by (re)indexing its source file.
func (a *App) openTimeline(tl incident.TimelineMeta) {
	a.confirmIfDirty(func() {
		idx, err := model.Open(tl.SourcePath, nil)
		if err != nil {
			a.showError(fmt.Errorf("open %s: %w", tl.SourcePath, err))
			return
		}
		a.openTimelineWithIndex(tl, idx)
	})
}

// openTimelineWithIndex loads a timeline's annotations from the incident onto an
// already-opened index and shows it. It does not prompt about unsaved changes;
// callers that switch context should have done so already.
func (a *App) openTimelineWithIndex(tl incident.TimelineMeta, idx *model.Index) {
	sess := model.NewSession(tl.SourcePath)
	if snap, err := a.inc.LoadAnnotations(tl.ID); err != nil {
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
	a.rebuildIncidentMenu()
}

// showMasterTimeline builds a read-only, time-sorted view of every tagged row
// across all timelines in the incident.
func (a *App) showMasterTimeline() {
	if a.inc == nil {
		return
	}
	a.confirmIfDirty(func() {
		entries, err := a.inc.Master()
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
		a.rebuildIncidentMenu()
		if len(entries) == 0 {
			dialog.ShowInformation("Master timeline",
				"No tagged rows yet. Tag rows in a timeline and save to populate the master view.", a.win)
		}
	})
}

// openMasterSource opens the timeline behind a master-view row at that row.
func (a *App) openMasterSource(viewRow int) {
	if !a.masterMode || a.inc == nil {
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
	tl, err := a.inc.Timeline(e.TimelineID)
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
