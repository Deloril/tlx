package gui

import (
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"timeline-engine/internal/model"
)

func (a *App) newTable() *widget.Table {
	t := widget.NewTable(
		func() (int, int) { return a.view.Len(), len(a.visible) },
		func() fyne.CanvasObject {
			bg := canvas.NewRectangle(color.Transparent)
			lbl := widget.NewLabel("")
			lbl.Truncation = fyne.TextTruncateEllipsis
			return container.NewStack(bg, lbl)
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			a.updateCell(id, o)
		},
	)
	t.ShowHeaderRow = true
	t.CreateHeader = func() fyne.CanvasObject {
		b := widget.NewButton("", nil)
		b.Alignment = widget.ButtonAlignLeading
		b.Importance = widget.LowImportance
		return b
	}
	t.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		a.updateHeader(id, o)
	}
	t.OnSelected = func(id widget.TableCellID) {
		a.selRow, a.selCol = id.Row, id.Col
		a.showDetail(a.view.Master(id.Row))
	}
	for i, ci := range a.visible {
		t.SetColumnWidth(i, a.cols[ci].width)
	}
	return t
}

func (a *App) updateCell(id widget.TableCellID, o fyne.CanvasObject) {
	stack, ok := o.(*fyne.Container)
	if !ok || len(stack.Objects) < 2 {
		return
	}
	bg, _ := stack.Objects[0].(*canvas.Rectangle)
	lbl, _ := stack.Objects[1].(*widget.Label)
	if lbl == nil {
		return
	}
	if id.Col < 0 || id.Col >= len(a.visible) || id.Row < 0 || id.Row >= a.view.Len() {
		lbl.SetText("")
		return
	}
	master := a.view.Master(id.Row)
	ref := a.cols[a.visible[id.Col]].ref
	lbl.SetText(oneLine(a.valueOf(master, ref)))

	if bg != nil {
		if a.rowAnnotated(master) {
			bg.FillColor = tagHighlight
		} else {
			bg.FillColor = color.Transparent
		}
		bg.Refresh()
	}
}

func (a *App) updateHeader(id widget.TableCellID, o fyne.CanvasObject) {
	btn, ok := o.(*widget.Button)
	if !ok {
		return
	}
	// Only column headers are shown (ShowHeaderRow); guard other callbacks.
	if id.Col < 0 || id.Col >= len(a.visible) {
		btn.SetText("")
		btn.OnTapped = nil
		return
	}
	col := a.cols[a.visible[id.Col]]
	title := col.title
	if arrow := a.sortArrow(col.ref); arrow != "" {
		title += " " + arrow
	}
	btn.SetText(title)
	ref := col.ref
	btn.OnTapped = func() { a.sortByColumn(ref) }
}

func (a *App) sortArrow(ref model.ColumnRef) string {
	if len(a.sortState) == 0 || a.sortState[0].Col != ref {
		return ""
	}
	if a.sortState[0].Desc {
		return "▼"
	}
	return "▲"
}

func (a *App) sortByColumn(ref model.ColumnRef) {
	desc := false
	if len(a.sortState) > 0 && a.sortState[0].Col == ref {
		desc = !a.sortState[0].Desc // toggle direction on repeat click
	}
	a.sortState = []model.SortKey{{Col: ref, Desc: desc}}
	if err := a.view.Sort(a.sortState); err != nil {
		a.showError(err)
		return
	}
	a.clearSelection()
	a.refreshTable()
}

// valueOf returns the display value for a master row and column, applying edits
// and virtual columns.
func (a *App) valueOf(master int, ref model.ColumnRef) string {
	switch ref {
	case model.ColTags:
		return strings.Join(a.sess.Tags(master), ", ")
	case model.ColComment:
		return a.sess.Comment(master)
	default:
		c := int(ref)
		if v, ok := a.sess.CellOverride(master, c); ok {
			return v
		}
		rec, err := a.idx.Row(master)
		if err != nil {
			return "?"
		}
		if c >= 0 && c < len(rec) {
			return rec[c]
		}
		return ""
	}
}

func (a *App) rowAnnotated(master int) bool {
	return len(a.sess.Tags(master)) > 0 || a.sess.Comment(master) != ""
}

func (a *App) clearSelection() {
	if a.table != nil {
		a.table.UnselectAll()
	}
	a.selRow, a.selCol = -1, -1
}

// oneLine collapses embedded newlines so a multi-line Summary shows as a single
// grid row; the full text is visible in the detail pane.
func oneLine(s string) string {
	if strings.IndexByte(s, '\n') < 0 && strings.IndexByte(s, '\r') < 0 {
		return s
	}
	r := strings.NewReplacer("\r\n", " ⏎ ", "\n", " ⏎ ", "\r", " ⏎ ")
	return r.Replace(s)
}
