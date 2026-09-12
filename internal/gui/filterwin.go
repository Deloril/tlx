package gui

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// The filter window is a separate, non-modal OS window that can be dragged
// outside the main window. It splits into structured per-column conditions on
// top and the freetext query with its checkboxes below. Applying updates the
// main grid and leaves the window open, so an examiner can refine a filter
// against what they just found without reopening it.

func (a *App) toggleFilterWindow() {
	if a.idx == nil {
		return
	}
	if a.filterWinShown {
		a.hideFilterWindow()
		return
	}
	a.showFilterWindow()
}

func (a *App) hideFilterWindow() {
	if a.filterWin != nil {
		a.filterWin.Hide()
	}
	a.filterWinShown = false
	// Drop the row widgets so commitFilter can't read stale rows while hidden;
	// they are rebuilt on the next show.
	a.filterRows = nil
}

// focusFilterWindow shows the window (building it if needed) and puts the caret
// in the query box, so / and Ctrl+F drop straight into typing a filter. When the
// window is already open it just refocuses, so any unapplied edits survive.
func (a *App) focusFilterWindow() {
	if a.idx == nil {
		return
	}
	if a.filterWinShown {
		a.filterWin.RequestFocus()
	} else {
		a.showFilterWindow()
	}
	if a.filterWin != nil && a.search != nil {
		a.filterWin.Canvas().Focus(a.search)
	}
}

func (a *App) showFilterWindow() {
	if a.filterWin == nil {
		a.filterWin = a.fyne.NewWindow("Filter — Timeline explorer")
		a.filterWin.Resize(fyne.NewSize(560, 600))
		// Closing hides and reuses the window rather than destroying it, so its
		// size and position survive the next open.
		a.filterWin.SetCloseIntercept(a.hideFilterWindow)
	}
	a.filterWin.SetContent(a.buildFilterContent())
	a.filterWin.Show()
	a.filterWinShown = true
}

// buildFilterContent lays out the two halves. It is rebuilt on each show so the
// column choices track the current file; the query box and checkboxes are
// persistent widgets (see buildFilterWidgets) reparented into the new content.
func (a *App) buildFilterContent() fyne.CanvasObject {
	a.filterConds = container.NewVBox()
	a.filterRows = nil
	if len(a.conds) == 0 {
		a.addFilterRow(nil)
	} else {
		for i := range a.conds {
			a.addFilterRow(&a.conds[i])
		}
	}

	a.combineSel = widget.NewRadioGroup(
		[]string{"Match all conditions (AND)", "Match any condition (OR)"}, nil)
	if a.condsAny {
		a.combineSel.SetSelected("Match any condition (OR)")
	} else {
		a.combineSel.SetSelected("Match all conditions (AND)")
	}
	a.combineSel.Horizontal = true

	addBtn := widget.NewButtonWithIcon("Add condition", theme.ContentAddIcon(), func() { a.addFilterRow(nil) })
	topHeader := container.NewVBox(
		widget.NewLabelWithStyle("Column conditions", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		a.combineSel,
		widget.NewSeparator(),
	)
	top := container.NewBorder(topHeader, addBtn, nil, nil, container.NewVScroll(a.filterConds))

	applyBtn := widget.NewButtonWithIcon("Apply", theme.ConfirmIcon(), a.commitFilter)
	applyBtn.Importance = widget.HighImportance
	clearBtn := widget.NewButtonWithIcon("Clear all", theme.ContentClearIcon(), a.clearFilter)
	bottom := container.NewVBox(
		widget.NewLabelWithStyle("Query", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		a.search,
		container.NewHBox(a.caseChk, a.taggedChk),
		container.NewHBox(applyBtn, clearBtn),
	)

	split := container.NewVSplit(top, bottom)
	split.Offset = 0.6
	return split
}

// addFilterRow appends one structured condition row to the conditions box,
// prefilled from cond when given.
func (a *App) addFilterRow(cond *model.ColumnCond) {
	titles, _, titleByRef := a.filterColumnChoices()
	r := &filterRow{
		colSel: widget.NewSelect(titles, nil),
		values: widget.NewMultiLineEntry(),
		allChk: widget.NewCheck("all values", nil),
		reChk:  widget.NewCheck("regex", nil),
	}
	r.values.SetPlaceHolder("one value per line")
	r.values.SetMinRowsVisible(2)
	r.colSel.SetSelected(anyColumnTitle)
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
		a.filterConds.Remove(r.box)
		a.filterConds.Refresh()
	})
	opts := container.NewHBox(r.allChk, r.reChk, removeBtn)
	r.box = container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Column:"), opts, r.colSel),
		r.values,
		widget.NewSeparator(),
	)
	a.filterRows = append(a.filterRows, r)
	a.filterConds.Add(r.box)
	a.filterConds.Refresh()
}

// filterColumnChoices returns the column titles for the row selectors and the
// two-way maps between a title and its column reference.
func (a *App) filterColumnChoices() ([]string, map[string]model.ColumnRef, map[model.ColumnRef]string) {
	titles := []string{anyColumnTitle}
	refByTitle := map[string]model.ColumnRef{anyColumnTitle: model.ColAll}
	titleByRef := map[model.ColumnRef]string{model.ColAll: anyColumnTitle}
	for i := range a.cols {
		t := a.cols[i].title
		titles = append(titles, t)
		refByTitle[t] = a.cols[i].ref
		titleByRef[a.cols[i].ref] = t
	}
	return titles, refByTitle, titleByRef
}

// commitFilter reads the structured rows (if the window has been built) into the
// active conditions, then applies the whole filter to the grid.
func (a *App) commitFilter() {
	if a.suppressFilter {
		return
	}
	if a.filterRows != nil {
		_, refByTitle, _ := a.filterColumnChoices()
		var conds []model.ColumnCond
		for _, r := range a.filterRows {
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
		if a.combineSel != nil {
			a.condsAny = a.combineSel.Selected == "Match any condition (OR)"
		}
	}
	a.applySearch()
}
