package gui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// The bottom bar and the docked query box share their text: typing in either
// updates the other, without the two callbacks bouncing the change back and
// forth (the qSync guard).
func TestFilterBarSharesTextWithSearch(t *testing.T) {
	app := test.NewApp()
	defer test.NewApp()
	a := &App{fyne: app, themeVariant: theme.VariantDark}
	a.buildFilterWidgets()
	a.buildFilterBar()

	a.filterBarEntry.SetText("tag=bad")
	if a.search.Text != "tag=bad" {
		t.Fatalf("bar -> search: search.Text = %q, want %q", a.search.Text, "tag=bad")
	}

	a.search.SetText("Host=dc1")
	if a.filterBarEntry.Text != "Host=dc1" {
		t.Fatalf("search -> bar: bar.Text = %q, want %q", a.filterBarEntry.Text, "Host=dc1")
	}
}

// The coloured layer tags each run with the role colour: field orange, operator
// blue, value green.
func TestFilterSegmentsColourByRole(t *testing.T) {
	segs := filterSegments("Summary=svchost")
	got := map[string]fyne.ThemeColorName{}
	for _, s := range segs {
		ts, ok := s.(*widget.TextSegment)
		if !ok {
			t.Fatalf("segment %T is not a TextSegment", s)
		}
		got[ts.Text] = ts.Style.ColorName
	}
	if got["Summary"] != cnFilterField {
		t.Errorf("field colour = %q, want %q", got["Summary"], cnFilterField)
	}
	if got["="] != cnFilterCond {
		t.Errorf("operator colour = %q, want %q", got["="], cnFilterCond)
	}
	if got["svchost"] != cnFilterParam {
		t.Errorf("value colour = %q, want %q", got["svchost"], cnFilterParam)
	}
}

// Every kind resolves to a distinct, non-transparent colour under both variants,
// so the highlighting reads on light and dark themes alike.
func TestFilterColorThemeDistinctColours(t *testing.T) {
	app := test.NewApp()
	defer test.NewApp()
	for _, v := range []fyne.ThemeVariant{theme.VariantDark, theme.VariantLight} {
		a := &App{fyne: app, themeVariant: v}
		th := &filterColorTheme{a: a}
		seen := map[string]bool{}
		for _, name := range []fyne.ThemeColorName{cnFilterField, cnFilterCond, cnFilterParam} {
			c := th.Color(name, v)
			_, _, _, alpha := c.RGBA()
			if alpha == 0 {
				t.Errorf("variant %v: %q is transparent", v, name)
			}
			key := colourKey(c)
			if seen[key] {
				t.Errorf("variant %v: %q duplicates another role colour", v, name)
			}
			seen[key] = true
		}
	}
}

func colourKey(c interface{ RGBA() (r, g, b, a uint32) }) string {
	r, g, b, _ := c.RGBA()
	return string(rune(r)) + string(rune(g)) + string(rune(b))
}
