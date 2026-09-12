// Package gui is the Fyne front end for the Timeline explorer. It depends on a
// C/OpenGL toolchain (Fyne), unlike internal/model, which is pure Go.
package gui

import (
	"fmt"
	"path/filepath"

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

	table  *bigTable
	detail *fyne.Container
	scroll *container.Scroll
	split  *container.Split // table | detail

	// Standalone, non-modal filter window (see filterwin.go). filterConds holds
	// the structured per-column condition rows; combineSel picks AND/OR.
	filterWin      fyne.Window
	filterWinShown bool
	filterConds    *fyne.Container
	filterRows     []*filterRow
	combineSel     *widget.RadioGroup
	suppressFilter bool // set while resetting filter widgets, to swallow callbacks

	// Left sidebar listing saved views, and the split that holds it beside the
	// main content. viewsList is the repopulated list of view rows.
	outerSplit *container.Split // viewsPanel | split
	viewsPanel *fyne.Container
	viewsList  *fyne.Container

	search     *widget.Entry
	modeSelect *widget.Select
	taggedChk  *widget.Check
	caseChk    *widget.Check
	themeBtn   *widget.Button

	statusMode   *widget.Label
	statusRows   *widget.Label
	statusFilter *widget.Label
	statusDirty  *widget.Label

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

	sidebarVisible      bool // right detail pane
	viewsSidebarVisible bool // left saved-views pane
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

	// iocList holds the IOC list for a standalone (non-case) timeline. Inside a
	// case the list lives on the case instead. Loaded from and saved to a
	// per-file preference (see ioc.go).
	iocList string

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
}

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
		sidebarVisible:      false, // detail pane starts collapsed; Ctrl+B reveals it
		viewsSidebarVisible: false, // saved-views pane starts collapsed; Ctrl+L reveals it
		themeVariant:        theme.VariantDark,
		startMode:           model.Investigator,
		colFilter:           map[model.ColumnRef]string{},
		tagFilter:           map[string]bool{},
	}
	a.fyne.Settings().SetTheme(newCompactTheme(a.themeVariant))
	a.fyne.SetIcon(appIcon)
	a.win = a.fyne.NewWindow("Timeline explorer")
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
		a.cols = append(a.cols, column{ref: model.ColumnRef(i), title: title, visible: true, width: colWidth(title)})
	}
	a.rebuildVisible()
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

	a.detail = container.NewVBox(widget.NewLabel("Select a row to see details."))
	a.scroll = container.NewVScroll(a.detail)
	a.scroll.SetMinSize(fyne.NewSize(340, 100))

	a.split = container.NewHSplit(a.table, a.scroll)
	a.split.Offset = 0.72

	a.viewsPanel = a.buildViewsSidebar()
	a.outerSplit = container.NewHSplit(a.viewsPanel, a.split)
	a.outerSplit.Offset = viewsSidebarOffset

	body := container.NewBorder(a.buildToolbar(), a.buildStatusBar(), nil, nil, a.outerSplit)
	// The hover layer floats above everything but captures no input.
	content := container.NewStack(body, a.hoverLayer)
	a.win.SetContent(content)
	a.win.Resize(fyne.NewSize(1280, 760))
	if !a.viewsSidebarVisible {
		a.viewsPanel.Hide()
		a.outerSplit.SetOffset(0.0)
	}
	if !a.sidebarVisible {
		a.scroll.Hide()
		a.split.SetOffset(1.0)
	}
	a.refreshStatus()
}

// reloadWith swaps the open file, keeping the same window and shortcuts.
func (a *App) reloadWith(idx *model.Index, sess *model.Session) {
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
	a.loadStandaloneIOCList()
	a.buildColumns()
	a.buildUI()
	a.modeSelect.SetSelected(a.sess.Mode().String())
	a.resetFilterState() // a fresh file starts unfiltered; clears the query box too
}

// resetFilterState clears the active filter and its widgets without triggering
// their change callbacks, then rebuilds the filter window if it is open.
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
	if a.filterWinShown {
		a.showFilterWindow()
	}
}

// windowTitle reflects the current context: master view, a named timeline in an
// case, or a standalone file.
func (a *App) windowTitle() string {
	const base = "Timeline explorer"
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
	exportBtn := widget.NewButtonWithIcon("Export view", theme.DownloadIcon(), a.export)

	a.modeSelect = widget.NewSelect(
		[]string{model.ReadOnly.String(), model.Investigator.String(), model.WorldWrite.String()},
		a.onModeChange,
	)
	a.modeSelect.SetSelected(a.sess.Mode().String())

	tagBtn := widget.NewButtonWithIcon("Tag", theme.ContentAddIcon(), a.tagSelected)
	commentBtn := widget.NewButtonWithIcon("Comment", theme.MailComposeIcon(), a.commentSelected)
	viewsBtn := widget.NewButtonWithIcon("Views", theme.ListIcon(), a.toggleViewsSidebar)
	filterBtn := widget.NewButtonWithIcon("Filter", theme.SearchIcon(), a.toggleFilterWindow)
	clearBtn := widget.NewButtonWithIcon("Clear filters", theme.ContentClearIcon(), a.clearFilter)
	a.filterRowBtn = widget.NewButtonWithIcon("Filter row", theme.VisibilityIcon(), a.toggleFilterRow)
	a.updateFilterRowButton()
	colsBtn := widget.NewButtonWithIcon("Columns", theme.ViewFullScreenIcon(), a.columnPicker)
	sidebarBtn := widget.NewButtonWithIcon("Sidebar", theme.MenuIcon(), a.toggleSidebar)
	a.themeBtn = widget.NewButtonWithIcon("", theme.ColorPaletteIcon(), a.toggleTheme)
	a.updateThemeButton()
	helpBtn := widget.NewButtonWithIcon("Help", theme.HelpIcon(), a.showHelp)

	left := container.NewHBox(viewsBtn, openBtn, saveBtn, exportBtn, widget.NewSeparator(),
		widget.NewLabel("Mode:"), a.modeSelect, widget.NewSeparator(),
		tagBtn, commentBtn, widget.NewSeparator(), filterBtn, a.filterRowBtn, clearBtn)
	right := container.NewHBox(sidebarBtn, a.themeBtn, colsBtn, helpBtn)
	return container.NewBorder(nil, nil, left, right, nil)
}

func (a *App) buildStatusBar() fyne.CanvasObject {
	a.statusMode = widget.NewLabel("")
	a.statusRows = widget.NewLabel("")
	a.statusFilter = widget.NewLabel("")
	a.statusDirty = widget.NewLabel("")
	bg := canvas.NewRectangle(theme.Color(theme.ColorNameInputBackground))
	bar := container.NewHBox(a.statusMode, widget.NewSeparator(), a.statusRows,
		widget.NewSeparator(), a.statusFilter, widget.NewSeparator(), a.statusDirty)
	return container.NewStack(bg, bar)
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
