package gui

import (
	"math"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// submitEntry is a multi-line entry where a plain Enter fires OnSubmitted — the
// box's apply/save action — and Ctrl/Cmd+Enter inserts a newline. That is the
// reverse of Fyne's default (Enter makes a newline, Shift+Enter submits). A box
// that sets no OnSubmitted keeps Enter as a newline, so free-text fields (the
// comments panel, a per-line value list, an IOC indicator list) are unaffected.
type submitEntry struct {
	widget.Entry
	modDown bool // Ctrl or Cmd held, so Enter should insert a newline
}

func newSubmitEntry() *submitEntry {
	e := &submitEntry{}
	e.MultiLine = true
	e.Wrapping = fyne.TextWrapWord
	e.ExtendBaseWidget(e)
	return e
}

// isNewlineModifier reports whether a key is a modifier that turns Enter into a
// newline rather than a submit (Ctrl, or Cmd on macOS).
func isNewlineModifier(name fyne.KeyName) bool {
	switch name {
	case desktop.KeyControlLeft, desktop.KeyControlRight, desktop.KeySuperLeft, desktop.KeySuperRight:
		return true
	}
	return false
}

// KeyDown/KeyUp track the newline modifier the way Fyne's Entry tracks Shift; the
// KeyEvent passed to TypedKey carries no modifier field, so state must be kept.
func (e *submitEntry) KeyDown(key *fyne.KeyEvent) {
	if isNewlineModifier(key.Name) {
		e.modDown = true
	}
	e.Entry.KeyDown(key)
}

func (e *submitEntry) KeyUp(key *fyne.KeyEvent) {
	if isNewlineModifier(key.Name) {
		e.modDown = false
	}
	e.Entry.KeyUp(key)
}

func (e *submitEntry) TypedKey(key *fyne.KeyEvent) {
	if key.Name == fyne.KeyReturn || key.Name == fyne.KeyEnter {
		// Plain Enter submits; with the modifier held, fall through so Fyne inserts
		// a newline (it only submits on Shift, which we never combine here).
		if !e.modDown && e.OnSubmitted != nil {
			e.OnSubmitted(e.Text)
			return
		}
	}
	e.Entry.TypedKey(key)
}

// TypedShortcut catches Ctrl/Cmd+Enter. Fyne's driver turns a modifier+key press
// into a CustomShortcut routed here and then stops, so TypedKey (and the modDown
// tracking above) never sees the combo — this is the path that actually fires for
// Ctrl+Enter. On a multi-line box, insert a newline; everything else (copy, paste,
// word motion) goes to the embedded Entry.
func (e *submitEntry) TypedShortcut(s fyne.Shortcut) {
	if cs, ok := s.(*desktop.CustomShortcut); ok && e.MultiLine {
		if (cs.KeyName == fyne.KeyReturn || cs.KeyName == fyne.KeyEnter) &&
			cs.Modifier&(fyne.KeyModifierControl|fyne.KeyModifierSuper) != 0 {
			// Reuse Fyne's own insertion: Shift isn't held, so a multi-line entry
			// adds a newline here rather than submitting.
			e.Entry.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn})
			return
		}
	}
	e.Entry.TypedShortcut(s)
}

// growEntry is a multi-line entry that grows to fit its text and can be resized
// by dragging the grip along its bottom edge. Auto-fit tracks the wrapped line
// count (floored at minRows, capped at maxRows so one huge cell can't fill the
// pane); dragging the grip adds or removes rows on top of the fitted height.
type growEntry struct {
	submitEntry
	minRows   int
	maxRows   int
	extraRows int     // rows added (or, negative, removed) by dragging the grip
	shownRows int     // last value pushed to SetMinRowsVisible
	rowPx     float32 // cached height of one row, for converting drag to rows
	dragAccum float32 // sub-row drag distance not yet turned into a row step

	userChanged func(string) // caller's OnChanged, run after auto-fit
}

const growEntryMaxRows = 20

func newGrowEntry(minRows int) *growEntry {
	e := &growEntry{minRows: minRows, maxRows: growEntryMaxRows, shownRows: -1}
	e.MultiLine = true
	e.Wrapping = fyne.TextWrapWord
	e.ExtendBaseWidget(e)
	e.Entry.OnChanged = func(s string) {
		e.autofit()
		if e.userChanged != nil {
			e.userChanged(s)
		}
	}
	e.SetMinRowsVisible(minRows)
	return e
}

// newResizableEntry returns a growEntry and the object to place in the layout: a
// border with the entry above a drag grip.
func newResizableEntry(minRows int) (*growEntry, fyne.CanvasObject) {
	e := newGrowEntry(minRows)
	grip := newEntryGrip(e.nudge)
	return e, container.NewBorder(nil, grip, nil, nil, e)
}

// setOnChanged installs the caller's change handler without displacing the
// internal auto-fit hook (setting e.OnChanged directly would).
func (e *growEntry) setOnChanged(fn func(string)) { e.userChanged = fn }

// SetText refits after a programmatic change; setText also fires OnChanged, so
// autofit runs there too, but this covers callers that suppress the handler.
func (e *growEntry) SetText(s string) {
	e.Entry.SetText(s)
	e.autofit()
}

func (e *growEntry) Resize(s fyne.Size) {
	e.Entry.Resize(s)
	e.autofit() // width changed: rewrap and refit
}

func (e *growEntry) autofit() {
	rows := e.wrappedLineCount() + e.extraRows
	if rows < e.minRows {
		rows = e.minRows
	}
	if rows > e.maxRows {
		rows = e.maxRows
	}
	if rows == e.shownRows {
		return
	}
	e.shownRows = rows
	e.SetMinRowsVisible(rows)
	e.Refresh()
}

// wrappedLineCount estimates how many display rows the text needs at the current
// width. Before the first layout the width is zero, so it falls back to the
// logical line count.
func (e *growEntry) wrappedLineCount() int {
	th := e.Theme()
	ts := th.Size(theme.SizeNameText)
	avail := e.Size().Width - 2*th.Size(theme.SizeNameInnerPadding)
	total := 0
	for _, ln := range strings.Split(e.Text, "\n") {
		if avail <= 0 {
			total++
			continue
		}
		w := fyne.MeasureText(ln, ts, e.TextStyle).Width
		n := int(math.Ceil(float64(w / avail)))
		if n < 1 {
			n = 1
		}
		total += n
	}
	if total < 1 {
		total = 1
	}
	return total
}

func (e *growEntry) rowHeight() float32 {
	th := e.Theme()
	return fyne.MeasureText("Mg", th.Size(theme.SizeNameText), e.TextStyle).Height +
		th.Size(theme.SizeNameInnerPadding)
}

// nudge turns a vertical drag on the grip into whole-row height changes. It only
// grows below the fitted size and shrinks back to it, never below the content.
func (e *growEntry) nudge(dy float32) {
	if e.rowPx == 0 {
		e.rowPx = e.rowHeight()
	}
	e.dragAccum += dy
	for e.dragAccum >= e.rowPx {
		e.extraRows++
		e.dragAccum -= e.rowPx
	}
	for e.dragAccum <= -e.rowPx && e.extraRows > 0 {
		e.extraRows--
		e.dragAccum += e.rowPx
	}
	if e.extraRows < 0 {
		e.extraRows = 0
	}
	e.shownRows = -1 // force autofit to re-push even if the fitted count is unchanged
	e.autofit()
}

// entryGrip is the drag handle drawn under a growEntry.
type entryGrip struct {
	widget.BaseWidget
	onDrag func(dy float32)
}

func newEntryGrip(onDrag func(dy float32)) *entryGrip {
	g := &entryGrip{onDrag: onDrag}
	g.ExtendBaseWidget(g)
	return g
}

func (g *entryGrip) Dragged(e *fyne.DragEvent) {
	if g.onDrag != nil {
		g.onDrag(e.Dragged.DY)
	}
}
func (g *entryGrip) DragEnd()               {}
func (g *entryGrip) Cursor() desktop.Cursor { return desktop.VResizeCursor }
func (g *entryGrip) MinSize() fyne.Size     { return fyne.NewSize(24, 9) }
func (g *entryGrip) CreateRenderer() fyne.WidgetRenderer {
	col := g.Theme().Color(theme.ColorNameInputBorder, fyne.CurrentApp().Settings().ThemeVariant())
	r := &gripRenderer{g: g}
	for i := range r.lines {
		l := canvas.NewLine(col)
		l.StrokeWidth = 1
		r.lines[i] = l
	}
	return r
}

type gripRenderer struct {
	g     *entryGrip
	lines [2]*canvas.Line
}

func (r *gripRenderer) Layout(s fyne.Size) {
	// Two short centred lines, like a resize handle.
	const halfW = 8
	cx := s.Width / 2
	y := s.Height / 2
	for i, l := range r.lines {
		yy := y - 2 + float32(i)*4
		l.Position1 = fyne.NewPos(cx-halfW, yy)
		l.Position2 = fyne.NewPos(cx+halfW, yy)
	}
}
func (r *gripRenderer) MinSize() fyne.Size { return r.g.MinSize() }
func (r *gripRenderer) Refresh() {
	col := r.g.Theme().Color(theme.ColorNameInputBorder, fyne.CurrentApp().Settings().ThemeVariant())
	for _, l := range r.lines {
		l.StrokeColor = col
		l.Refresh()
	}
}
func (r *gripRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.lines[0], r.lines[1]}
}
func (r *gripRenderer) Destroy() {}
