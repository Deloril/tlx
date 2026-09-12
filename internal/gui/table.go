package gui

import (
	"image/color"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// bigTable wraps widget.Table to dodge a Fyne memory blow-up on huge row counts.
//
// Fyne's table creates one separator object (~0.5 KB) per row the first time it
// lays out, and if that first layout runs while the table's content height is
// still zero it uses the full row count instead of the visible window. On a
// 2M-row file that is ~1 GB of separators that never get freed, and every later
// canvas repaint walks them — which is why even opening a dropdown was slow.
//
// We report zero rows until the table has a real on-screen height (the exact
// condition that avoids the blow-up), then force one refresh so the rows appear.
// Memory then stays flat (~180 MB) regardless of row count.
type bigTable struct {
	widget.Table
	rowCount    func() int              // real row count once we are sized
	cols        func() int              // column count
	onLeave     func()                  // called when the pointer leaves the table
	onSecondary func(pos fyne.Position) // called on right-click, with canvas position
	primed      bool
	lastPos     fyne.Position
	// lastMod carries the keyboard modifiers of the most recent mouse press to
	// the OnSelected callback, which Fyne's PointEvent does not include. It is
	// consumed (reset to 0) each time a click is handled, so a programmatic
	// Select is never mistaken for a modifier click.
	lastMod fyne.KeyModifier
}

func newBigTable(rowCount, cols func() int) *bigTable {
	b := &bigTable{rowCount: rowCount, cols: cols}
	b.Length = func() (int, int) {
		if b.Size().Height <= 1 {
			return 0, b.cols()
		}
		return b.rowCount(), b.cols()
	}
	b.ExtendBaseWidget(b)
	return b
}

// Resize primes the first real layout so rows appear once the widget is sized.
func (b *bigTable) Resize(s fyne.Size) {
	b.Table.Resize(s)
	if !b.primed && s.Height > 1 {
		b.primed = true
		b.Refresh()
	}
}

// MouseMoved records the cursor so hover callbacks can place a tooltip without
// the O(row) findY the table uses for scrolling.
func (b *bigTable) MouseMoved(e *desktop.MouseEvent) {
	b.lastPos = e.AbsolutePosition // canvas-relative, matches the overlay layer
	b.Table.MouseMoved(e)
}

// MouseOut clears any hover tooltip.
func (b *bigTable) MouseOut() {
	if b.onLeave != nil {
		b.onLeave()
	}
	b.Table.MouseOut()
}

// MouseDown records the press modifiers (Shift/Ctrl/Cmd) so the OnSelected
// handler that follows the tap can implement range- and toggle-selection. The
// press is forwarded to the embedded table only if it handles one.
func (b *bigTable) MouseDown(e *desktop.MouseEvent) {
	b.lastMod = e.Modifier
	if m, ok := any(&b.Table).(desktop.Mouseable); ok {
		m.MouseDown(e)
	}
}

func (b *bigTable) MouseUp(e *desktop.MouseEvent) {
	if m, ok := any(&b.Table).(desktop.Mouseable); ok {
		m.MouseUp(e)
	}
}

// TappedSecondary opens the row context menu at the pointer.
func (b *bigTable) TappedSecondary(e *fyne.PointEvent) {
	if b.onSecondary != nil {
		b.onSecondary(e.AbsolutePosition)
	}
}

func (a *App) newTable() *bigTable {
	t := newBigTable(
		func() int { return a.view.Len() },
		func() int { return len(a.visible) },
	)
	t.CreateCell = func() fyne.CanvasObject {
		bg := canvas.NewRectangle(color.Transparent)
		lbl := widget.NewLabel("")
		lbl.Truncation = fyne.TextTruncateEllipsis
		// rich is used only when a cell has filter matches to highlight; the plain
		// label handles the common case (and truncates cleanly).
		rich := widget.NewRichText()
		rich.Truncation = fyne.TextTruncateEllipsis
		rich.Wrapping = fyne.TextWrapOff
		rich.Hide()
		entry := newInlineEntry()
		entry.Hide()
		return container.NewStack(bg, lbl, rich, entry)
	}
	t.UpdateCell = func(id widget.TableCellID, o fyne.CanvasObject) {
		a.updateCell(id, o)
	}
	t.ShowHeaderRow = true
	// Two-row header: the sort button (column title) sits on top and the
	// per-column filter box underneath. Fyne sizes the header row from this
	// template's MinSize and data rows from the cell template separately, so the
	// taller header does not stretch the data rows.
	t.CreateHeader = func() fyne.CanvasObject {
		sort := widget.NewButton("", nil)
		sort.Alignment = widget.ButtonAlignLeading
		sort.Importance = widget.LowImportance
		filter := widget.NewEntry()
		filter.SetPlaceHolder("filter…")
		return container.NewVBox(sort, filter)
	}
	t.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		a.updateHeader(id, o)
	}
	t.OnSelected = func(id widget.TableCellID) {
		a.onCellSelected(id)
	}
	// Show the full contents of a truncated cell on hover, and remember which row
	// the pointer is over so a right-click can act on it.
	t.OnHighlighted = func(id widget.TableCellID) {
		a.hoverRow = id.Row
		a.hoverCell(id, t.lastPos)
	}
	t.onLeave = a.hideTooltip
	t.onSecondary = a.onTableSecondary
	for i, ci := range a.visible {
		t.SetColumnWidth(i, a.cols[ci].width)
	}
	return t
}

func (a *App) updateCell(id widget.TableCellID, o fyne.CanvasObject) {
	stack, ok := o.(*fyne.Container)
	if !ok || len(stack.Objects) < 4 {
		return
	}
	bg, _ := stack.Objects[0].(*canvas.Rectangle)
	lbl, _ := stack.Objects[1].(*widget.Label)
	rich, _ := stack.Objects[2].(*widget.RichText)
	entry, _ := stack.Objects[3].(*inlineEntry)
	if lbl == nil || rich == nil || entry == nil {
		return
	}
	if id.Col < 0 || id.Col >= len(a.visible) || id.Row < 0 || id.Row >= a.view.Len() {
		lbl.SetText("")
		entry.Hide()
		rich.Hide()
		lbl.Show()
		return
	}
	master := a.view.Master(id.Row)
	ref := a.cols[a.visible[id.Col]].ref
	val := a.valueOf(master, ref)

	// Inline edit: this exact cell is being edited and the column is editable.
	if a.editing && id.Row == a.editRow && id.Col == a.editCol && a.cellEditable(ref) {
		lbl.Hide()
		rich.Hide()
		entry.SetText(val)
		entry.onCommit = func(s string) { a.commitInlineEdit(master, ref, s) }
		entry.onCancel = func() { a.cancelInlineEdit() }
		entry.Show()
		if !a.editFocused {
			a.editFocused = true
			if c := a.win.Canvas(); c != nil {
				c.Focus(entry)
			}
		}
	} else {
		entry.Hide()
		disp := oneLine(val)
		if spans := a.hl.Spans(ref, disp); len(spans) > 0 {
			rich.Segments = highlightSegments(disp, spans)
			rich.Refresh()
			lbl.Hide()
			rich.Show()
		} else {
			lbl.SetText(disp)
			lbl.Show()
			rich.Hide()
		}
	}

	if bg != nil {
		var want color.Color = color.Transparent
		if hex, ok := a.sess.RowColor(master); ok {
			want = rowTintColor(hex)
		}
		// Highlight every selected row, composited over any tag tint.
		if a.selected[master] || id.Row == a.selRow {
			base, _ := want.(color.NRGBA)
			want = over(base, selectionTint())
		}
		if !colorEq(bg.FillColor, want) {
			bg.FillColor = want
			bg.Refresh()
		}
	}
}

func (a *App) updateHeader(id widget.TableCellID, o fyne.CanvasObject) {
	box, ok := o.(*fyne.Container)
	if !ok {
		return
	}
	var btn *widget.Button
	var filter *widget.Entry
	for _, obj := range box.Objects {
		switch w := obj.(type) {
		case *widget.Button:
			btn = w
		case *widget.Entry:
			filter = w
		}
	}
	if btn == nil || filter == nil {
		return
	}
	// Only column headers are shown (ShowHeaderRow); guard other callbacks.
	if id.Col < 0 || id.Col >= len(a.visible) {
		btn.SetText("")
		btn.OnTapped = nil
		filter.OnChanged = nil
		filter.OnSubmitted = nil
		filter.SetText("")
		return
	}
	col := a.cols[a.visible[id.Col]]
	ref := col.ref
	title := col.title
	if arrow := a.sortArrow(ref); arrow != "" {
		title += " " + arrow
	}
	btn.SetText(title)
	btn.OnTapped = func() { a.sortByColumn(ref) }

	// Per-column filter box. Detach OnChanged before syncing the text so setting
	// it doesn't fire the handler; only overwrite when it actually differs, so a
	// refresh never disturbs the caret of a box being typed into.
	want := a.colFilter[ref]
	filter.OnChanged = nil
	if filter.Text != want {
		filter.SetText(want)
	}
	filter.OnChanged = func(s string) {
		if a.suppressFilter {
			return
		}
		a.setColFilter(ref, s) // stored now; Enter applies (see OnSubmitted)
	}
	filter.OnSubmitted = func(s string) {
		a.setColFilter(ref, s)
		a.applySearch()
	}
}

// setColFilter records (or clears) a column's quick-filter text.
func (a *App) setColFilter(ref model.ColumnRef, s string) {
	if a.colFilter == nil {
		a.colFilter = map[model.ColumnRef]string{}
	}
	if s == "" {
		delete(a.colFilter, ref)
		return
	}
	a.colFilter[ref] = s
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
	case model.ColRowNum:
		return strconv.Itoa(master + 1)
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

func (a *App) clearSelection() {
	if a.table != nil {
		a.table.UnselectAll()
	}
	a.selRow, a.selCol = -1, -1
	a.selected = map[int]bool{}
	a.anchorView = -1
	a.cancelInlineEdit()
}

// highlightSegments splits s into rich-text segments, colouring the byte ranges
// in spans (the filter matches) so they stand out from the surrounding text. All
// segments are inline, so the cell stays on one line.
func highlightSegments(s string, spans [][2]int) []widget.RichTextSegment {
	var segs []widget.RichTextSegment
	plain := func(text string) {
		if text != "" {
			segs = append(segs, &widget.TextSegment{Text: text, Style: widget.RichTextStyle{Inline: true}})
		}
	}
	pos := 0
	for _, sp := range spans {
		if sp[0] < pos || sp[1] > len(s) { // defensive: skip out-of-range spans
			continue
		}
		plain(s[pos:sp[0]])
		segs = append(segs, &widget.TextSegment{
			Text: s[sp[0]:sp[1]],
			Style: widget.RichTextStyle{
				Inline:    true,
				ColorName: theme.ColorNamePrimary,
				TextStyle: fyne.TextStyle{Bold: true},
			},
		})
		pos = sp[1]
	}
	plain(s[pos:])
	return segs
}

// oneLine collapses embedded newlines so a multi-line Summary shows as a single
// grid row; the full text is visible in the detail pane.
func oneLine(s string) string {
	if strings.IndexByte(s, '\n') < 0 && strings.IndexByte(s, '\r') < 0 {
		return s
	}
	return newlineReplacer.Replace(s)
}

var newlineReplacer = strings.NewReplacer("\r\n", " ⏎ ", "\n", " ⏎ ", "\r", " ⏎ ")
