package gui

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"timeline-engine/internal/model"
)

// inlineEntry is a single-line entry used to edit a cell in place. Return
// commits (via OnSubmitted), Escape cancels.
type inlineEntry struct {
	widget.Entry
	onCommit func(string)
	onCancel func()
}

func newInlineEntry() *inlineEntry {
	e := &inlineEntry{}
	e.ExtendBaseWidget(e)
	e.OnSubmitted = func(s string) {
		if e.onCommit != nil {
			e.onCommit(s)
		}
	}
	return e
}

func (e *inlineEntry) TypedKey(k *fyne.KeyEvent) {
	if k.Name == fyne.KeyEscape {
		if e.onCancel != nil {
			e.onCancel()
		}
		return
	}
	e.Entry.TypedKey(k)
}

// FocusLost commits the edit, so clicking away keeps typed changes rather than
// discarding them.
func (e *inlineEntry) FocusLost() {
	e.Entry.FocusLost()
	if e.onCommit != nil {
		e.onCommit(e.Text)
	}
}

// cellEditable reports whether a column can be typed into directly, given the
// current mode. Tags are edited through the Tag dialog, not inline.
func (a *App) cellEditable(ref model.ColumnRef) bool {
	switch a.sess.Mode() {
	case model.WorldWrite:
		return ref == model.ColComment || ref >= 0
	case model.Investigator:
		return ref == model.ColComment
	default:
		return false
	}
}

// onCellSelected runs when a grid cell is clicked. It updates the detail pane
// (when shown) and, for editable columns, drops straight into inline editing.
func (a *App) onCellSelected(id widget.TableCellID) {
	a.selRow, a.selCol = id.Row, id.Col
	if id.Row < 0 || id.Row >= a.view.Len() || id.Col < 0 || id.Col >= len(a.visible) {
		return
	}
	master := a.view.Master(id.Row)
	if a.sidebarVisible {
		a.showDetail(master)
	}
	ref := a.cols[a.visible[id.Col]].ref
	if a.cellEditable(ref) {
		a.startInlineEdit(id.Row, id.Col)
	} else {
		a.cancelInlineEdit()
	}
}

func (a *App) startInlineEdit(row, col int) {
	a.hideTooltip()
	a.editing = true
	a.editRow, a.editCol = row, col
	a.editFocused = false
	if a.table != nil {
		a.table.Refresh()
	}
}

func (a *App) commitInlineEdit(master int, ref model.ColumnRef, s string) {
	if !a.editing { // guard against a second commit from FocusLost
		return
	}
	var err error
	switch ref {
	case model.ColComment:
		err = a.sess.SetComment(master, s)
	default:
		err = a.sess.SetCell(master, int(ref), s)
	}
	a.editing = false
	a.editFocused = false
	if err != nil {
		a.showError(err)
	}
	if a.sidebarVisible && a.selectedMaster() >= 0 {
		a.showDetail(a.selectedMaster())
	}
	a.refreshTable()
}

func (a *App) cancelInlineEdit() {
	if !a.editing {
		return
	}
	a.editing = false
	a.editFocused = false
	if a.table != nil {
		a.table.Refresh()
	}
}

// Sidebar.

func (a *App) toggleSidebar() {
	a.setSidebar(!a.sidebarVisible)
}

func (a *App) setSidebar(show bool) {
	a.sidebarVisible = show
	if a.scroll == nil || a.split == nil {
		return
	}
	if show {
		a.scroll.Show()
		a.split.SetOffset(0.72)
		if m := a.selectedMaster(); m >= 0 {
			a.showDetail(m)
		}
	} else {
		a.scroll.Hide()
		a.split.SetOffset(1.0)
	}
	a.split.Refresh()
}

// Hover tooltip. Rendered as a non-interactive overlay layer inside the content
// stack so it never captures clicks and needs no per-row layout math.

func (a *App) hoverCell(id widget.TableCellID, at fyne.Position) {
	if a.editing || a.hoverLayer == nil {
		a.hideTooltip()
		return
	}
	if id.Row < 0 || id.Row >= a.view.Len() || id.Col < 0 || id.Col >= len(a.visible) {
		a.hideTooltip()
		return
	}
	master := a.view.Master(id.Row)
	ref := a.cols[a.visible[id.Col]].ref
	full := a.valueOf(master, ref)
	if strings.TrimSpace(full) == "" {
		a.hideTooltip()
		return
	}
	// Only show when the cell would truncate: text wider than the column, or
	// multi-line content collapsed to one line in the grid.
	colWidth := a.cols[a.visible[id.Col]].width
	textW := fyne.MeasureText(oneLine(full), theme.TextSize(), fyne.TextStyle{}).Width
	if textW <= colWidth-16 && strings.IndexByte(full, '\n') < 0 {
		a.hideTooltip()
		return
	}
	a.showTooltip(full, at)
}

func (a *App) showTooltip(text string, at fyne.Position) {
	if a.hoverLayer == nil {
		return
	}
	a.hoverText.Segments = []widget.RichTextSegment{&widget.TextSegment{Text: text}}
	a.hoverText.Wrapping = fyne.TextWrapWord
	a.hoverText.Resize(fyne.NewSize(460, a.hoverText.MinSize().Height))

	sz := a.hoverText.Size()
	pad := float32(6)
	w, h := sz.Width+pad*2, sz.Height+pad*2

	// Clamp within the window so the box stays visible.
	canvasSize := a.win.Canvas().Size()
	x := at.X + 14
	y := at.Y + 16
	if x+w > canvasSize.Width {
		x = canvasSize.Width - w - 4
	}
	if y+h > canvasSize.Height {
		y = at.Y - h - 8
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	a.hoverBG.Resize(fyne.NewSize(w, h))
	a.hoverBG.Move(fyne.NewPos(x, y))
	a.hoverText.Move(fyne.NewPos(x+pad, y+pad))
	a.hoverLayer.Show()
	a.hoverLayer.Refresh()
}

func (a *App) hideTooltip() {
	if a.hoverLayer != nil && a.hoverLayer.Visible() {
		a.hoverLayer.Hide()
		a.hoverLayer.Refresh()
	}
}

// buildHoverLayer creates the (initially hidden) tooltip overlay.
func (a *App) buildHoverLayer() {
	a.hoverBG = canvas.NewRectangle(theme.Color(theme.ColorNameOverlayBackground))
	a.hoverBG.StrokeColor = theme.Color(theme.ColorNameInputBorder)
	a.hoverBG.StrokeWidth = 1
	a.hoverBG.CornerRadius = 4
	a.hoverText = widget.NewRichTextWithText("")
	a.hoverLayer = container.NewWithoutLayout(a.hoverBG, a.hoverText)
	a.hoverLayer.Hide()
}
