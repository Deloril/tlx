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

	"timeline-engine/internal/casefile"
	"timeline-engine/internal/model"
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

	cols      []column        // all columns, in display order
	visible   []int           // indices into cols that are currently shown
	sortState []model.SortKey // mirror of view sort keys, for header arrows

	table  *bigTable
	detail *fyne.Container
	scroll *container.Scroll
	split  *container.Split // table | detail

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

	selRow int // selected view row, -1 if none
	selCol int // selected table column, -1 if none

	sidebarVisible      bool // right detail pane
	viewsSidebarVisible bool // left saved-views pane
	themeVariant        fyne.ThemeVariant

	// Per-column filter conditions, kept so the search box and column filter
	// compose and both survive a re-apply.
	conds    []model.ColumnCond
	condsAny bool

	// colFilter holds the quick-filter text under each column header, keyed by
	// column ref so it survives column show/hide and reorders.
	colFilter map[model.ColumnRef]string

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
		sidebarVisible:      true,
		viewsSidebarVisible: true,
		themeVariant:        theme.VariantDark,
		startMode:           model.ReadOnly,
		colFilter:           map[model.ColumnRef]string{},
	}
	a.fyne.Settings().SetTheme(newCompactTheme(a.themeVariant))
	a.win = a.fyne.NewWindow("Timeline explorer")
	a.win.Resize(fyne.NewSize(1280, 760))
	a.win.SetCloseIntercept(a.onClose)
	a.win.SetMainMenu(a.buildMainMenu())
	a.buildHoverLayer()
	a.registerShortcuts()
	a.showPlaceholder()
	return a
}

// SetStartMode sets the mode used for files opened through the file dialog.
func (a *App) SetStartMode(m model.Mode) { a.startMode = m }

// OpenInitial loads an already-opened index and session into the window.
func (a *App) OpenInitial(idx *model.Index, sess *model.Session) {
	a.cse, a.curTimeline, a.masterMode = nil, nil, false
	a.annotCols = true
	a.reloadWith(idx, sess)
}

// showPlaceholder renders the empty state with an Open button.
func (a *App) showPlaceholder() {
	open := widget.NewButtonWithIcon("Open CSV…", theme.FolderOpenIcon(), a.openFile)
	hint := widget.NewLabel("Open a forensic CSV timeline to begin.")
	a.win.SetContent(container.NewCenter(container.NewVBox(hint, container.NewCenter(open))))
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
		a.cols = append(a.cols, column{ref: model.ColumnRef(i), title: h, visible: true, width: colWidth(h)})
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
	a.colFilter = map[model.ColumnRef]string{}
	a.sortState = nil
	a.selRow, a.selCol = -1, -1
	a.editing, a.editFocused = false, false
	a.hideTooltip()
	a.win.SetTitle(a.windowTitle())
	a.buildColumns()
	a.buildUI()
	a.modeSelect.SetSelected(a.sess.Mode().String())
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

	a.search = widget.NewEntry()
	a.search.SetPlaceHolder(`Filter: text, or field=value AND (tag=bad OR tag=suspicious). /regex/ for regex. Enter to apply, Esc to clear`)
	a.search.OnSubmitted = func(string) { a.applySearch() }

	a.taggedChk = widget.NewCheck("Tagged only", func(bool) { a.applySearch() })
	a.caseChk = widget.NewCheck("Case sensitive", func(bool) { a.applySearch() })

	tagBtn := widget.NewButtonWithIcon("Tag", theme.ContentAddIcon(), a.tagSelected)
	commentBtn := widget.NewButtonWithIcon("Comment", theme.MailComposeIcon(), a.commentSelected)
	viewsBtn := widget.NewButtonWithIcon("Views", theme.ListIcon(), a.toggleViewsSidebar)
	filterBtn := widget.NewButtonWithIcon("Filter", theme.ListIcon(), a.columnFilter)
	colsBtn := widget.NewButtonWithIcon("Columns", theme.ViewFullScreenIcon(), a.columnPicker)
	sidebarBtn := widget.NewButtonWithIcon("Sidebar", theme.MenuIcon(), a.toggleSidebar)
	a.themeBtn = widget.NewButtonWithIcon("", theme.ColorPaletteIcon(), a.toggleTheme)
	a.updateThemeButton()
	helpBtn := widget.NewButtonWithIcon("Help", theme.HelpIcon(), a.showHelp)

	left := container.NewHBox(viewsBtn, openBtn, saveBtn, exportBtn, widget.NewSeparator(),
		widget.NewLabel("Mode:"), a.modeSelect, widget.NewSeparator(),
		tagBtn, commentBtn, widget.NewSeparator(), a.taggedChk, a.caseChk)
	right := container.NewHBox(filterBtn, sidebarBtn, a.themeBtn, colsBtn, helpBtn)
	// Search stretches in the middle.
	return container.NewBorder(nil, nil, left, right, a.search)
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

// Run shows the window and blocks until it closes.
func (a *App) Run() {
	a.win.ShowAndRun()
}
