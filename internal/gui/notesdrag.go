package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
)

// draggableRow wraps a note row so it can be dragged to reorder within its list.
// The drag reorders the surrounding VBox live — moving the existing widget
// instances rather than rebuilding, so the widget under the pointer stays alive
// and keeps receiving drag events — and the new order is persisted on drop.
type draggableRow struct {
	widget.BaseWidget
	id      int64
	content fyne.CanvasObject
	onDrag  func(r *draggableRow, dy float32)
	onDrop  func()
	accum   float32 // drag distance banked since the last row-swap
}

func newDraggableRow(id int64, content fyne.CanvasObject) *draggableRow {
	r := &draggableRow{id: id, content: content}
	r.ExtendBaseWidget(r)
	return r
}

func (r *draggableRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(r.content)
}

func (r *draggableRow) Dragged(e *fyne.DragEvent) {
	if r.onDrag != nil {
		r.onDrag(r, e.Dragged.DY)
	}
}

func (r *draggableRow) DragEnd() {
	r.accum = 0
	if r.onDrop != nil {
		r.onDrop()
	}
}

// dragNoteRow moves r up or down its list as the pointer drags past roughly one
// row height. Rows vary in height (wrapped text), so the dragged row's own
// height is used as an approximate step.
func (a *App) dragNoteRow(box *fyne.Container, r *draggableRow, dy float32) {
	r.accum += dy
	h := r.Size().Height
	if h <= 0 {
		return
	}
	step := h * 0.6
	for r.accum <= -step {
		if !moveRow(box, r, -1) {
			r.accum = -step // clamp: nothing above to move into
			break
		}
		r.accum += h
	}
	for r.accum >= step {
		if !moveRow(box, r, +1) {
			r.accum = step
			break
		}
		r.accum -= h
	}
}

// moveRow swaps r with its neighbour in dir (-1 up, +1 down) within box, keeping
// the same widget instances. Returns false at the ends of the list.
func moveRow(box *fyne.Container, r *draggableRow, dir int) bool {
	idx := -1
	for i, o := range box.Objects {
		if o == r {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	j := idx + dir
	if j < 0 || j >= len(box.Objects) {
		return false
	}
	if _, ok := box.Objects[j].(*draggableRow); !ok {
		return false
	}
	box.Objects[idx], box.Objects[j] = box.Objects[j], box.Objects[idx]
	box.Refresh()
	return true
}

// commitNotesOrder reads the current row order from box and persists it, then
// re-renders the section from the stored order.
func (a *App) commitNotesOrder(box *fyne.Container, kind string) {
	var ids []int64
	for _, o := range box.Objects {
		if r, ok := o.(*draggableRow); ok {
			ids = append(ids, r.id)
		}
	}
	a.reorderNotes(kind, ids)
	a.refreshNotesSection()
}
