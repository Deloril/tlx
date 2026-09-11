// Package gui is the Fyne front end for the Timeline explorer. It depends on a
// C/OpenGL toolchain (Fyne), unlike internal/model, which is pure Go.
package gui

import (
	"fmt"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

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

	cols      []column        // all columns, in display order
	visible   []int           // indices into cols that are currently shown
	sortState []model.SortKey // mirror of view sort keys, for header arrows

	table  *widget.Table
	detail *fyne.Container
	scroll *container.Scroll

	search     *widget.Entry
	modeSelect *widget.Select
	taggedChk  *widget.Check

	statusMode   *widget.Label
	statusRows   *widget.Label
	statusFilter *widget.Label
	statusDirty  *widget.Label

	selRow int // selected view row, -1 if none
	selCol int // selected table column, -1 if none

	startMode model.Mode // mode applied to files opened via the dialog
}

// New builds an explorer with no file loaded yet. Call OpenInitial to load one,
// or let the user open one from the toolbar; either way Run shows the window.
func New() *App {
	a := &App{
		fyne:      app.NewWithID("nz.timeline.explorer"),
		selRow:    -1,
		selCol:    -1,
		startMode: model.ReadOnly,
	}
	a.win = a.fyne.NewWindow("Timeline explorer")
	a.win.Resize(fyne.NewSize(1280, 760))
	a.win.SetCloseIntercept(a.onClose)
	a.registerShortcuts()
	a.showPlaceholder()
	return a
}

// SetStartMode sets the mode used for files opened through the file dialog.
func (a *App) SetStartMode(m model.Mode) { a.startMode = m }

// OpenInitial loads an already-opened index and session into the window.
func (a *App) OpenInitial(idx *model.Index, sess *model.Session) {
	a.reloadWith(idx, sess)
}

// showPlaceholder renders the empty state with an Open button.
func (a *App) showPlaceholder() {
	open := widget.NewButtonWithIcon("Open CSV…", theme.FolderOpenIcon(), a.openFile)
	hint := widget.NewLabel("Open a forensic CSV timeline to begin.")
	a.win.SetContent(container.NewCenter(container.NewVBox(hint, container.NewCenter(open))))
}

func (a *App) buildColumns() {
	a.cols = []column{
		{ref: model.ColTags, title: "✎ Tags", visible: true, width: 160},
		{ref: model.ColComment, title: "✎ Comment", visible: true, width: 220},
	}
	for i, h := range a.idx.Headers() {
		w := float32(140)
		if h == "Summary" || h == "Message" {
			w = 520
		}
		a.cols = append(a.cols, column{ref: model.ColumnRef(i), title: h, visible: true, width: w})
	}
	a.rebuildVisible()
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

	split := container.NewHSplit(a.table, a.scroll)
	split.Offset = 0.72

	content := container.NewBorder(a.buildToolbar(), a.buildStatusBar(), nil, nil, split)
	a.win.SetContent(content)
	a.win.Resize(fyne.NewSize(1280, 760))
	a.refreshStatus()
}

// reloadWith swaps the open file, keeping the same window and shortcuts.
func (a *App) reloadWith(idx *model.Index, sess *model.Session) {
	if a.idx != nil {
		a.idx.Close()
	}
	a.idx, a.sess = idx, sess
	a.view = model.NewView(idx, sess)
	a.sortState = nil
	a.selRow, a.selCol = -1, -1
	a.win.SetTitle("Timeline explorer — " + idx.Path())
	a.buildColumns()
	a.buildUI()
	a.modeSelect.SetSelected(a.sess.Mode().String())
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
	a.search.SetPlaceHolder("Filter across all columns (regex with /… ). Enter to apply, Esc to clear")
	a.search.OnSubmitted = func(string) { a.applySearch() }

	a.taggedChk = widget.NewCheck("Tagged only", func(bool) { a.applySearch() })

	tagBtn := widget.NewButtonWithIcon("Tag", theme.ContentAddIcon(), a.tagSelected)
	commentBtn := widget.NewButtonWithIcon("Comment", theme.MailComposeIcon(), a.commentSelected)
	colsBtn := widget.NewButtonWithIcon("Columns", theme.ViewFullScreenIcon(), a.columnPicker)
	helpBtn := widget.NewButtonWithIcon("Help", theme.HelpIcon(), a.showHelp)

	left := container.NewHBox(openBtn, saveBtn, exportBtn, widget.NewSeparator(),
		widget.NewLabel("Mode:"), a.modeSelect, widget.NewSeparator(),
		tagBtn, commentBtn, widget.NewSeparator(), a.taggedChk)
	right := container.NewHBox(colsBtn, helpBtn)
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

// tagHighlight is the row background for rows that carry annotations.
var tagHighlight = color.NRGBA{R: 0xF6, G: 0xD8, B: 0x8A, A: 0x55}

// Run shows the window and blocks until it closes.
func (a *App) Run() {
	a.win.ShowAndRun()
}
