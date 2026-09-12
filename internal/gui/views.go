package gui

import (
	"encoding/json"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"timeline-engine/internal/model"
)

// Saved views are application-level, not tied to any incident or file. They are
// stored in the Fyne preferences as a JSON blob so the same named filters are
// available whether viewing a single CSV, a timeline, or the master view.

const savedViewsKey = "saved_views"

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
	Query      string       `json:"query"` // raw search-box text (a leading / means regex)
	TaggedOnly bool         `json:"tagged_only"`
	CondsAny   bool         `json:"conds_any"`
	Conds      []condPreset `json:"conds"`
	Sort       []sortPreset `json:"sort"`
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

func (a *App) viewsMenuItems() []*fyne.MenuItem {
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("Save current view…", a.saveCurrentView),
		fyne.NewMenuItem("Manage saved views…", a.manageViews),
		fyne.NewMenuItemSeparator(),
	}
	views := a.loadViews()
	if len(views) == 0 {
		empty := fyne.NewMenuItem("(no saved views)", nil)
		empty.Disabled = true
		items = append(items, empty)
		return items
	}
	for _, v := range views {
		v := v
		items = append(items, fyne.NewMenuItem(v.Name, func() { a.applyView(v) }))
	}
	return items
}

// captureView reads the current filter/sort into a preset with the given name.
func (a *App) captureView(name string) viewPreset {
	p := viewPreset{
		Name:       name,
		Query:      a.search.Text,
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
		a.rebuildIncidentMenu()
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
	a.search.SetText(p.Query)
	a.taggedChk.SetChecked(p.TaggedOnly)
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
}

func (a *App) manageViews() {
	views := a.loadViews()
	if len(views) == 0 {
		dialog.ShowInformation("Saved views", "No saved views yet.", a.win)
		return
	}
	list := container.NewVBox()
	var rebuild func()
	rebuild = func() {
		list.Objects = nil
		for i := range views {
			i := i
			del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
				views = append(views[:i], views[i+1:]...)
				a.storeViews(views)
				a.rebuildIncidentMenu()
				rebuild()
			})
			del.Importance = widget.DangerImportance
			apply := widget.NewButton("Apply", func() { a.applyView(views[i]) })
			row := container.NewBorder(nil, nil, nil, container.NewHBox(apply, del),
				widget.NewLabel(views[i].Name))
			list.Add(row)
		}
		list.Refresh()
	}
	rebuild()
	d := dialog.NewCustom("Saved views", "Close", container.NewVScroll(list), a.win)
	d.Resize(a.dialogSize(520, 520))
	d.Show()
}
