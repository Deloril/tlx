package gui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// The filter bar is a permanent, always-visible query box across the bottom of
// the window, in the style of Timeline Explorer. It shares its text with the
// docked Filter panel's query box (a.search): typing in either updates the
// other, and Enter applies. The text is colour-coded as you type — field names
// orange, operators and keywords blue, values green — using an invisible entry
// laid over a rich-text layer that redraws the same characters in colour.

// Custom theme colour names for the three query roles. A ThemeOverride wrapping
// the rich-text layer resolves them (see filterColorTheme); the standard theme
// never sees them.
const (
	cnFilterField fyne.ThemeColorName = "tlxFilterField"
	cnFilterCond  fyne.ThemeColorName = "tlxFilterCond"
	cnFilterParam fyne.ThemeColorName = "tlxFilterParam"
)

// appTheme is the theme currently in force, so the bar's wrappers track the
// light/dark toggle instead of a snapshot taken when the bar was built.
func (a *App) appTheme() fyne.Theme { return a.fyne.Settings().Theme() }

// buildFilterBar creates the bottom query bar and returns the object to place in
// the window. It builds three stacked layers — a background fill, the coloured
// rich-text display, and a transparent-text entry that takes the typing — with a
// label at the left and a clear button at the right.
func (a *App) buildFilterBar() fyne.CanvasObject {
	a.filterBarEntry = widget.NewEntry()
	a.filterBarEntry.TextStyle = fyne.TextStyle{Monospace: true}
	a.filterBarEntry.Wrapping = fyne.TextWrapOff
	a.filterBarEntry.Scroll = fyne.ScrollNone // no horizontal scroll: text keeps a fixed origin, so the colour layer stays aligned
	a.filterBarEntry.SetPlaceHolder("Filter:  Summary=svchost AND (tag=bad OR tag=suspicious)   —   Enter applies, /regex/ for regex")
	a.filterBarEntry.OnChanged = func(s string) {
		if a.qSync {
			return
		}
		a.qSync = true
		if a.search != nil {
			a.search.SetText(s)
		}
		a.qSync = false
		a.refreshFilterBarColors()
	}
	a.filterBarEntry.OnSubmitted = func(string) { a.commitFilter() }
	if a.search != nil {
		a.filterBarEntry.SetText(a.search.Text)
	}

	a.filterBarRT = widget.NewRichText()
	a.filterBarRT.Wrapping = fyne.TextWrapOff
	a.refreshFilterBarColors()

	a.filterBarBG = canvas.NewRectangle(a.appTheme().Color(theme.ColorNameInputBackground, a.themeVariant))

	a.filterBarRTWrap = container.NewThemeOverride(a.filterBarRT, &filterColorTheme{a: a})
	entryWrap := container.NewThemeOverride(a.filterBarEntry, &ghostEntryTheme{a: a})

	stack := container.NewStack(a.filterBarBG, a.filterBarRTWrap, entryWrap)

	label := widget.NewLabelWithStyle("Filter", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	clearBtn := widget.NewButtonWithIcon("", theme.ContentClearIcon(), a.clearFilter)
	clearBtn.Importance = widget.LowImportance

	bar := container.NewBorder(nil, nil, label, clearBtn, stack)
	return container.NewVBox(widget.NewSeparator(), bar)
}

// refreshFilterBarColors rebuilds the coloured rich-text layer from the current
// query text. Cheap enough to run on every keystroke.
func (a *App) refreshFilterBarColors() {
	if a.filterBarRT == nil {
		return
	}
	txt := ""
	if a.filterBarEntry != nil {
		txt = a.filterBarEntry.Text
	}
	a.filterBarRT.Segments = filterSegments(txt)
	if a.filterBarRTWrap != nil {
		a.filterBarRTWrap.Refresh() // re-applies the colour theme to the fresh segments
	} else {
		a.filterBarRT.Refresh()
	}
}

// filterSegments turns a query string into inline rich-text segments coloured by
// role. An empty query yields a single empty segment so the widget has content.
func filterSegments(s string) []widget.RichTextSegment {
	spans := model.FilterSpans(s)
	if len(spans) == 0 {
		return []widget.RichTextSegment{&widget.TextSegment{Style: filterSegStyle("")}}
	}
	segs := make([]widget.RichTextSegment, 0, len(spans))
	for _, sp := range spans {
		segs = append(segs, &widget.TextSegment{
			Text:  s[sp.Start:sp.End],
			Style: filterSegStyle(colorNameFor(sp.Kind)),
		})
	}
	return segs
}

func filterSegStyle(cn fyne.ThemeColorName) widget.RichTextStyle {
	return widget.RichTextStyle{
		Inline:    true,
		ColorName: cn,
		TextStyle: fyne.TextStyle{Monospace: true},
	}
}

func colorNameFor(k model.FilterSpanKind) fyne.ThemeColorName {
	switch k {
	case model.SpanField:
		return cnFilterField
	case model.SpanCond:
		return cnFilterCond
	case model.SpanParam:
		return cnFilterParam
	default:
		return "" // plain text uses the default foreground
	}
}

// filterColorTheme themes the rich-text layer: it answers the three custom
// colour names with the role colours and delegates everything else to the live
// app theme (so text size and fonts match the rest of the window). Colours are
// chosen against the app's forced variant, not the OS one.
type filterColorTheme struct{ a *App }

func (t *filterColorTheme) Color(name fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	light := t.a.themeVariant == theme.VariantLight
	switch name {
	case cnFilterField: // orange
		if light {
			return color.NRGBA{R: 0xB0, G: 0x5A, B: 0x00, A: 0xFF}
		}
		return color.NRGBA{R: 0xE0, G: 0x9B, B: 0x3D, A: 0xFF}
	case cnFilterCond: // blue
		if light {
			return color.NRGBA{R: 0x15, G: 0x5A, B: 0xC0, A: 0xFF}
		}
		return color.NRGBA{R: 0x5A, G: 0xA0, B: 0xF2, A: 0xFF}
	case cnFilterParam: // green
		if light {
			return color.NRGBA{R: 0x2E, G: 0x7D, B: 0x32, A: 0xFF}
		}
		return color.NRGBA{R: 0x6B, G: 0xCB, B: 0x77, A: 0xFF}
	}
	return t.a.appTheme().Color(name, v)
}

func (t *filterColorTheme) Font(s fyne.TextStyle) fyne.Resource     { return t.a.appTheme().Font(s) }
func (t *filterColorTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return t.a.appTheme().Icon(n) }
func (t *filterColorTheme) Size(n fyne.ThemeSizeName) float32       { return t.a.appTheme().Size(n) }

// ghostEntryTheme makes the editing entry's own glyphs, background and border
// invisible so the coloured layer shows through, while leaving the cursor
// (primary), selection and placeholder colours untouched. Its input border is
// zeroed so the text sits at the same origin as the rich-text layer.
type ghostEntryTheme struct{ a *App }

func (t *ghostEntryTheme) Color(name fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameForeground, theme.ColorNameInputBackground, theme.ColorNameInputBorder, theme.ColorNameDisabled:
		return color.Transparent
	}
	return t.a.appTheme().Color(name, v)
}

func (t *ghostEntryTheme) Font(s fyne.TextStyle) fyne.Resource     { return t.a.appTheme().Font(s) }
func (t *ghostEntryTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return t.a.appTheme().Icon(n) }
func (t *ghostEntryTheme) Size(n fyne.ThemeSizeName) float32 {
	if n == theme.SizeNameInputBorder {
		return 0
	}
	return t.a.appTheme().Size(n)
}
