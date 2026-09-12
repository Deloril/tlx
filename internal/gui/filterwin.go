package gui

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// The filter UI is a panel in the right dock (see dock.go): structured
// per-column conditions on top, the freetext query and its checkboxes below.
// Applying updates the grid without dismissing the panel, so an examiner can
// refine a filter against what they just found. The panel can float out into
// its own window and dock back, like every right-hand panel.

// revealFilterPanel brings the filter into view and focuses the query box, so
// the toolbar button, / and Ctrl+F all drop straight into typing a filter.
func (a *App) revealFilterPanel() {
	if a.idx == nil || a.filterPanel == nil {
		return
	}
	if !a.sidebarVisible && !a.filterPanel.floating {
		a.setSidebar(true)
	}
	a.filterPanel.focus() // expands if docked, raises the window if floating
	if a.search != nil {
		a.filterPanel.canvas().Focus(a.search)
	}
}

// rebuildFilterPanel regenerates the filter content so its column choices track
// the current file and its condition rows match the active conditions. The
// query box and checkboxes are persistent widgets reparented into the new
// content (see buildFilterWidgets).
func (a *App) rebuildFilterPanel() {
	if a.filterPanel == nil {
		return
	}
	a.filterPanel.setBody(a.buildFilterContent())
}

// buildFilterContent lays out the filter panel as a single scrollable column:
// the column conditions, then the query and its controls. A VBox (rather than a
// split) suits both the narrow docked pane and the float window.
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
	applyBtn := widget.NewButtonWithIcon("Apply", theme.ConfirmIcon(), a.commitFilter)
	applyBtn.Importance = widget.HighImportance
	clearBtn := widget.NewButtonWithIcon("Clear all", theme.ContentClearIcon(), a.clearFilter)

	return container.NewVBox(
		widget.NewLabelWithStyle("Column conditions", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		a.combineSel,
		a.filterConds,
		addBtn,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Query", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		a.search,
		container.NewHBox(a.caseChk, a.taggedChk, a.tagFilterButton()),
		container.NewHBox(applyBtn, clearBtn),
	)
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
