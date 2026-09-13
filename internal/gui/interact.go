package gui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
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

// onCellSelected runs when a grid cell is clicked. A plain click selects the one
// row, updates the detail pane, and drops into inline editing or the tag
// drop-down depending on the column. Shift-click extends a range from the anchor
// row; Ctrl/Cmd-click toggles the clicked row in or out of the selection. The
// modifier click is a selection gesture only — it never starts an edit.
func (a *App) onCellSelected(id widget.TableCellID) {
	a.selRow, a.selCol = id.Row, id.Col
	if id.Row < 0 || id.Row >= a.view.Len() || id.Col < 0 || id.Col >= len(a.visible) {
		return
	}
	mod := a.table.lastMod
	a.table.lastMod = 0 // consume; a later programmatic Select must read as plain
	toggle := mod&(fyne.KeyModifierControl|fyne.KeyModifierSuper) != 0
	rangeSel := mod&fyne.KeyModifierShift != 0

	master := a.view.Master(id.Row)
	switch {
	case rangeSel && a.anchorView >= 0:
		a.selectRange(a.anchorView, id.Row)
	case toggle:
		if a.selected[master] {
			delete(a.selected, master)
		} else {
			a.selected[master] = true
		}
		a.anchorView = id.Row
	default:
		a.selected = map[int]bool{master: true}
		a.anchorView = id.Row
	}

	if a.sidebarVisible {
		a.showDetail(master)
	}

	ref := a.cols[a.visible[id.Col]].ref
	if toggle || rangeSel {
		a.cancelInlineEdit()
	} else {
		switch {
		case ref == model.ColTags && a.sess.Mode() != model.ReadOnly:
			a.cancelInlineEdit()
			a.editTagsPopup(master)
		case a.cellEditable(ref):
			a.startInlineEdit(id.Row, id.Col)
		default:
			a.cancelInlineEdit()
		}
	}
	// Drop the table's own single-cell selection so the next click — even on the
	// same cell — fires OnSelected again; Fyne's table early-returns when a cell
	// is re-selected. Our highlight is driven by a.selected, not the table's.
	a.table.Table.UnselectAll()
	a.table.Refresh()
}

// selectRange sets the selection to every row between two view positions
// (inclusive), keyed by master index so it survives later sorts.
func (a *App) selectRange(fromView, toView int) {
	if fromView > toView {
		fromView, toView = toView, fromView
	}
	if fromView < 0 {
		fromView = 0
	}
	if toView >= a.view.Len() {
		toView = a.view.Len() - 1
	}
	a.selected = map[int]bool{}
	for r := fromView; r <= toView; r++ {
		a.selected[a.view.Master(r)] = true
	}
}

// selectedMasters returns the selected rows as a sorted slice of master indices.
func (a *App) selectedMasters() []int {
	out := make([]int, 0, len(a.selected))
	for m := range a.selected {
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

// onTableSecondary opens the row context menu at a right-click. If the clicked
// row is not already part of the selection, it becomes the sole selection first,
// so a plain right-click acts on the row under the pointer.
func (a *App) onTableSecondary(pos fyne.Position) {
	if a.view == nil || a.view.Len() == 0 {
		return
	}
	if a.hoverRow >= 0 && a.hoverRow < a.view.Len() {
		m := a.view.Master(a.hoverRow)
		if !a.selected[m] {
			a.selected = map[int]bool{m: true}
			a.selRow, a.selCol = a.hoverRow, 0
			a.anchorView = a.hoverRow
			if a.sidebarVisible {
				a.showDetail(m)
			}
			a.table.Refresh()
		}
	}
	if len(a.selected) == 0 {
		return
	}
	a.showSelectionMenu(pos)
}

// showSelectionMenu pops up the bulk-action menu for the current selection.
func (a *App) showSelectionMenu(pos fyne.Position) {
	n := len(a.selected)
	readOnly := a.sess == nil || a.sess.Mode() == model.ReadOnly

	tag := fyne.NewMenuItem(fmt.Sprintf("Tag %s…", plural(n, "row")), a.bulkTag)
	comment := fyne.NewMenuItem(fmt.Sprintf("Comment %s…", plural(n, "row")), a.bulkComment)
	tag.Disabled = readOnly
	comment.Disabled = readOnly
	clear := fyne.NewMenuItem("Clear selection", func() {
		a.clearSelection()
		a.refreshTable()
	})
	items := []*fyne.MenuItem{tag, comment}
	if ni := a.noteMenuItems(); len(ni) > 0 {
		items = append(items, fyne.NewMenuItemSeparator())
		items = append(items, ni...)
	}
	if ts := a.timeWindowMenuItem(); ts != nil {
		items = append(items, fyne.NewMenuItemSeparator(), ts)
	}
	items = append(items, fyne.NewMenuItemSeparator(), clear)
	widget.NewPopUpMenu(fyne.NewMenu("", items...), a.win.Canvas()).ShowAtPosition(pos)
}

// timeWindowMenuItem offers a ±5-minute filter around the right-clicked cell,
// but only when that cell holds a parseable timestamp. Returns nil otherwise.
func (a *App) timeWindowMenuItem() *fyne.MenuItem {
	row, col := a.hoverRow, a.hoverCol
	if a.view == nil || row < 0 || row >= a.view.Len() || col < 0 || col >= len(a.visible) {
		return nil
	}
	ref := a.cols[a.visible[col]].ref
	if ref < 0 { // virtual columns (#, Tags, Comment) hold no timestamp
		return nil
	}
	headers := a.idx.Headers()
	if int(ref) >= len(headers) {
		return nil
	}
	t, ok := model.ParseTime(a.valueOf(a.view.Master(row), ref))
	if !ok {
		return nil
	}
	field := headers[int(ref)] // query resolves fields against source headers, not display names
	return fyne.NewMenuItem("Filter ±5 min around this time", func() {
		a.applyTimeWindow(field, t, 5*time.Minute)
	})
}

// applyTimeWindow replaces the query with one keeping rows whose column falls in
// [t-d, t+d], then applies it. The field is quoted so a header with spaces still
// parses, and operands are RFC3339 so any timezone offset is preserved.
func (a *App) applyTimeWindow(field string, t time.Time, d time.Duration) {
	expr := fmt.Sprintf("%s between %s and %s",
		quoteQueryField(field), t.Add(-d).Format(time.RFC3339), t.Add(d).Format(time.RFC3339))
	a.suppressFilter = true
	a.search.SetText(expr)
	a.suppressFilter = false
	a.applySearch()
}

// quoteQueryField wraps a field name in double quotes for the query language,
// escaping backslashes and quotes the way readAtom unwraps them.
func quoteQueryField(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// plural renders "1 row" / "3 rows".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// editTagsPopup shows a drop-down of the tag palette anchored at the pointer.
// Ticking a tag adds it to the row, unticking removes it, live.
func (a *App) editTagsPopup(master int) {
	defs := a.sess.TagDefs()
	has := map[string]bool{}
	for _, t := range a.sess.Tags(master) {
		has[t] = true
	}

	box := container.NewVBox(widget.NewLabelWithStyle(
		"Tags for row "+strconv.Itoa(master+1), fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))

	var pop *widget.PopUp
	for _, d := range defs {
		d := d
		chk := widget.NewCheck(d.Name, func(on bool) {
			var err error
			if on {
				err = a.sess.AddTag(master, d.Name)
			} else {
				err = a.sess.RemoveTag(master, d.Name)
			}
			if err != nil {
				a.showError(err)
				return
			}
			if a.sidebarVisible {
				a.showDetail(master)
			}
			a.refreshTable()
		})
		chk.SetChecked(has[d.Name])
		del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
			if pop != nil {
				pop.Hide()
			}
			a.confirmDeleteTag(d.Name)
		})
		del.Importance = widget.LowImportance
		box.Add(container.NewBorder(nil, nil, container.NewHBox(colorSquare(d.Color), chk), del))
	}

	box.Add(widget.NewSeparator())
	box.Add(container.NewHBox(
		widget.NewButtonWithIcon("New tag…", theme.ContentAddIcon(), func() {
			if pop != nil {
				pop.Hide()
			}
			a.tagSelected()
		}),
		widget.NewButton("Close", func() {
			if pop != nil {
				pop.Hide()
			}
		}),
	))

	h := float32(80 + len(defs)*30)
	if h > 380 {
		h = 380
	}
	pop = widget.NewPopUp(container.NewVScroll(box), a.win.Canvas())
	pop.Resize(fyne.NewSize(260, h))
	pop.ShowAtPosition(a.table.lastPos)
}

// confirmDeleteTag asks before removing a tag everywhere it is used.
func (a *App) confirmDeleteTag(name string) {
	dialog.NewConfirm("Delete tag",
		fmt.Sprintf("Delete the tag %q and remove it from every row?", name),
		func(ok bool) {
			if ok {
				a.deleteTag(name)
			}
		}, a.win).Show()
}

// deleteTag removes a tag from the session palette and every row it is on. In a
// case it also removes the tag from every timeline in the case database, so it
// is gone from the master view too.
func (a *App) deleteTag(name string) {
	if a.sess == nil {
		return
	}
	if err := a.sess.DeleteTag(name); err != nil {
		a.showError(err)
		return
	}
	if a.cse != nil {
		if err := a.cse.DeleteTag(name); err != nil {
			a.showError(err)
			return
		}
	}
	// Drop the tag from the tag-filter selection so a deleted tag can't keep
	// filtering the view to nothing.
	if a.tagFilter[name] {
		delete(a.tagFilter, name)
		if a.view != nil {
			a.applySearch()
		}
	}
	if a.sidebarVisible && a.selectedMaster() >= 0 {
		a.showDetail(a.selectedMaster())
	}
	a.refreshTable()
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

// toggleSidebar shows or hides the right dock (filter + detail panels).
func (a *App) toggleSidebar() {
	a.setSidebar(!a.sidebarVisible)
}

func (a *App) setSidebar(show bool) {
	a.sidebarVisible = show
	if a.rightScroll == nil || a.split == nil {
		return
	}
	if show {
		// Take the right pane only if a panel is actually docked; if every panel
		// has floated out there is nothing to show here.
		if a.rightDockBox != nil && len(a.rightDockBox.Objects) > 0 {
			a.rightScroll.Show()
			a.split.SetOffset(a.rightDockOffset)
		}
		if m := a.selectedMaster(); m >= 0 {
			a.showDetail(m)
		}
	} else {
		a.rightScroll.Hide()
		a.split.SetOffset(1.0)
	}
	a.split.Refresh()
}

// revealPanel brings a right-dock panel into view: it shows the dock if hidden
// (unless the panel has floated into its own window), then expands or raises it.
// Mirrors revealFilterPanel for panels that have nothing to type into.
func (a *App) revealPanel(p *dockPanel) {
	if a.idx == nil || p == nil {
		return
	}
	if !a.sidebarVisible && !p.floating {
		a.setSidebar(true)
	}
	// Collapse the other docked panels so the intended one gets the space.
	// Floating panels have their own window, so leave them be.
	for _, other := range a.rightPanels {
		if other == nil || other == p || other.floating {
			continue
		}
		other.setExpanded(false)
	}
	p.focus() // expands if docked, raises the window if floating
}

// revealDetailsPanel drops straight to the Details panel and fills it with the
// current selection, so the toolbar button works even when nothing changed the
// selection since the dock was last hidden.
func (a *App) revealDetailsPanel() {
	a.revealPanel(a.detailPanel)
	if m := a.selectedMaster(); m >= 0 {
		a.showDetail(m)
	}
}

// toggleViewsSidebar shows or hides the left sidebar (Views/Case/IOC sections).
func (a *App) toggleViewsSidebar() {
	a.setViewsSidebar(!a.viewsSidebarVisible)
}

func (a *App) setViewsSidebar(show bool) {
	a.viewsSidebarVisible = show
	if a.leftScroll == nil || a.outerSplit == nil {
		return
	}
	if show {
		a.refreshViewsSection()
		a.refreshCaseSection()
		a.refreshIOCSection()
		a.leftScroll.Show()
		a.outerSplit.SetOffset(viewsSidebarOffset)
	} else {
		a.leftScroll.Hide()
		a.outerSplit.SetOffset(0.0)
	}
	a.outerSplit.Refresh()
}

// Double-click. Two plain presses on the same cell within doubleClickInterval
// pop a non-editable cell's full contents into their own window.

const doubleClickInterval = 300 * time.Millisecond

// onTablePress times consecutive presses on the same cell. The hover row/col are
// current at press time (the pointer is over the cell), so they identify it.
func (a *App) onTablePress() {
	now := time.Now()
	row, col := a.hoverRow, a.hoverCol
	if row < 0 || col < 0 {
		a.lastClickAt = time.Time{}
		return
	}
	if !a.lastClickAt.IsZero() && now.Sub(a.lastClickAt) <= doubleClickInterval &&
		row == a.lastClickRow && col == a.lastClickCol {
		a.lastClickAt = time.Time{} // consume, so a third press starts fresh
		a.onCellDoubleClick(row, col)
		return
	}
	a.lastClickAt = now
	a.lastClickRow, a.lastClickCol = row, col
}

// onCellDoubleClick pops out a cell's contents, but only for columns that can't
// be edited inline — an editable cell is already in an edit box by now.
func (a *App) onCellDoubleClick(row, col int) {
	if a.view == nil || row < 0 || row >= a.view.Len() || col < 0 || col >= len(a.visible) {
		return
	}
	c := a.cols[a.visible[col]]
	if a.cellEditable(c.ref) {
		return
	}
	a.showCellPopout(c.title, a.view.Master(row), c.ref)
}

// showCellPopout opens a small window with the full cell text, selectable and
// copyable (and note-capturable, like the detail pane), for a value too big to
// read in the grid.
func (a *App) showCellPopout(title string, master int, ref model.ColumnRef) {
	a.hideTooltip()
	body := newSelectableLabel(a, a.valueOf(master, ref))
	label := widget.NewLabelWithStyle(
		fmt.Sprintf("%s — row %d", title, master+1), fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	w := a.fyne.NewWindow(title + " — tlx")
	onTop := false
	header := container.NewBorder(nil, nil, label, newAlwaysOnTopButton(w, &onTop), nil)
	w.SetContent(container.NewBorder(header, nil, nil, nil, container.NewVScroll(body)))
	w.Resize(fyne.NewSize(520, 360))
	w.Show()
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
	// A huge cell would make a tooltip taller than the screen, so cap it. Spans
	// are computed on the capped text (not the one-line form) so the byte offsets
	// line up with what the tooltip actually renders.
	shown := capTooltipText(full)
	a.showTooltip(shown, a.hl.Spans(ref, shown), at)
}

const (
	tooltipMaxLines   = 15  // most rows the hover box will show
	tooltipMaxLineLen = 300 // longest single line, so one line can't wrap off-screen
	tooltipWrapWidth  = 460 // width the hover text wraps at
)

// capTooltipText limits the hover text to the first tooltipMaxLines lines and
// bounds each line's length. When either limit trims content the last line
// becomes an ellipsis, so it is clear more was hidden.
func capTooltipText(s string) string {
	lines := strings.Split(s, "\n")
	trimmed := false
	if len(lines) > tooltipMaxLines {
		lines = lines[:tooltipMaxLines-1]
		trimmed = true
	}
	for i, ln := range lines {
		if len(ln) > tooltipMaxLineLen {
			lines[i] = ln[:tooltipMaxLineLen] + "…"
			trimmed = true
		}
	}
	if trimmed {
		lines = append(lines, "…")
	}
	return strings.Join(lines, "\n")
}

func (a *App) showTooltip(text string, spans [][2]int, at fyne.Position) {
	if a.hoverLayer == nil {
		return
	}
	// Re-read the box colours from the active theme every time. The rectangle's
	// fill is a static value that Fyne does not refresh on a theme toggle, so
	// without this the box keeps whatever variant it was built with (dark box,
	// dark text — unreadable in light mode).
	th := a.fyne.Settings().Theme()
	a.hoverBG.FillColor = th.Color(theme.ColorNameInputBackground, a.themeVariant)
	a.hoverBG.StrokeColor = th.Color(theme.ColorNameInputBorder, a.themeVariant)

	if len(spans) > 0 {
		a.hoverText.Segments = highlightSegments(text, spans)
	} else {
		a.hoverText.Segments = []widget.RichTextSegment{&widget.TextSegment{Text: text}}
	}
	a.hoverText.Wrapping = fyne.TextWrapWord
	// RichText caches its MinSize and only recomputes row wrapping (against its
	// current width) on Refresh. Assigning Segments alone leaves both stale, so
	// without this the box keeps the previous cell's height. Pin the wrap width,
	// Refresh to recompute at that width, then measure.
	a.hoverText.Resize(fyne.NewSize(tooltipWrapWidth, a.hoverText.Size().Height))
	a.hoverText.Refresh()
	a.hoverText.Resize(fyne.NewSize(tooltipWrapWidth, a.hoverText.MinSize().Height))

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
	a.hoverBG = canvas.NewRectangle(theme.Color(theme.ColorNameInputBackground))
	a.hoverBG.StrokeColor = theme.Color(theme.ColorNameInputBorder)
	a.hoverBG.StrokeWidth = 1
	a.hoverBG.CornerRadius = 4
	a.hoverText = widget.NewRichTextWithText("")
	a.hoverLayer = container.NewWithoutLayout(a.hoverBG, a.hoverText)
	a.hoverLayer.Hide()
}
