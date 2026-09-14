package gui

import (
	"errors"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// showExportMenu drops the two export modes under the Export toolbar button:
// "Current view" runs the plain view export; "Custom align" opens the aligned
// export builder.
func (a *App) showExportMenu(anchor fyne.CanvasObject) {
	if a.view == nil {
		return
	}
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Current view", a.export),
		fyne.NewMenuItem("Custom align…", a.customAlignExport),
	)
	pop := widget.NewPopUpMenu(menu, a.win.Canvas())
	pop.ShowAtRelativePosition(fyne.NewPos(0, anchor.Size().Height), anchor)
}

// alignOutCol is one output column being built in the custom-align dialog: a
// name entry, the ordered sources dropped onto it, and the widgets that show
// them.
type alignOutCol struct {
	name  *widget.Entry
	srcs  []model.AlignedSource
	chips *fyne.Container // GridWrap of removable source chips
	hint  *widget.Label   // "drop columns here", shown only when empty
	card  fyne.CanvasObject
}

// alignEditor holds the transient state of one custom-align session: the output
// columns and the current drag target. hoverCol is set by the drop zone the
// pointer is over (hover fires even mid-drag), so a chip's DragEnd knows where
// it landed without any geometry.
type alignEditor struct {
	a        *App
	cols     []*alignOutCol
	list     *fyne.Container
	dragging bool
	dragSrc  model.AlignedSource
	hoverCol *alignOutCol
}

// customAlignExport opens the aligned-export builder: a palette of every view
// column on the left, user-defined output columns on the right. Dragging a
// palette column onto an output column adds it as a source; the source stays in
// the palette so it can feed more than one column.
func (a *App) customAlignExport() {
	if a.view == nil {
		return
	}
	ed := &alignEditor{a: a}

	palette := container.NewVBox()
	for _, ci := range a.visible {
		col := a.cols[ci]
		palette.Add(newDragChip(ed, model.AlignedSource{Ref: col.ref, Title: col.title}))
	}

	ed.list = container.NewVBox()
	ed.addColumn()

	addBtn := widget.NewButtonWithIcon("Add output column", theme.ContentAddIcon(), func() {
		ed.addColumn()
	})

	left := container.NewBorder(
		widget.NewLabelWithStyle("View columns — drag onto an output column", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		nil, nil, nil,
		container.NewVScroll(palette),
	)
	right := container.NewBorder(
		widget.NewLabelWithStyle("Output columns", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		addBtn, nil, nil,
		container.NewVScroll(ed.list),
	)
	split := container.NewHSplit(left, right)
	split.Offset = 0.34

	d := dialog.NewCustomConfirm("Custom align export", "Export", "Cancel", split, func(ok bool) {
		if ok {
			ed.doExport()
		}
	}, a.win)
	d.Resize(a.dialogSize(940, 640))
	d.Show()
}

// addColumn appends a fresh, empty output column to the builder.
func (ed *alignEditor) addColumn() {
	oc := &alignOutCol{}
	oc.name = widget.NewEntry()
	oc.name.SetPlaceHolder(fmt.Sprintf("Column %d name", len(ed.cols)+1))
	oc.chips = container.New(layout.NewGridWrapLayout(fyne.NewSize(150, 32)))
	oc.hint = widget.NewLabelWithStyle("drop columns here", fyne.TextAlignCenter, fyne.TextStyle{Italic: true})

	rm := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() { ed.removeColumn(oc) })
	rm.Importance = widget.LowImportance
	head := container.NewBorder(nil, nil, nil, rm, oc.name)

	zoneBody := container.NewVBox(oc.chips, oc.hint)
	oc.card = container.NewVBox(head, newDropZone(ed, oc, zoneBody))

	ed.cols = append(ed.cols, oc)
	ed.rebuildList()
	ed.rebuildChips(oc)
}

// removeColumn drops an output column from the builder.
func (ed *alignEditor) removeColumn(oc *alignOutCol) {
	for i, c := range ed.cols {
		if c == oc {
			ed.cols = append(ed.cols[:i], ed.cols[i+1:]...)
			break
		}
	}
	ed.rebuildList()
}

// rebuildList re-renders the output-column list from ed.cols.
func (ed *alignEditor) rebuildList() {
	ed.list.Objects = ed.list.Objects[:0]
	for _, oc := range ed.cols {
		ed.list.Add(oc.card)
	}
	ed.list.Refresh()
}

// assign adds src to oc unless the same source is already there, then re-renders
// that column's chips.
func (ed *alignEditor) assign(oc *alignOutCol, src model.AlignedSource) {
	for _, s := range oc.srcs {
		if s.Ref == src.Ref {
			return
		}
	}
	oc.srcs = append(oc.srcs, src)
	ed.rebuildChips(oc)
}

// rebuildChips redraws the assigned-source chips for one output column, each
// removable by a tap, and shows the "drop here" hint only while empty.
func (ed *alignEditor) rebuildChips(oc *alignOutCol) {
	oc.chips.Objects = oc.chips.Objects[:0]
	for _, src := range oc.srcs {
		ref := src.Ref
		b := widget.NewButton(src.Title+"  ✕", func() { ed.removeSrc(oc, ref) })
		b.Importance = widget.LowImportance
		oc.chips.Add(b)
	}
	oc.chips.Refresh()
	if len(oc.srcs) == 0 {
		oc.hint.Show()
	} else {
		oc.hint.Hide()
	}
}

// removeSrc drops the source with the given ref from oc.
func (ed *alignEditor) removeSrc(oc *alignOutCol, ref model.ColumnRef) {
	for i, s := range oc.srcs {
		if s.Ref == ref {
			oc.srcs = append(oc.srcs[:i], oc.srcs[i+1:]...)
			break
		}
	}
	ed.rebuildChips(oc)
}

// doExport gathers the named output columns (skipping empty ones) and writes the
// aligned CSV. An empty builder is refused.
func (ed *alignEditor) doExport() {
	var cols []model.AlignedColumn
	for i, oc := range ed.cols {
		if len(oc.srcs) == 0 {
			continue
		}
		name := strings.TrimSpace(oc.name.Text)
		if name == "" {
			name = fmt.Sprintf("Column %d", i+1)
		}
		cols = append(cols, model.AlignedColumn{Name: name, Sources: oc.srcs})
	}
	if len(cols) == 0 {
		ed.a.showError(errors.New("add at least one output column with a source dropped onto it"))
		return
	}
	ed.a.showSaveFile("", func(path string) {
		if err := model.ExportAligned(ed.a.view, ed.a.sess, path, ed.a.currentTimelineComment(), cols); err != nil {
			ed.a.showError(err)
			return
		}
		dialog.ShowInformation("Exported", fmt.Sprintf("%d rows written to\n%s", ed.a.view.Len(), path), ed.a.win)
	})
}

// dragChip is a palette entry: a labelled box that, when dragged onto a drop
// zone, adds its source to that output column. The source stays in the palette
// so it can be reused across columns.
type dragChip struct {
	widget.BaseWidget
	ed  *alignEditor
	src model.AlignedSource
}

func newDragChip(ed *alignEditor, src model.AlignedSource) *dragChip {
	c := &dragChip{ed: ed, src: src}
	c.ExtendBaseWidget(c)
	return c
}

func (c *dragChip) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(theme.Color(theme.ColorNameButton))
	bg.CornerRadius = 6
	bg.StrokeColor = theme.Color(theme.ColorNameInputBorder)
	bg.StrokeWidth = 1
	label := widget.NewLabel(c.src.Title)
	label.Truncation = fyne.TextTruncateEllipsis
	return widget.NewSimpleRenderer(container.NewStack(bg, container.NewPadded(label)))
}

func (c *dragChip) Dragged(*fyne.DragEvent) {
	c.ed.dragSrc = c.src
	c.ed.dragging = true
}

func (c *dragChip) DragEnd() {
	if c.ed.dragging && c.ed.hoverCol != nil {
		c.ed.assign(c.ed.hoverCol, c.src)
	}
	c.ed.dragging = false
}

// dropZone is one output column's drop target. Fyne fires hover on the widget
// under the pointer even during a drag, so MouseIn/MouseOut track which column a
// chip would land on and highlight it. It implements desktop.Hoverable.
type dropZone struct {
	widget.BaseWidget
	ed   *alignEditor
	oc   *alignOutCol
	bg   *canvas.Rectangle
	body fyne.CanvasObject
}

func newDropZone(ed *alignEditor, oc *alignOutCol, body fyne.CanvasObject) *dropZone {
	z := &dropZone{ed: ed, oc: oc, body: body}
	z.bg = canvas.NewRectangle(theme.Color(theme.ColorNameInputBackground))
	z.bg.CornerRadius = 6
	z.bg.StrokeColor = theme.Color(theme.ColorNameInputBorder)
	z.bg.StrokeWidth = 1
	z.ExtendBaseWidget(z)
	return z
}

func (z *dropZone) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.NewStack(z.bg, container.NewPadded(z.body)))
}

func (z *dropZone) MouseIn(*desktop.MouseEvent) {
	z.ed.hoverCol = z.oc
	if z.ed.dragging {
		z.bg.FillColor = theme.Color(theme.ColorNameHover)
		z.bg.Refresh()
	}
}

func (z *dropZone) MouseMoved(*desktop.MouseEvent) { z.ed.hoverCol = z.oc }

func (z *dropZone) MouseOut() {
	if z.ed.hoverCol == z.oc {
		z.ed.hoverCol = nil
	}
	z.bg.FillColor = theme.Color(theme.ColorNameInputBackground)
	z.bg.Refresh()
}
