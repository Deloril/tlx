package gui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// compactTheme tightens padding and text size so more rows fit on screen and
// each cell paints fewer pixels. Colours and fonts defer to the default theme,
// but the light/dark variant can be forced so the app has its own toggle
// independent of the OS setting.
type compactTheme struct {
	fyne.Theme
	force   bool
	variant fyne.ThemeVariant
}

func newCompactTheme(variant fyne.ThemeVariant) fyne.Theme {
	return &compactTheme{Theme: theme.DefaultTheme(), force: true, variant: variant}
}

func (c *compactTheme) Color(name fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if c.force {
		v = c.variant
	}
	// The default light theme's foreground is a mid grey that reads as washed
	// out against white; use a near-black so cell text has more contrast.
	if v == theme.VariantLight && name == theme.ColorNameForeground {
		return color.NRGBA{R: 0x14, G: 0x14, B: 0x14, A: 0xFF}
	}
	return c.Theme.Color(name, v)
}

func (c *compactTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNamePadding:
		return 2
	case theme.SizeNameInnerPadding:
		return 4
	case theme.SizeNameText:
		return 12
	case theme.SizeNameHeadingText:
		return 14
	case theme.SizeNameSeparatorThickness:
		return 1
	case theme.SizeNameScrollBarSmall:
		return 4
	}
	return c.Theme.Size(name)
}
