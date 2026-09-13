package gui

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// dialogSize returns a comfortable size for a custom dialog: a fraction of the
// window, clamped so it is never cramped nor larger than the window. Fyne sizes
// custom dialogs to their content otherwise, which comes out tiny.
func (a *App) dialogSize(maxW, maxH float32) fyne.Size {
	ws := a.win.Canvas().Size()
	w := ws.Width * 0.7
	h := ws.Height * 0.8
	if w < 460 {
		w = 460
	}
	if h < 360 {
		h = 360
	}
	if w > maxW {
		w = maxW
	}
	if h > maxH {
		h = maxH
	}
	// Never exceed the window.
	if ws.Width > 0 && w > ws.Width-40 {
		w = ws.Width - 40
	}
	if ws.Height > 0 && h > ws.Height-40 {
		h = ws.Height - 40
	}
	return fyne.NewSize(w, h)
}

// Theme toggle.

func (a *App) toggleTheme() {
	if a.themeVariant == theme.VariantDark {
		a.themeVariant = theme.VariantLight
	} else {
		a.themeVariant = theme.VariantDark
	}
	a.fyne.Settings().SetTheme(newCompactTheme(a.themeVariant))
	a.updateThemeButton()
}

// updateThemeButton labels the button with the mode it will switch to.
func (a *App) updateThemeButton() {
	if a.themeBtn == nil {
		return
	}
	if a.themeVariant == theme.VariantDark {
		a.themeBtn.SetText("Light")
	} else {
		a.themeBtn.SetText("Dark")
	}
}

// filterRow is one structured per-column condition in the filter window
// (see filterwin.go).
type filterRow struct {
	colSel  *widget.Select
	values  *growEntry
	allChk  *widget.Check
	reChk   *widget.Check
	negChk  *widget.Check // "exclude": invert the condition (the ≠ case)
	box     *fyne.Container
	removed bool
}

// splitLines splits on newlines, trims each, and drops blanks.
func splitLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out
}
