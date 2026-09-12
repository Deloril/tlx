package gui

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"timeline-engine/internal/model"
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

// Per-column filter dialog.

type filterRow struct {
	colSel  *widget.Select
	values  *widget.Entry
	allChk  *widget.Check
	reChk   *widget.Check
	box     *fyne.Container
	removed bool
}

func (a *App) columnFilter() {
	// Column choices: "Any column" plus every logical column, with a two-way map.
	titles := []string{"Any column"}
	refByTitle := map[string]model.ColumnRef{"Any column": model.ColAll}
	titleByRef := map[model.ColumnRef]string{model.ColAll: "Any column"}
	for i := range a.cols {
		t := a.cols[i].title
		titles = append(titles, t)
		refByTitle[t] = a.cols[i].ref
		titleByRef[a.cols[i].ref] = t
	}

	rows := []*filterRow{}
	rowsBox := container.NewVBox()

	var addRow func(cond *model.ColumnCond)
	addRow = func(cond *model.ColumnCond) {
		r := &filterRow{
			colSel: widget.NewSelect(titles, nil),
			values: widget.NewMultiLineEntry(),
			allChk: widget.NewCheck("all values", nil),
			reChk:  widget.NewCheck("regex", nil),
		}
		r.values.SetPlaceHolder("one value per line")
		r.values.SetMinRowsVisible(2)
		r.colSel.SetSelected("Any column")
		if cond != nil {
			if t, ok := titleByRef[cond.Column]; ok {
				r.colSel.SetSelected(t)
			}
			r.values.SetText(strings.Join(cond.Values, "\n"))
			r.allChk.SetChecked(cond.All)
			r.reChk.SetChecked(cond.Regexp)
		}
		removeBtn := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
			r.removed = true
			rowsBox.Remove(r.box)
			rowsBox.Refresh()
		})
		// Column select on top; values with the option checks and remove beside.
		opts := container.NewHBox(r.allChk, r.reChk, removeBtn)
		r.box = container.NewVBox(
			container.NewBorder(nil, nil, widget.NewLabel("Column:"), opts, r.colSel),
			r.values,
			widget.NewSeparator(),
		)
		rows = append(rows, r)
		rowsBox.Add(r.box)
		rowsBox.Refresh()
	}

	if len(a.conds) == 0 {
		addRow(nil)
	} else {
		for i := range a.conds {
			addRow(&a.conds[i])
		}
	}

	combine := widget.NewRadioGroup([]string{"Match all conditions (AND)", "Match any condition (OR)"}, nil)
	if a.condsAny {
		combine.SetSelected("Match any condition (OR)")
	} else {
		combine.SetSelected("Match all conditions (AND)")
	}
	combine.Horizontal = true

	addBtn := widget.NewButtonWithIcon("Add condition", theme.ContentAddIcon(), func() { addRow(nil) })

	header := container.NewVBox(
		widget.NewLabelWithStyle("Filter rows by column", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		combine,
		widget.NewSeparator(),
	)
	content := container.NewBorder(header, addBtn, nil, nil, container.NewVScroll(rowsBox))

	d := dialog.NewCustomConfirm("Filter", "Apply", "Cancel", content, func(ok bool) {
		if !ok {
			return
		}
		var conds []model.ColumnCond
		for _, r := range rows {
			if r.removed {
				continue
			}
			vals := splitLines(r.values.Text)
			if len(vals) == 0 {
				continue
			}
			conds = append(conds, model.ColumnCond{
				Column: refByTitle[r.colSel.Selected],
				Values: vals,
				All:    r.allChk.Checked,
				Regexp: r.reChk.Checked,
			})
		}
		a.conds = conds
		a.condsAny = combine.Selected == "Match any condition (OR)"
		a.applySearch()
	}, a.win)
	d.Resize(a.dialogSize(720, 640))
	d.Show()
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
