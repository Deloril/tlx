package gui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// buildReorderHarness wires a minimal App around a real bigTable with header
// buttons and the drag overlay, mirroring newTable's header shape but without a
// data source. It returns the App and the window showing the wrapped table.
func buildReorderHarness(t *testing.T) (*App, fyne.Window) {
	t.Helper()
	test.NewApp()

	a := &App{
		selRow:         -1,
		selCol:         -1,
		hoverRow:       -1,
		hoverCol:       -1,
		hoverHeaderPos: -1,
		colFilter:      map[model.ColumnRef]string{},
	}
	a.cols = []column{
		{ref: 0, title: "Alpha", visible: true, width: 120},
		{ref: 1, title: "Bravo", visible: true, width: 120},
		{ref: 2, title: "Charlie", visible: true, width: 120},
	}
	a.rebuildVisible()

	tbl := newBigTable(
		func() int { return 3 },
		func() int { return len(a.visible) },
	)
	tbl.ShowHeaderRow = true
	tbl.CreateCell = func() fyne.CanvasObject { return widget.NewLabel("") }
	tbl.UpdateCell = func(id widget.TableCellID, o fyne.CanvasObject) {}
	tbl.CreateHeader = func() fyne.CanvasObject { return container.NewVBox(newHeaderButton(a)) }
	tbl.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		b := o.(*fyne.Container).Objects[0].(*headerButton)
		b.pos = id.Col
		if id.Col < 0 || id.Col >= len(a.visible) {
			return
		}
		b.SetText(a.cols[a.visible[id.Col]].title)
		b.OnTapped = func() {}
	}
	a.table = tbl
	for i, ci := range a.visible {
		tbl.SetColumnWidth(i, a.cols[ci].width)
	}

	w := test.NewWindow(a.wrapTable())
	w.Resize(fyne.NewSize(400, 200))
	tbl.Refresh()
	return a, w
}

func titles(a *App) []string {
	out := make([]string, len(a.visible))
	for i, ci := range a.visible {
		out[i] = a.cols[ci].title
	}
	return out
}

// TestHeaderDragReordersColumn drives the real gesture path: hover a header cell
// (so the overlay learns the start column), then drag it past its right
// neighbour. The Table's header clip swallows the drag to the button, so this
// only passes because the overlay picks it up.
func TestHeaderDragReordersColumn(t *testing.T) {
	a, w := buildReorderHarness(t)
	defer w.Close()

	if got := titles(a); got[0] != "Alpha" || got[1] != "Bravo" {
		t.Fatalf("unexpected start order %v", got)
	}

	// Hover the first column's header to set the drag's start column, then drag
	// right past the second column's half-width (60px).
	start := fyne.NewPos(40, 8)
	test.MoveMouse(w.Canvas(), start)
	if a.hoverHeaderPos != 0 {
		t.Fatalf("hover did not land on column 0 (got %d) — header hover routing changed", a.hoverHeaderPos)
	}
	test.Drag(w.Canvas(), start, 130, 0)

	if got := titles(a); got[0] != "Bravo" || got[1] != "Alpha" || got[2] != "Charlie" {
		t.Fatalf("drag did not reorder columns: got %v, want [Bravo Alpha Charlie]", got)
	}
	if a.dragHdrActive {
		t.Error("drag state left active after the gesture")
	}
}

// TestHeaderTapDoesNotReorder guards the tap/drag split: a plain click on a
// header must sort (reach the button), never reorder.
func TestHeaderTapDoesNotReorder(t *testing.T) {
	a, w := buildReorderHarness(t)
	defer w.Close()

	before := titles(a)
	test.TapCanvas(w.Canvas(), fyne.NewPos(40, 8))
	if got := titles(a); got[0] != before[0] || got[1] != before[1] {
		t.Fatalf("a tap reordered columns: %v -> %v", before, got)
	}
}
