package gui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// A timeline can carry several named IOC lists. Inside a case the lists live in
// the case database (every case has a default list named after the case);
// standalone, they live in a per-file preference. Each list tags its hits with
// its own tag, ioc:<name>, so the grid shows which list matched a row.

const iocTagColor = "#8E24AA" // purple, distinct from the bad/suspicious/good presets

// iocTagFor is the tag a list applies to its hits. Commas and semicolons are
// stripped from the name because tag cells split on them.
func iocTagFor(listName string) string {
	safe := strings.NewReplacer(",", " ", ";", " ").Replace(listName)
	return "ioc:" + strings.TrimSpace(safe)
}

// iocEntry identifies an IOC list. ID is the case-database id; for a standalone
// timeline it is 0 and the list is identified by Name.
type iocEntry struct {
	ID   int64
	Name string
}

// --- storage: case database or per-file preference ---

// iocPrefKey is the legacy single-list preference key (one list per file),
// migrated into the named-list store on first read.
func iocPrefKey(path string) string { return "ioc_list:" + path }

// iocListsPrefKey holds a standalone timeline's named IOC lists as JSON.
func iocListsPrefKey(path string) string { return "ioc_lists:" + path }

// standaloneIOC is one named list for a file outside a case.
type standaloneIOC struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// standaloneIOCLists reads the open file's named lists. The first time a file is
// seen it seeds one list named after the file, carrying any legacy single-list
// value across, and stores it so the seed is not repeated (an empty list set is
// stored as "[]", distinct from the unseeded absent key).
func (a *App) standaloneIOCLists() []standaloneIOC {
	if a.idx == nil {
		return nil
	}
	raw := a.fyne.Preferences().String(iocListsPrefKey(a.idx.Path()))
	if raw == "" { // never seeded for this file
		body := a.fyne.Preferences().String(iocPrefKey(a.idx.Path()))
		lists := []standaloneIOC{{Name: fileBaseName(a.idx.Path()), Body: body}}
		a.storeStandaloneIOCLists(lists)
		return lists
	}
	var lists []standaloneIOC
	json.Unmarshal([]byte(raw), &lists)
	return lists
}

func (a *App) storeStandaloneIOCLists(lists []standaloneIOC) {
	if a.idx == nil {
		return
	}
	if lists == nil {
		lists = []standaloneIOC{}
	}
	data, _ := json.Marshal(lists)
	a.fyne.Preferences().SetString(iocListsPrefKey(a.idx.Path()), string(data))
}

// fileBaseName is a file's base name without its extension.
func fileBaseName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// iocEntries lists the IOC lists for the current context (case or standalone).
func (a *App) iocEntries() []iocEntry {
	if a.cse != nil {
		metas, err := a.cse.IOCLists()
		if err != nil {
			a.showError(err)
			return nil
		}
		out := make([]iocEntry, len(metas))
		for i, m := range metas {
			out[i] = iocEntry{ID: m.ID, Name: m.Name}
		}
		return out
	}
	sl := a.standaloneIOCLists()
	out := make([]iocEntry, len(sl))
	for i, s := range sl {
		out[i] = iocEntry{Name: s.Name}
	}
	return out
}

func (a *App) iocBody(e iocEntry) (string, error) {
	if a.cse != nil {
		return a.cse.IOCListBody(e.ID)
	}
	for _, s := range a.standaloneIOCLists() {
		if s.Name == e.Name {
			return s.Body, nil
		}
	}
	return "", nil
}

func (a *App) setIOCBody(e iocEntry, body string) error {
	if a.cse != nil {
		return a.cse.SetIOCListBody(e.ID, body)
	}
	lists := a.standaloneIOCLists()
	for i := range lists {
		if lists[i].Name == e.Name {
			lists[i].Body = body
			a.storeStandaloneIOCLists(lists)
			return nil
		}
	}
	return nil
}

func (a *App) createIOCListNamed(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("list name is required")
	}
	if a.cse != nil {
		_, err := a.cse.CreateIOCList(name)
		return err
	}
	lists := a.standaloneIOCLists()
	for _, s := range lists {
		if strings.EqualFold(s.Name, name) {
			return fmt.Errorf("an IOC list named %q already exists", name)
		}
	}
	lists = append(lists, standaloneIOC{Name: name})
	a.storeStandaloneIOCLists(lists)
	return nil
}

func (a *App) renameIOCEntry(e iocEntry, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("list name is required")
	}
	if name == e.Name {
		return nil
	}
	if a.cse != nil {
		return a.cse.RenameIOCList(e.ID, name)
	}
	lists := a.standaloneIOCLists()
	for _, s := range lists {
		if s.Name != e.Name && strings.EqualFold(s.Name, name) {
			return fmt.Errorf("an IOC list named %q already exists", name)
		}
	}
	for i := range lists {
		if lists[i].Name == e.Name {
			lists[i].Name = name
			a.storeStandaloneIOCLists(lists)
			return nil
		}
	}
	return nil
}

func (a *App) deleteIOCEntry(e iocEntry) error {
	if a.cse != nil {
		return a.cse.DeleteIOCList(e.ID)
	}
	lists := a.standaloneIOCLists()
	kept := lists[:0]
	for _, s := range lists {
		if s.Name != e.Name {
			kept = append(kept, s)
		}
	}
	a.storeStandaloneIOCLists(kept)
	return nil
}

// appendToCaseIOCList adds one indicator line to the case's default IOC list
// (the list named after the case). Used by the investigator-notes buttons.
func (a *App) appendToCaseIOCList(line string) error {
	if a.cse == nil {
		return fmt.Errorf("open a case first")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	name := caseDisplayName(a.cse.Path())
	var target iocEntry
	found := false
	for _, e := range a.iocEntries() {
		if strings.EqualFold(e.Name, name) {
			target, found = e, true
			break
		}
	}
	if !found { // default list was deleted; recreate it
		if err := a.createIOCListNamed(name); err != nil {
			return err
		}
		for _, e := range a.iocEntries() {
			if strings.EqualFold(e.Name, name) {
				target, found = e, true
				break
			}
		}
	}
	if !found {
		return fmt.Errorf("could not resolve the case IOC list")
	}
	body, err := a.iocBody(target)
	if err != nil {
		return err
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"
	return a.setIOCBody(target, body)
}

// --- left-sidebar IOC section ---

// buildIOCSection is the body of the left sidebar's IOC lists section, filled by
// refreshIOCSection.
func (a *App) buildIOCSection() fyne.CanvasObject {
	a.iocListBox = container.NewVBox()
	a.refreshIOCSection()
	return a.iocListBox
}

// refreshIOCSection rebuilds the list of IOC lists. Each row opens its editor on
// the name, runs the list, or deletes it; a Run-all appears when there is more
// than one.
func (a *App) refreshIOCSection() {
	if a.iocListBox == nil {
		return
	}
	a.iocListBox.Objects = nil
	if a.cse == nil && a.idx == nil {
		a.iocListBox.Add(widget.NewLabel("(open a timeline or case)"))
		a.iocListBox.Refresh()
		return
	}
	a.iocListBox.Add(widget.NewButtonWithIcon("New list…", theme.ContentAddIcon(), a.newIOCList))

	entries := a.iocEntries()
	if len(entries) == 0 {
		a.iocListBox.Add(widget.NewLabel("(no IOC lists)"))
	}
	for i, e := range entries {
		e := e
		if i > 0 {
			a.iocListBox.Add(widget.NewSeparator())
		}
		name := widget.NewButton(e.Name, func() { a.editIOCList(e) })
		name.Alignment = widget.ButtonAlignLeading
		run := widget.NewButtonWithIcon("", theme.MediaPlayIcon(), func() { a.runOneIOCList(e) })
		del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() { a.confirmDeleteIOCList(e) })
		del.Importance = widget.DangerImportance
		a.iocListBox.Add(container.NewBorder(nil, nil, nil, container.NewHBox(run, del), name))
	}
	if len(entries) > 1 {
		a.iocListBox.Add(widget.NewButtonWithIcon("Run all on this timeline",
			theme.MediaFastForwardIcon(), func() { a.runAllIOCLists(false) }))
	}
	a.iocListBox.Refresh()
}

// revealIOCSection shows the left sidebar and expands the IOC section.
func (a *App) revealIOCSection() {
	if !a.viewsSidebarVisible {
		a.setViewsSidebar(true)
	}
	if a.iocPanel != nil {
		a.iocPanel.setExpanded(true)
	}
}

func (a *App) newIOCList() {
	if a.cse == nil && a.idx == nil {
		a.showError(fmt.Errorf("open a timeline or case first"))
		return
	}
	entry := widget.NewEntry()
	entry.SetPlaceHolder("list name, e.g. ransomware")
	dialog.ShowCustomConfirm("New IOC list", "Create", "Cancel", entry, func(ok bool) {
		if !ok || strings.TrimSpace(entry.Text) == "" {
			return
		}
		if err := a.createIOCListNamed(entry.Text); err != nil {
			a.showError(err)
			return
		}
		a.refreshIOCSection()
		for _, e := range a.iocEntries() {
			if strings.EqualFold(e.Name, strings.TrimSpace(entry.Text)) {
				a.editIOCList(e)
				break
			}
		}
	}, a.win)
}

// editIOCList opens a list for editing: its name and its indicators. Saving
// stores both and runs the list against the open timeline.
func (a *App) editIOCList(e iocEntry) {
	cur, err := a.iocBody(e)
	if err != nil {
		a.showError(err)
		return
	}
	nameEntry := widget.NewEntry()
	nameEntry.SetText(e.Name)
	body := widget.NewMultiLineEntry()
	body.SetText(cur)
	body.SetMinRowsVisible(14)
	body.Wrapping = fyne.TextWrapOff

	hint := widget.NewLabel(
		"One indicator per line. A plain string matches any column; wrap it in " +
			"/…/ for a regex. Start a line with a backtick to write a filter query, " +
			"e.g. `Summary=psexec AND Timestamp between 2024 and 2025. Blank lines " +
			"and lines starting with # are ignored. Matching is case-insensitive. " +
			"Hits are tagged " + iocTagFor(e.Name) + ".")
	hint.Wrapping = fyne.TextWrapWord

	top := container.NewVBox(container.NewBorder(nil, nil, widget.NewLabel("Name:"), nil, nameEntry), hint)
	content := container.NewBorder(top, nil, nil, nil, body)
	d := dialog.NewCustomConfirm("IOC list", "Save & run", "Cancel", content, func(ok bool) {
		if !ok {
			return
		}
		if err := a.renameIOCEntry(e, nameEntry.Text); err != nil {
			a.showError(err)
			return
		}
		e.Name = strings.TrimSpace(nameEntry.Text)
		if err := a.setIOCBody(e, body.Text); err != nil {
			a.showError(err)
			return
		}
		a.refreshIOCSection()
		if a.view != nil && !a.masterMode {
			a.runOneIOCList(e)
		} else {
			dialog.ShowInformation("IOC list", "Saved. Open a timeline to run it.", a.win)
		}
	}, a.win)
	d.Resize(a.dialogSize(720, 620))
	d.Show()
}

func (a *App) confirmDeleteIOCList(e iocEntry) {
	dialog.ShowConfirm("Delete IOC list",
		fmt.Sprintf("Delete the IOC list %q? Its indicators are removed; rows already tagged keep their tags.", e.Name),
		func(ok bool) {
			if !ok {
				return
			}
			if err := a.deleteIOCEntry(e); err != nil {
				a.showError(err)
				return
			}
			a.refreshIOCSection()
		}, a.win)
}

// --- running ---

// runIOCList scans the open timeline against one list and tags every hit with
// that list's tag. It returns how many rows were tagged and the compiled set
// (nil when the list is empty). A read-only session is briefly raised to
// Investigator for the tagging, since the user asked for these tags.
func (a *App) runIOCList(e iocEntry) (tagged int, set *model.IOCSet, err error) {
	if a.view == nil || a.idx == nil || a.masterMode {
		return 0, nil, fmt.Errorf("open a timeline first")
	}
	text, err := a.iocBody(e)
	if err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(text) == "" {
		return 0, nil, nil
	}
	scanView := model.NewView(a.idx, a.sess) // scan the full row set, ignoring any filter
	set = scanView.CompileIOCs(text, false)
	hits, err := scanView.ScanIOCs(set)
	if err != nil {
		return 0, set, err
	}
	tag := iocTagFor(e.Name)
	prev := a.sess.Mode()
	if prev == model.ReadOnly {
		a.sess.SetMode(model.Investigator)
	}
	defer func() {
		if prev == model.ReadOnly {
			a.sess.SetMode(prev)
		}
	}()
	a.sess.DefineTag(tag, iocTagColor)
	for _, m := range hits {
		if err := a.sess.AddTag(m, tag); err != nil {
			return tagged, set, err
		}
		tagged++
	}
	return tagged, set, nil
}

// runOneIOCList runs a single list and reports the outcome.
func (a *App) runOneIOCList(e iocEntry) {
	tagged, set, err := a.runIOCList(e)
	if err != nil {
		a.showError(err)
		return
	}
	a.afterIOCRun()
	if set == nil {
		dialog.ShowInformation("IOCs", fmt.Sprintf("The list %q is empty.", e.Name), a.win)
		return
	}
	a.reportIOCRun(e.Name, set, tagged)
}

// runAllIOCLists runs every list against the open timeline. A manual run shows a
// combined summary; an import (auto) stays quiet unless something was tagged or
// a line failed, and persists the new tags to the case.
func (a *App) runAllIOCLists(auto bool) {
	if a.view == nil || a.idx == nil || a.masterMode {
		if !auto {
			a.showError(fmt.Errorf("open a timeline first"))
		}
		return
	}
	entries := a.iocEntries()
	ran, total := 0, 0
	var errs []model.IOCError
	for _, e := range entries {
		tagged, set, err := a.runIOCList(e)
		if err != nil {
			a.showError(err)
			continue
		}
		if set == nil {
			continue // empty list
		}
		ran++
		total += tagged
		errs = append(errs, set.Errors...)
	}
	if auto && total > 0 && a.cse != nil && a.curTimeline != nil {
		if err := a.cse.SaveAnnotations(*a.curTimeline, a.sess.Snapshot(), a.idx); err != nil {
			a.showError(err)
		} else {
			a.sess.MarkSaved()
		}
	}
	a.afterIOCRun()
	if auto && total == 0 && len(errs) == 0 {
		return // quiet background run, nothing to report
	}
	a.reportAllIOCRuns(auto, ran, total, errs)
}

func (a *App) afterIOCRun() {
	a.refreshTable()
	if a.selectedMaster() >= 0 {
		a.showDetail(a.selectedMaster())
	}
}

// reportIOCRun shows the outcome of running one list.
func (a *App) reportIOCRun(name string, set *model.IOCSet, tagged int) {
	var b strings.Builder
	fmt.Fprintf(&b, "List %q: %s ran; %s tagged %s.",
		name, plural(set.Count(), "indicator"), plural(tagged, "row"), iocTagFor(name))
	a.showIOCReport(b.String(), set.Errors)
}

// reportAllIOCRuns shows the combined outcome of running every list.
func (a *App) reportAllIOCRuns(auto bool, ran, total int, errs []model.IOCError) {
	var b strings.Builder
	if auto && a.curTimeline != nil {
		fmt.Fprintf(&b, "Imported timeline %q.\n", a.curTimeline.Name)
	}
	fmt.Fprintf(&b, "%s ran; %s tagged.", plural(ran, "IOC list"), plural(total, "row"))
	a.showIOCReport(b.String(), errs)
}

// showIOCReport shows an IOC run summary, listing any lines that failed to
// compile.
func (a *App) showIOCReport(summary string, errs []model.IOCError) {
	if len(errs) == 0 {
		dialog.ShowInformation("IOCs", summary, a.win)
		return
	}
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\nLines skipped:\n")
	for _, e := range errs {
		fmt.Fprintf(&b, "  line %d: %s — %s\n", e.Line, e.Text, e.Err)
	}
	lbl := widget.NewLabel(b.String())
	lbl.Wrapping = fyne.TextWrapWord
	d := dialog.NewCustom("IOCs", "Close", container.NewVScroll(lbl), a.win)
	d.Resize(a.dialogSize(560, 480))
	d.Show()
}
