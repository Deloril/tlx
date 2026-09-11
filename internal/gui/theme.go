package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// compactTheme tightens padding and text size so more rows fit on screen and
// each cell paints fewer pixels. It defers colours and fonts to the default
// dark/light theme.
type compactTheme struct {
	fyne.Theme
}

func newCompactTheme() fyne.Theme {
	return &compactTheme{Theme: theme.DefaultTheme()}
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
