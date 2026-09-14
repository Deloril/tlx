package gui

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// rightmostText returns the largest right edge of any canvas.Text under o, in o's
// coordinate space — how far the painted glyphs actually reach.
func rightmostText(o fyne.CanvasObject, offX float32, out *float32) {
	switch v := o.(type) {
	case *canvas.Text:
		if r := offX + v.Position().X + v.MinSize().Width; r > *out {
			*out = r
		}
	case *fyne.Container:
		for _, c := range v.Objects {
			rightmostText(c, offX+v.Position().X, out)
		}
	}
}

func richRightEdge(t *testing.T, segs []widget.RichTextSegment, width float32) float32 {
	t.Helper()
	r := widget.NewRichText(segs...)
	r.Truncation = fyne.TextTruncateEllipsis
	r.Wrapping = fyne.TextWrapOff
	w := test.NewWindow(container.NewStack(r))
	defer w.Close()
	w.Resize(fyne.NewSize(width, 40))
	var edge float32
	for _, o := range test.WidgetRenderer(r).Objects() {
		rightmostText(o, 0, &edge)
	}
	return edge
}

// TestHighlightDoesNotOverflow checks the fix for highlighted match cells
// painting past their column. A raw multi-segment RichText overflows; the same
// text run through fitHighlight must stay within the column width.
func TestHighlightDoesNotOverflow(t *testing.T) {
	test.NewApp()
	const width float32 = 120
	long := strings.Repeat("alpha bravo charlie delta ", 6) // ~156 chars
	spans := [][2]int{{0, 5}, {40, 45}, {120, 125}}

	// Baseline: without clipping the rich paints well past the column.
	raw := richRightEdge(t, highlightSegments(long, spans), width)
	if raw <= width {
		t.Skipf("raw rich already fit (%.1f <= %.1f); nothing to prove here", raw, width)
	}

	td, ts := fitHighlight(long, spans, width)
	if td == long {
		t.Fatalf("fitHighlight did not truncate a %d-char string into %v px", len(long), width)
	}
	fitted := richRightEdge(t, highlightSegments(td, ts), width)
	if fitted > width {
		t.Errorf("highlighted cell still overflows: painted edge %.1f > column width %.1f", fitted, width)
	}
}

// TestFitHighlightSpans checks the span bookkeeping: matches past the cut are
// dropped, a straddling match is trimmed, and the input slice isn't mutated.
func TestFitHighlightSpans(t *testing.T) {
	test.NewApp()
	long := strings.Repeat("x", 400)
	spans := [][2]int{{0, 3}, {390, 395}}
	orig := [][2]int{{0, 3}, {390, 395}}

	td, ts := fitHighlight(long, spans, 100)
	body := len(strings.TrimSuffix(td, "…"))
	for _, sp := range ts {
		if sp[0] < 0 || sp[1] > body || sp[0] > sp[1] {
			t.Errorf("span %v out of range for body length %d", sp, body)
		}
	}
	if spans[0] != orig[0] || spans[1] != orig[1] {
		t.Errorf("fitHighlight mutated caller spans: %v", spans)
	}
}
