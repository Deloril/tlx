package gui

import (
	"encoding/json"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// Saved views are application-level, not tied to any case or file. They are
// stored in the Fyne preferences as a JSON blob so the same named filters are
// available whether viewing a single CSV, a timeline, or the master view.

const savedViewsKey = "saved_views"

// viewsSidebarOffset is the split position of the left saved-views pane when
// shown: a narrow column beside the main content.
const viewsSidebarOffset = 0.16

// condPreset mirrors a model.ColumnCond but references its column by title so a
// saved view stays portable across timelines with different column orders.
type condPreset struct {
	ColumnTitle string   `json:"column"`
	Values      []string `json:"values"`
	Regexp      bool     `json:"regexp"`
	Cased       bool     `json:"cased"`
	All         bool     `json:"all"`
}

type sortPreset struct {
	ColumnTitle string `json:"column"`
	Desc        bool   `json:"desc"`
}

type viewPreset struct {
	Name       string       `json:"name"`
	Query      string       `json:"query"` // raw search-box text (query language; see query.go)
	Cased      bool         `json:"cased"` // case-sensitive matching for the query
	TaggedOnly bool         `json:"tagged_only"`
	CondsAny   bool         `json:"conds_any"`
	Conds      []condPreset `json:"conds"`
	Sort       []sortPreset `json:"sort"`
	// ColFilters are the per-column header boxes, keyed by column title so they
	// carry across timelines with the same fields.
	ColFilters map[string]string `json:"col_filters,omitempty"`
	// TagFilter is the set of tag names ticked in the Tags drop-down.
	TagFilter []string `json:"tag_filter,omitempty"`
}

const anyColumnTitle = "Any column"

func (a *App) loadViews() []viewPreset {
	raw := a.fyne.Preferences().String(savedViewsKey)
	if raw == "" {
		return nil
	}
	var out []viewPreset
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (a *App) storeViews(views []viewPreset) {
	data, err := json.Marshal(views)
	if err != nil {
		a.showError(err)
		return
	}
	a.fyne.Preferences().SetString(savedViewsKey, string(data))
}

// colTitle maps a column reference to its display title for storage.
func (a *App) colTitle(ref model.ColumnRef) string {
	if ref == model.ColAll {
		return anyColumnTitle
	}
	for _, c := range a.cols {
		if c.ref == ref {
			return c.title
		}
	}
	return anyColumnTitle
}

// refForTitle resolves a stored column title against the current columns. An
// unknown title (e.g. a column absent from this timeline) falls back to ColAll,
// so the condition still applies as an any-column match.
func (a *App) refForTitle(title string) model.ColumnRef {
	if title == "" || title == anyColumnTitle {
		return model.ColAll
	}
	for _, c := range a.cols {
		if c.title == title {
			return c.ref
		}
	}
	return model.ColAll
}

// buildViewsSection is the body of the left sidebar's Saved views section: a
// save action over the list of saved views, filled by refreshViewsSection.
func (a *App) buildViewsSection() fyne.CanvasObject {
	saveBtn := widget.NewButtonWithIcon("Save current view…", theme.DocumentSaveIcon(), a.saveCurrentView)
	a.viewsList = container.NewVBox()
	a.refreshViewsSection()
	return container.NewVBox(saveBtn, widget.NewSeparator(), a.viewsList)
}

// refreshViewsSection repopulates the saved-views list from preferences. Each
// row applies its view on click and has a delete button.
func (a *App) refreshViewsSection() {
	if a.viewsList == nil {
		return
	}
	a.viewsList.Objects = nil
	views := a.loadViews()
	if len(views) == 0 {
		empty := widget.NewLabel("(no saved views)")
		empty.Wrapping = fyne.TextWrapWord
		a.viewsList.Add(empty)
		a.viewsList.Refresh()
		return
	}
	for i := range views {
		v := views[i]
		apply := widget.NewButton(v.Name, func() { a.applyView(v) })
		apply.Alignment = widget.ButtonAlignLeading
		del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() { a.deleteView(v.Name) })
		del.Importance = widget.DangerImportance
		a.viewsList.Add(container.NewBorder(nil, nil, nil, del, apply))
	}
	a.viewsList.Refresh()
}

// deleteView removes a saved view by name and refreshes the sidebar.
func (a *App) deleteView(name string) {
	views := a.loadViews()
	kept := views[:0]
	for _, v := range views {
		if v.Name != name {
			kept = append(kept, v)
		}
	}
	a.storeViews(kept)
	a.refreshViewsSection()
}

// captureView reads the current filter/sort into a preset with the given name.
func (a *App) captureView(name string) viewPreset {
	p := viewPreset{
		Name:       name,
		Query:      a.search.Text,
		Cased:      a.caseChk.Checked,
		TaggedOnly: a.taggedChk.Checked,
		CondsAny:   a.condsAny,
	}
	for _, c := range a.conds {
		p.Conds = append(p.Conds, condPreset{
			ColumnTitle: a.colTitle(c.Column),
			Values:      append([]string(nil), c.Values...),
			Regexp:      c.Regexp,
			Cased:       c.Cased,
			All:         c.All,
		})
	}
	for _, sk := range a.sortState {
		p.Sort = append(p.Sort, sortPreset{ColumnTitle: a.colTitle(sk.Col), Desc: sk.Desc})
	}
	p.TagFilter = a.selectedTagNames()
	for ref, val := range a.colFilter {
		if val == "" {
			continue
		}
		if p.ColFilters == nil {
			p.ColFilters = map[string]string{}
		}
		p.ColFilters[a.colTitle(ref)] = val
	}
	return p
}

func (a *App) saveCurrentView() {
	if a.view == nil {
		dialog.ShowInformation("Save view", "Open a timeline or file first.", a.win)
		return
	}
	entry := widget.NewEntry()
	entry.SetPlaceHolder("view name, e.g. lateral movement")
	dialog.ShowCustomConfirm("Save view", "Save", "Cancel", entry, func(ok bool) {
		if !ok {
			return
		}
		name := entry.Text
		if name == "" {
			return
		}
		views := a.loadViews()
		p := a.captureView(name)
		replaced := false
		for i := range views {
			if views[i].Name == name {
				views[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			views = append(views, p)
		}
		a.storeViews(views)
		a.refreshViewsSection()
		if !a.viewsSidebarVisible {
			a.setViewsSidebar(true) // reveal the pane so the saved view is visible
		}
	}, a.win)
}

// applyView loads a preset into the current view, mapping its stored column
// titles onto the current columns.
func (a *App) applyView(p viewPreset) {
	if a.view == nil {
		return
	}
	a.conds = nil
	for _, cp := range p.Conds {
		a.conds = append(a.conds, model.ColumnCond{
			Column: a.refForTitle(cp.ColumnTitle),
			Values: append([]string(nil), cp.Values...),
			Regexp: cp.Regexp,
			Cased:  cp.Cased,
			All:    cp.All,
		})
	}
	a.condsAny = p.CondsAny
	a.colFilter = map[model.ColumnRef]string{}
	for title, val := range p.ColFilters {
		if val == "" {
			continue
		}
		a.colFilter[a.refForTitle(title)] = val
	}
	a.tagFilter = map[string]bool{}
	for _, name := range p.TagFilter {
		a.tagFilter[name] = true
	}
	// Set the persistent widgets with callbacks suppressed, so the checkbox
	// OnChanged doesn't run commitFilter and overwrite the conditions we just
	// loaded from the view with the (stale) filter-window rows.
	a.suppressFilter = true
	a.search.SetText(p.Query)
	a.caseChk.SetChecked(p.Cased)
	a.taggedChk.SetChecked(p.TaggedOnly)
	a.suppressFilter = false
	a.applySearch() // applies query + conds + tagged-only together

	// Apply the saved ordering, if any.
	a.sortState = nil
	for _, sp := range p.Sort {
		a.sortState = append(a.sortState, model.SortKey{Col: a.refForTitle(sp.ColumnTitle), Desc: sp.Desc})
	}
	if len(a.sortState) > 0 {
		if err := a.view.Sort(a.sortState); err != nil {
			a.showError(err)
		}
	}
	a.refreshTable()
	a.rebuildFilterPanel() // mirror the applied query and conditions in the panel
}
