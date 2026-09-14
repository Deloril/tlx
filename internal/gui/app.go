// Package gui is the Fyne front end for the Timeline explorer. It depends on a
// C/OpenGL toolchain (Fyne), unlike internal/model, which is pure Go.
package gui

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/casefile"
	"tlx/internal/model"
)

// column is one logical column shown in the grid: either a virtual annotation
// column or a data column from the CSV.
type column struct {
	ref     model.ColumnRef
	title   string
	visible bool
	width   float32
}

// App is the running explorer over a single file.
type App struct {
	fyne fyne.App
	win  fyne.Window

	idx  *model.Index
	sess *model.Session
	view *model.View

	// Case context. When cse is non-nil the app is working inside a case:
	// curTimeline names the open timeline (nil in the master view), masterMode
	// is true while showing the cross-timeline master timeline, and masterEntries
	// backs that view's rows.
	cse           *casefile.Case
	curTimeline   *casefile.TimelineMeta
	masterMode    bool
	masterEntries []casefile.MasterEntry
	caseMenu      *fyne.Menu

	// annotCols controls whether the virtual #/Tags/Comment columns are built.
	// True for CSV and timeline views, false for the master timeline (whose
	// tags and comment are ordinary data columns).
	annotCols bool

	// adopted names the source columns of the open timeline that already hold
	// tags/comments and have been taken over as the session's annotations. Those
	// data columns are hidden (the virtual Tags/Comment columns stand in for
	// them) and dropped from an export. Both -1 when none, or in master mode.
	adopted model.AdoptedColumns

	cols      []column        // all columns, in display order
	visible   []int           // indices into cols that are currently shown
	sortState []model.SortKey // mirror of view sort keys, for header arrows

	// colNames overrides the data-column titles by source index, used inside a
	// case so renamed timeline columns show (and merge) under their new names.
	// Empty means fall back to the index headers.
	colNames []string

	table *bigTable
	split *container.Split // table | right dock

	// Right dock: collapsible, floatable panels (see dock.go) stacked in a
	// scrollable column. filterPanel holds the query/conditions UI (was a
	// separate window); detailPanel holds the selected-row detail. rightPanels
	// is the display order; rightDockBox is the VBox of docked cards.
	filterPanel     *dockPanel
	detailPanel     *dockPanel
	notesPanel      *dockPanel
	commentsPanel   *dockPanel
	rightPanels     []*dockPanel
	rightDockBox    *fyne.Container
	rightScroll     *container.Scroll
	rightDockOffset float64
	detail          *fyne.Container // detail panel body, filled by showDetail

	// Reusable Details-panel widgets. showDetail builds the field widgets once per
	// "shape" (mode + column set, see detailShape) and updates their text on each
	// selection, so clicking around does not reconstruct dozens of Entry widgets.
	detailShape        string
	detailMaster       int
	detailAcc          *widget.Accordion
	detailHeadLabel    *widget.Label
	detailHeadBtn      *widget.Button   // master view "Open in <timeline>"; nil otherwise
	detailTagsBox      *fyne.Container  // Tags section body, repopulated per row
	detailCommentEntry *growEntry       // writable comment box; nil in read-only
	detailCommentLabel *widget.Label    // read-only comment; nil when writable
	detailFields       []detailField

	// Investigator's notes (per-timeline Artifacts/Times) and the timeline
	// comments field, both in the right dock.
	notesArtifactsBox *fyne.Container
	notesTimesBox     *fyne.Container
	noteAddArtifact   *widget.Entry // manual entry for the Artifacts list
	noteAddTime       *widget.Entry // manual entry for the Times list
	commentEntry      *growEntry
	suppressComment   bool // set while loading the comment field, to swallow OnChanged

	// Filter UI state. filterConds holds the structured per-column condition
	// rows; combineSel picks AND/OR.
	filterConds    *fyne.Container
	filterRows     []*filterRow
	combineSel     *widget.RadioGroup
	suppressFilter bool // set while resetting filter widgets, to swallow callbacks

	// Left sidebar: three collapsible sections (Views, Case, IOC lists) stacked
	// in a scrollable column, held in outerSplit beside the main content.
	outerSplit *container.Split // left sidebar | split
	leftBox    *fyne.Container  // VBox of the three section cards
	leftScroll *container.Scroll
	viewsPanel *dockPanel
	casePanel  *dockPanel
	iocPanel   *dockPanel
	viewsList  *fyne.Container // repopulated list of saved-view rows
	caseList   *fyne.Container // repopulated case/timeline controls
	iocListBox *fyne.Container // repopulated IOC-list rows

	search     *widget.Entry
	modeSelect *widget.Select
	taggedChk  *widget.Check
	caseChk    *widget.Check
	themeBtn   *widget.Button

	statusMode   *widget.Label
	statusRows   *widget.Label
	statusFilter *widget.Label
	statusDirty  *widget.Label
	statusBG     *canvas.Rectangle // status-bar fill; re-coloured on theme toggle

	selRow int // active view row (drives detail pane and inline edit), -1 if none
	selCol int // active table column, -1 if none

	// Multi-selection. selected holds the set of selected rows keyed by master
	// index, so a selection survives sorting and filtering. anchorView is the
	// view-row index a shift-click ranges from; hoverRow is the row last under the
	// pointer, used to seat the right-click menu.
	selected   map[int]bool
	anchorView int
	hoverRow   int
	hoverCol   int // column last under the pointer, for column-aware context actions

	// Double-click detection for the grid. Fyne's Table isn't DoubleTappable and
	// wiring the interface in would delay every single click by the double-tap
	// window; instead we time consecutive presses on the same cell ourselves.
	lastClickAt  time.Time
	lastClickRow int
	lastClickCol int

	// Header drag-reorder state. Like the double-click fields, it lives on the App
	// rather than the header widget: widget.Table rebinds header cells to columns
	// as it refreshes, so the button under the pointer can't be trusted to still
	// represent the column being dragged. dragHdrPos is the dragged column's
	// current display position; dragHdrAccum banks horizontal drag distance since
	// the last swap.
	dragHdrActive bool
	dragHdrPos    int
	dragHdrAccum  float32

	sidebarVisible      bool // right dock (filter + detail panels)
	viewsSidebarVisible bool // left sidebar (views + case + IOC sections)
	themeVariant        fyne.ThemeVariant

	// Per-column filter conditions, kept so the search box and column filter
	// compose and both survive a re-apply.
	conds    []model.ColumnCond
	condsAny bool

	// colFilter holds per-column quick-filter text keyed by column ref, populated
	// by the boxes under each column header and by saved views' col_filters.
	colFilter map[model.ColumnRef]string

	// tagFilter is the set of tag names ticked in the Tags dropdown (filter
	// window). A row is kept if it carries any of them (OR). Empty means the tag
	// dropdown imposes no filter.
	tagFilter map[string]bool

	// showFilters toggles the per-column filter boxes under the headers. Off by
	// default: headers are a single row and data rows stay compact. On: each
	// header grows a filter box beneath its title, which makes data rows taller
	// (Fyne sizes cells to max(cell, header) template height). filterRowBtn is the
	// toolbar toggle, restyled to show the current state.
	showFilters  bool
	filterRowBtn *widget.Button

	// hl marks the substrings the active filter matches, so the grid can
	// highlight them. Rebuilt on every apply; nil or empty means no highlight.
	hl *model.Highlighter

	// Inline cell editing state (view coordinates).
	editing     bool
	editRow     int
	editCol     int
	editFocused bool

	// Hover tooltip overlay.
	hoverLayer *fyne.Container
	hoverBG    *canvas.Rectangle
	hoverText  *widget.RichText

	startMode model.Mode // mode applied to files opened via the dialog

	// Autosave. Annotation edits arm a short debounce timer (autosaveMu guards
	// it) so a burst of changes — a bulk tag, an IOC run — collapses into one
	// write. closing is set on shutdown so a late timer can't fire into a
	// half-torn-down app.
	autosaveMu    sync.Mutex
	autosaveTimer *time.Timer
	closing       bool
}

// autosaveDelay is how long after the last annotation change the autosave fires.
// Long enough to coalesce a burst, short enough that little is at risk if the
// process dies.
const autosaveDelay = 600 * time.Millisecond

// New builds an explorer with no file loaded yet. Call OpenInitial to load one,
// or let the user open one from the toolbar; either way Run shows the window.
func New() *App {
	a := &App{
		fyne:                app.NewWithID("nz.timeline.explorer"),
		selRow:              -1,
		selCol:              -1,
		selected:            map[int]bool{},
		anchorView:          -1,
		hoverRow:            -1,
		hoverCol:            -1,
		lastClickRow:        -1,
		lastClickCol:        -1,
		sidebarVisible:      false, // detail pane starts collapsed; Ctrl+B reveals it
		viewsSidebarVisible: false, // saved-views pane starts collapsed; Ctrl+L reveals it
		themeVariant:        theme.VariantDark,
		startMode:           model.Investigator,
		colFilter:           map[model.ColumnRef]string{},
		tagFilter:           map[string]bool{},
	}
	a.fyne.Settings().SetTheme(newCompactTheme(a.themeVariant))
	a.fyne.SetIcon(appIcon)
	a.win = a.fyne.NewWindow("tlx")
	a.win.SetIcon(appIcon)
	a.win.Resize(fyne.NewSize(1280, 760))
	a.win.SetCloseIntercept(a.onClose)
	a.win.SetMainMenu(a.buildMainMenu())
	a.buildFilterWidgets()
	a.buildHoverLayer()
	a.registerShortcuts()
	a.showPlaceholder()
	return a
}

// buildFilterWidgets creates the freetext query box and its checkboxes once, so
// they keep their state and callbacks whether the filter window is open, closed
// or being rebuilt for a new file.
func (a *App) buildFilterWidgets() {
	a.search = widget.NewMultiLineEntry()
	a.search.SetPlaceHolder("text, or field=value AND (tag=bad OR tag=suspicious). /regex/ for regex. Enter to apply, Shift+Enter for newline")
	a.search.Wrapping = fyne.TextWrapWord
	// At least five rows on open; the multi-line entry grows past that as more
	// lines are added.
	a.search.SetMinRowsVisible(5)
	a.search.OnSubmitted = func(string) { a.commitFilter() }
	a.taggedChk = widget.NewCheck("Tagged only", func(bool) { a.commitFilter() })
	a.caseChk = widget.NewCheck("Case sensitive", func(bool) { a.commitFilter() })
}

// SetStartMode sets the mode used for files opened through the file dialog.
func (a *App) SetStartMode(m model.Mode) { a.startMode = m }

// OpenInitial loads an already-opened index and session into the window.
func (a *App) OpenInitial(idx *model.Index, sess *model.Session) {
	a.cse, a.curTimeline, a.masterMode = nil, nil, false
	a.annotCols = true
	a.colNames = nil
	a.reloadWith(idx, sess)
}

// showPlaceholder renders the empty state with buttons to open a standalone
// timeline, open an existing case, or start a new one.
func (a *App) showPlaceholder() {
	openTL := widget.NewButtonWithIcon("Open timeline…", theme.FolderOpenIcon(), a.openFile)
	openCase := widget.NewButtonWithIcon("Open case…", theme.StorageIcon(), a.openCase)
	newCase := widget.NewButtonWithIcon("New case…", theme.ContentAddIcon(), a.newCase)
	hint := widget.NewLabel("Open a forensic CSV timeline, or open or create a case to group several.")
	buttons := container.NewHBox(openTL, openCase, newCase)
	a.win.SetContent(container.NewCenter(container.NewVBox(hint, container.NewCenter(buttons))))
}

func (a *App) buildColumns() {
	a.cols = nil
	if a.annotCols {
		a.cols = append(a.cols,
			column{ref: model.ColRowNum, title: "#", visible: true, width: 72},
			column{ref: model.ColTags, title: "✎ Tags", visible: true, width: 160},
			column{ref: model.ColComment, title: "✎ Comment", visible: true, width: 220},
		)
	}
	for i, h := range a.idx.Headers() {
		// A column adopted as the session's tags/comments is represented by the
		// virtual Tags/Comment column, so don't show the raw source column.
		if a.annotCols && a.adopted.Has(i) {
			continue
		}
		title := h
		if i < len(a.colNames) && a.colNames[i] != "" {
			title = a.colNames[i]
		}
		a.cols = append(a.cols, column{ref: model.ColumnRef(i), title: title, visible: true, width: fitColWidth(title)})
	}
	a.rebuildVisible()
}

// Header text metrics. headerPad covers the sort button's inner padding either
// side of the label; sortArrowW reserves room for the " ▲"/"▼" sort indicator so
// sorting a column never truncates its title.
const (
	headerPad  = 24
	sortArrowW = 18
)

// fitColWidth is the starting width for a column: the by-name default, widened if
// the title text (plus padding and sort-arrow room) needs more, so the header
// never spills into the next column on open.
func fitColWidth(title string) float32 {
	base := colWidth(title)
	need := fyne.MeasureText(title, theme.TextSize(), fyne.TextStyle{}).Width + headerPad + sortArrowW
	if need > base {
		return need
	}
	return base
}

// ellipsizeToWidth trims s to fit max pixels, appending "…" when it has to cut.
// Used to keep a header title inside a column narrowed below its natural width.
func ellipsizeToWidth(s string, max float32) string {
	if max <= 0 {
		return ""
	}
	if fyne.MeasureText(s, theme.TextSize(), fyne.TextStyle{}).Width <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		if fyne.MeasureText(string(r)+"…", theme.TextSize(), fyne.TextStyle{}).Width <= max {
			return string(r) + "…"
		}
	}
	return "…"
}

// colWidth picks a starting width for a column by its header name.
func colWidth(h string) float32 {
	switch h {
	case "Summary", "Message":
		return 520
	case "Time", "Timestamp":
		return 180
	case "Timeline", "Tags":
		return 150
	case "Comment":
		return 240
	default:
		return 140
	}
}

func (a *App) rebuildVisible() {
	a.visible = a.visible[:0]
	for i := range a.cols {
		if a.cols[i].visible {
			a.visible = append(a.visible, i)
		}
	}
}

func (a *App) buildUI() {
	a.table = a.newTable()

	a.rightDockOffset = 0.72
	a.buildRightDock()
	a.buildLeftSidebar()

	a.split = container.NewHSplit(a.table, a.rightScroll)
	a.split.Offset = a.rightDockOffset

	a.outerSplit = container.NewHSplit(a.leftScroll, a.split)
	a.outerSplit.Offset = viewsSidebarOffset

	body := container.NewBorder(a.buildToolbar(), a.buildStatusBar(), nil, nil, a.outerSplit)
	// The hover layer floats above everything but captures no input.
	content := container.NewStack(body, a.hoverLayer)
	a.win.SetContent(content)
	a.win.Resize(fyne.NewSize(1280, 760))
	if !a.viewsSidebarVisible {
		a.leftScroll.Hide()
		a.outerSplit.SetOffset(0.0)
	}
	if !a.sidebarVisible {
		a.rightScroll.Hide()
		a.split.SetOffset(1.0)
	}
	a.refreshStatus()
}

// buildRightDock creates the Filter and Details panels and the scrollable
// column that stacks whichever of them are docked.
func (a *App) buildRightDock() {
	// Close any float windows left over from a previous file before rebuilding
	// the panels, so a reload doesn't orphan them.
	for _, p := range a.rightPanels {
		if p != nil && p.win != nil {
			p.win.SetContent(container.NewWithoutLayout())
			p.win.Close()
			p.win = nil
		}
	}
	a.detail = container.NewVBox(widget.NewLabel("Select a row to see details."))

	a.filterPanel = newDockPanel(a, "Filter", true)
	a.filterPanel.onChange = a.refreshRightDock
	a.filterPanel.setBody(a.buildFilterContent())

	a.detailPanel = newDockPanel(a, "Details", true)
	a.detailPanel.onChange = a.refreshRightDock
	a.detailPanel.setBody(a.detail)

	a.notesPanel = newDockPanel(a, "Investigator's notes", true)
	a.notesPanel.onChange = a.refreshRightDock
	a.notesPanel.setBody(a.buildNotesContent())

	a.commentsPanel = newDockPanel(a, "Timeline comments", true)
	a.commentsPanel.onChange = a.refreshRightDock
	a.commentsPanel.setBody(a.buildCommentsContent())

	a.rightPanels = []*dockPanel{a.filterPanel, a.detailPanel, a.notesPanel, a.commentsPanel}
	a.rightDockBox = container.NewVBox()
	a.rightScroll = container.NewVScroll(a.rightDockBox)
	a.rightScroll.SetMinSize(fyne.NewSize(340, 100))
	a.refreshRightDock()
}

// refreshRightDock rebuilds the docked-card list from the panels that are not
// floating, and reveals or hides the right pane so it never shows as an empty
// strip when every panel has floated out.
func (a *App) refreshRightDock() {
	if a.rightDockBox == nil {
		return
	}
	a.rightDockBox.Objects = nil
	for _, p := range a.rightPanels {
		if !p.floating {
			a.rightDockBox.Objects = append(a.rightDockBox.Objects, p.root)
		}
	}
	a.rightDockBox.Refresh()
	if a.split == nil {
		return
	}
	if len(a.rightDockBox.Objects) == 0 { // all floated out
		a.rightScroll.Hide()
		a.split.SetOffset(1.0)
	} else if a.sidebarVisible {
		a.rightScroll.Show()
		a.split.SetOffset(a.rightDockOffset)
	}
	a.split.Refresh()
}

// buildLeftSidebar creates the three collapsible sections (Views, Case, IOC
// lists) and the scrollable column that stacks them.
func (a *App) buildLeftSidebar() {
	a.viewsPanel = newDockPanel(a, "Saved views", false)
	a.viewsPanel.onChange = a.refreshLeftSidebar
	a.viewsPanel.setBody(a.buildViewsSection())

	a.casePanel = newDockPanel(a, "Case", false)
	a.casePanel.onChange = a.refreshLeftSidebar
	a.casePanel.setBody(a.buildCaseSection())

	a.iocPanel = newDockPanel(a, "IOC lists", false)
	a.iocPanel.onChange = a.refreshLeftSidebar
	a.iocPanel.setBody(a.buildIOCSection())

	a.leftBox = container.NewVBox(a.viewsPanel.root, a.casePanel.root, a.iocPanel.root)
	a.leftScroll = container.NewVScroll(a.leftBox)
}

// refreshLeftSidebar re-lays-out the left column after a section collapses or
// its contents change.
func (a *App) refreshLeftSidebar() {
	if a.leftBox != nil {
		a.leftBox.Refresh()
	}
}

// reloadWith swaps the open file, keeping the same window and shortcuts.
func (a *App) reloadWith(idx *model.Index, sess *model.Session) {
	// Drop any autosave still pending for the outgoing session; the context
	// switch that got us here already flushed it (see confirmIfDirty).
	a.autosaveMu.Lock()
	if a.autosaveTimer != nil {
		a.autosaveTimer.Stop()
		a.autosaveTimer = nil
	}
	a.autosaveMu.Unlock()
	if a.idx != nil {
		a.idx.Close()
	}
	a.idx, a.sess = idx, sess
	a.view = model.NewView(idx, sess)
	// Detect adopted tag/comment columns only where the virtual annotation
	// columns exist; the master timeline has its own real Tags/Comment columns.
	if a.annotCols {
		a.adopted = model.DetectAnnotationColumns(idx.Headers())
	} else {
		a.adopted = model.AdoptedColumns{Tag: -1, Comment: -1}
	}
	a.sortState = nil
	a.selRow, a.selCol = -1, -1
	a.selected = map[int]bool{}
	a.anchorView, a.hoverRow, a.hoverCol = -1, -1, -1
	a.editing, a.editFocused = false, false
	a.hideTooltip()
	a.win.SetTitle(a.windowTitle())
	a.buildColumns()
	a.buildUI()
	a.modeSelect.SetSelected(a.sess.Mode().String())
	a.resetFilterState() // a fresh file starts unfiltered; clears the query box too
}

// resetFilterState clears the active filter and its widgets without triggering
// their change callbacks, then rebuilds the docked filter panel.
func (a *App) resetFilterState() {
	a.suppressFilter = true
	a.conds = nil
	a.condsAny = false
	a.colFilter = map[model.ColumnRef]string{}
	a.tagFilter = map[string]bool{}
	a.hl = nil
	if a.search != nil {
		a.search.SetText("")
	}
	if a.taggedChk != nil {
		a.taggedChk.SetChecked(false)
	}
	if a.caseChk != nil {
		a.caseChk.SetChecked(false)
	}
	a.suppressFilter = false
	a.rebuildFilterPanel() // reflect the cleared conditions in the docked filter UI
}

// windowTitle reflects the current context: master view, a named timeline in an
// case, or a standalone file.
func (a *App) windowTitle() string {
	const base = "tlx"
	switch {
	case a.masterMode && a.cse != nil:
		return base + " — Master timeline [" + filepath.Base(a.cse.Path()) + "]"
	case a.curTimeline != nil && a.cse != nil:
		return base + " — " + a.curTimeline.Name + " [" + filepath.Base(a.cse.Path()) + "]"
	case a.idx != nil:
		return base + " — " + a.idx.Path()
	default:
		return base
	}
}

func (a *App) buildToolbar() fyne.CanvasObject {
	openBtn := widget.NewButtonWithIcon("Open", theme.FolderOpenIcon(), a.openFile)
	saveBtn := widget.NewButtonWithIcon("Save", theme.DocumentSaveIcon(), a.save)
	exportBtn := widget.NewButtonWithIcon("Export", theme.DownloadIcon(), nil)
	exportBtn.OnTapped = func() { a.showExportMenu(exportBtn) }

	a.modeSelect = widget.NewSelect(
		[]string{model.ReadOnly.String(), model.Investigator.String(), model.WorldWrite.String()},
		a.onModeChange,
	)
	a.modeSelect.SetSelected(a.sess.Mode().String())

	tagBtn := widget.NewButtonWithIcon("Tag", theme.ContentAddIcon(), a.tagSelected)
	commentBtn := widget.NewButtonWithIcon("Comment", theme.MailComposeIcon(), a.commentSelected)
	detailsBtn := widget.NewButtonWithIcon("Details", theme.InfoIcon(), a.revealDetailsPanel)
	notesBtn := widget.NewButtonWithIcon("Notes", theme.DocumentIcon(), a.revealNotesPanel)
	viewsBtn := widget.NewButtonWithIcon("Views", theme.ListIcon(), a.toggleViewsSidebar)
	filterBtn := widget.NewButtonWithIcon("Filter", theme.SearchIcon(), a.revealFilterPanel)
	clearBtn := widget.NewButtonWithIcon("Clear filters", theme.ContentClearIcon(), a.clearFilter)
	a.filterRowBtn = widget.NewButtonWithIcon("Filter row", theme.VisibilityIcon(), a.toggleFilterRow)
	a.updateFilterRowButton()
	colsBtn := widget.NewButtonWithIcon("Columns", theme.ViewFullScreenIcon(), a.columnPicker)
	sidebarBtn := widget.NewButtonWithIcon("Panels", theme.MenuIcon(), a.toggleSidebar)
	a.themeBtn = widget.NewButtonWithIcon("", theme.ColorPaletteIcon(), a.toggleTheme)
	a.updateThemeButton()
	helpBtn := widget.NewButtonWithIcon("Help", theme.HelpIcon(), a.showHelp)

	left := container.NewHBox(viewsBtn, openBtn, saveBtn, exportBtn, widget.NewSeparator(),
		widget.NewLabel("Mode:"), a.modeSelect, widget.NewSeparator(),
		tagBtn, commentBtn, detailsBtn, notesBtn, widget.NewSeparator(), filterBtn, a.filterRowBtn, clearBtn)
	right := container.NewHBox(sidebarBtn, a.themeBtn, colsBtn, helpBtn)
	return container.NewBorder(nil, nil, left, right, nil)
}

func (a *App) buildStatusBar() fyne.CanvasObject {
	a.statusMode = widget.NewLabel("")
	a.statusRows = widget.NewLabel("")
	a.statusFilter = widget.NewLabel("")
	a.statusDirty = widget.NewLabel("")
	a.statusBG = canvas.NewRectangle(theme.Color(theme.ColorNameInputBackground))
	bar := container.NewHBox(a.statusMode, widget.NewSeparator(), a.statusRows,
		widget.NewSeparator(), a.statusFilter, widget.NewSeparator(), a.statusDirty)
	return container.NewStack(a.statusBG, bar)
}

func (a *App) refreshStatus() {
	// May be called before the status bar or a file exists (e.g. the mode
	// Select fires its callback during construction, or in the empty state).
	if a.statusMode == nil || a.sess == nil || a.idx == nil || a.view == nil {
		return
	}
	m := a.sess.Mode()
	a.statusMode.SetText("Mode: " + m.String())
	a.statusRows.SetText(fmt.Sprintf("Rows: %d shown / %d total", a.view.Len(), a.idx.RowCount()))
	f := a.view.Filter()
	switch {
	case !f.Empty():
		a.statusFilter.SetText("Filter: active")
	default:
		a.statusFilter.SetText("Filter: none")
	}
	if a.sess.Dirty() {
		a.statusDirty.SetText("● unsaved")
	} else {
		a.statusDirty.SetText("saved")
	}
}

// selectedMaster returns the master row index of the current selection, or -1.
func (a *App) selectedMaster() int {
	if a.selRow < 0 || a.selRow >= a.view.Len() {
		return -1
	}
	return a.view.Master(a.selRow)
}

func (a *App) refreshTable() {
	a.table.Refresh()
	a.refreshStatus()
	a.scheduleAutosave()
}

// scheduleAutosave arms (or re-arms) the debounce timer whenever there are
// unsaved annotations. It is called from refreshTable, which runs after every
// annotation change, so any edit path schedules a save; a run of edits just
// keeps pushing the timer out, collapsing into one write. Cheap and a no-op
// when nothing is dirty.
func (a *App) scheduleAutosave() {
	if a.sess == nil || !a.sess.Dirty() || a.masterMode {
		return
	}
	a.autosaveMu.Lock()
	defer a.autosaveMu.Unlock()
	if a.closing {
		return
	}
	if a.autosaveTimer != nil {
		a.autosaveTimer.Stop()
	}
	a.autosaveTimer = time.AfterFunc(autosaveDelay, func() {
		fyne.Do(a.flushAutosave)
	})
}

// flushAutosave persists annotations now: to the case database inside a case, or
// to the sidecar file for a standalone timeline. Errors surface in the status
// bar rather than a modal, so a transient failure doesn't interrupt work. Runs
// on the UI goroutine (via fyne.Do or a direct call from onClose).
func (a *App) flushAutosave() {
	if a.sess == nil || a.masterMode || !a.sess.Dirty() {
		return
	}
	var err error
	switch {
	case a.cse != nil:
		if a.curTimeline == nil {
			return // master view or no timeline open; nothing to persist
		}
		if err = a.cse.SaveAnnotations(*a.curTimeline, a.sess.Snapshot(), a.idx); err == nil {
			a.sess.MarkSaved()
		}
	case a.idx != nil:
		err = a.sess.Save() // writes the <file>.tlx.json sidecar; clears dirty
	default:
		return
	}
	if err != nil {
		if a.statusDirty != nil {
			a.statusDirty.SetText("● autosave failed")
		}
		return
	}
	a.refreshStatus()
}

// toggleFilterRow shows or hides the per-column filter boxes under the headers.
// The header template determines both the header and data-row height, so the
// table has to be rebuilt for the change to take. Active filters live in
// a.colFilter, not in the widgets, so they survive the toggle either way.
func (a *App) toggleFilterRow() {
	a.showFilters = !a.showFilters
	a.updateFilterRowButton()
	a.rebuildTable()
}

// updateFilterRowButton restyles the toolbar toggle to reflect whether the
// filter row is showing.
func (a *App) updateFilterRowButton() {
	if a.filterRowBtn == nil {
		return
	}
	if a.showFilters {
		a.filterRowBtn.Importance = widget.HighImportance
	} else {
		a.filterRowBtn.Importance = widget.MediumImportance
	}
	a.filterRowBtn.Refresh()
}

// rebuildTable recreates the grid in place, e.g. after toggling the filter row.
// The view, selection and column state all live on the App, so the fresh table
// picks them up on its first refresh.
func (a *App) rebuildTable() {
	if a.split == nil {
		return
	}
	a.table = a.newTable()
	a.split.Leading = a.table
	a.split.Refresh()
	a.refreshTable()
}

// Run shows the window and blocks until it closes.
func (a *App) Run() {
	a.win.ShowAndRun()
}
