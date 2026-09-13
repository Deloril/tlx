package gui

import (
	"encoding/json"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/casefile"
	"tlx/internal/model"
)

// The investigator's-notes panel keeps two per-timeline lists: Artifacts (free
// strings) and Times (timestamps picked off the grid). Right-clicking a cell in
// the main view adds its value to the matching list. Each entry has a done
// checkbox that strikes the text through, a button to filter the current view by
// it, a button to copy it into the case IOC list, and a delete button.
//
// In a case the notes live in the case database (per timeline, not shared); a
// standalone timeline keeps them in a per-file preference.

// noteItem is one note as the UI handles it. ID identifies it for edits: the
// case-database id, or a per-file counter for a standalone timeline.
type noteItem struct {
	ID   int64
	Kind string
	Text string
	Done bool
}

// storedNote is a standalone timeline's note as persisted in preferences.
type storedNote struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func notesPrefKey(path string) string { return "notes:" + path }

// notesAvailable reports whether a single timeline is open to hold notes: not
// the master view, and either a standalone file or a timeline within a case.
func (a *App) notesAvailable() bool {
	if a.view == nil || a.masterMode {
		return false
	}
	if a.cse != nil {
		return a.curTimeline != nil
	}
	return a.idx != nil
}

func (a *App) standaloneNotes() []storedNote {
	if a.idx == nil {
		return nil
	}
	raw := a.fyne.Preferences().String(notesPrefKey(a.idx.Path()))
	if raw == "" {
		return nil
	}
	var out []storedNote
	json.Unmarshal([]byte(raw), &out)
	return out
}

func (a *App) storeStandaloneNotes(notes []storedNote) {
	if a.idx == nil {
		return
	}
	if notes == nil {
		notes = []storedNote{}
	}
	data, _ := json.Marshal(notes)
	a.fyne.Preferences().SetString(notesPrefKey(a.idx.Path()), string(data))
}

// notes returns the current timeline's notes of a kind, in insertion order.
func (a *App) notes(kind string) []noteItem {
	if a.cse != nil {
		if a.curTimeline == nil {
			return nil
		}
		ns, err := a.cse.Notes(a.curTimeline.ID, kind)
		if err != nil {
			a.showError(err)
			return nil
		}
		out := make([]noteItem, len(ns))
		for i, n := range ns {
			out[i] = noteItem{ID: n.ID, Kind: n.Kind, Text: n.Text, Done: n.Done}
		}
		return out
	}
	var out []noteItem
	for _, n := range a.standaloneNotes() {
		if n.Kind == kind {
			out = append(out, noteItem{ID: n.ID, Kind: n.Kind, Text: n.Text, Done: n.Done})
		}
	}
	return out
}

func (a *App) addNote(kind, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if a.cse != nil {
		if a.curTimeline == nil {
			return fmt.Errorf("open a timeline first")
		}
		_, err := a.cse.AddNote(a.curTimeline.ID, kind, text)
		return err
	}
	notes := a.standaloneNotes()
	var maxID int64
	for _, n := range notes {
		if n.ID > maxID {
			maxID = n.ID
		}
	}
	notes = append(notes, storedNote{ID: maxID + 1, Kind: kind, Text: text})
	a.storeStandaloneNotes(notes)
	return nil
}

func (a *App) setNoteDone(n noteItem, done bool) error {
	if a.cse != nil {
		return a.cse.SetNoteDone(n.ID, done)
	}
	notes := a.standaloneNotes()
	for i := range notes {
		if notes[i].ID == n.ID {
			notes[i].Done = done
			a.storeStandaloneNotes(notes)
			return nil
		}
	}
	return nil
}

func (a *App) deleteNote(n noteItem) error {
	if a.cse != nil {
		return a.cse.DeleteNote(n.ID)
	}
	notes := a.standaloneNotes()
	kept := notes[:0]
	for _, s := range notes {
		if s.ID != n.ID {
			kept = append(kept, s)
		}
	}
	a.storeStandaloneNotes(kept)
	return nil
}

// --- right-dock notes section ---

// buildNotesContent is the body of the right dock's investigator's-notes panel:
// an Artifacts list over a Times list, filled by refreshNotesSection.
func (a *App) buildNotesContent() fyne.CanvasObject {
	a.notesArtifactsBox = container.NewVBox()
	a.notesTimesBox = container.NewVBox()
	a.noteAddArtifact = a.noteAddEntry(casefile.NoteArtifact, "add artifact…")
	a.noteAddTime = a.noteAddEntry(casefile.NoteTime, "add time…")
	a.refreshNotesSection()
	head := func(s string) fyne.CanvasObject {
		return widget.NewLabelWithStyle(s, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	}
	return container.NewVBox(
		head("Artifacts"), a.notesArtifactsBox, a.noteAddRow(a.noteAddArtifact),
		widget.NewSeparator(),
		head("Times"), a.notesTimesBox, a.noteAddRow(a.noteAddTime),
	)
}

// noteAddEntry builds the text box that types a note straight into a list.
func (a *App) noteAddEntry(kind, placeholder string) *widget.Entry {
	e := widget.NewEntry()
	e.SetPlaceHolder(placeholder)
	e.OnSubmitted = func(string) { a.commitManualNote(kind, e) }
	return e
}

// noteAddRow wraps a manual-entry box with its add button.
func (a *App) noteAddRow(e *widget.Entry) fyne.CanvasObject {
	var kind string
	if e == a.noteAddTime {
		kind = casefile.NoteTime
	} else {
		kind = casefile.NoteArtifact
	}
	add := widget.NewButtonWithIcon("", theme.ContentAddIcon(), func() { a.commitManualNote(kind, e) })
	add.Importance = widget.LowImportance
	return container.NewBorder(nil, nil, nil, add, e)
}

// commitManualNote adds the typed text to the list and clears the box.
func (a *App) commitManualNote(kind string, e *widget.Entry) {
	if strings.TrimSpace(e.Text) == "" {
		return
	}
	if err := a.addNote(kind, e.Text); err != nil {
		a.showError(err)
		return
	}
	e.SetText("")
	a.refreshNotesSection()
}

func (a *App) refreshNotesSection() {
	if a.notesArtifactsBox == nil || a.notesTimesBox == nil {
		return
	}
	a.notesArtifactsBox.Objects = nil
	a.notesTimesBox.Objects = nil
	avail := a.notesAvailable()
	a.setManualNoteEnabled(avail)
	if !avail {
		hint := widget.NewLabel("Open a timeline to keep notes. Right-click a cell to add one.")
		hint.Wrapping = fyne.TextWrapWord
		a.notesArtifactsBox.Add(hint)
		a.notesArtifactsBox.Refresh()
		a.notesTimesBox.Refresh()
		return
	}
	fill := func(box *fyne.Container, kind string) {
		items := a.notes(kind)
		if len(items) == 0 {
			box.Add(widget.NewLabel("(none)"))
		}
		for i, n := range items {
			if i > 0 {
				box.Add(widget.NewSeparator())
			}
			box.Add(a.noteRow(n))
		}
		box.Refresh()
	}
	fill(a.notesArtifactsBox, casefile.NoteArtifact)
	fill(a.notesTimesBox, casefile.NoteTime)
}

// setManualNoteEnabled enables or disables the two manual-entry boxes, so they
// only accept input when a timeline is open to hold the notes.
func (a *App) setManualNoteEnabled(on bool) {
	for _, e := range []*widget.Entry{a.noteAddArtifact, a.noteAddTime} {
		if e == nil {
			continue
		}
		if on {
			e.Enable()
		} else {
			e.SetText("")
			e.Disable()
		}
	}
}

// noteRow renders one note: done checkbox (strikes the text through), the text,
// then filter / add-to-IOC / delete buttons.
func (a *App) noteRow(n noteItem) fyne.CanvasObject {
	label := widget.NewLabel(n.Text)
	label.Wrapping = fyne.TextWrapWord
	if n.Done {
		label.SetText(strikeThrough(n.Text))
	}

	chk := widget.NewCheck("", func(done bool) {
		if err := a.setNoteDone(n, done); err != nil {
			a.showError(err)
			return
		}
		if done {
			label.SetText(strikeThrough(n.Text))
		} else {
			label.SetText(n.Text)
		}
	})
	chk.SetChecked(n.Done)

	filterBtn := widget.NewButtonWithIcon("", theme.SearchIcon(), func() { a.filterByString(n.Text) })
	filterBtn.Importance = widget.LowImportance
	del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
		if err := a.deleteNote(n); err != nil {
			a.showError(err)
			return
		}
		a.refreshNotesSection()
	})
	del.Importance = widget.LowImportance

	right := container.NewHBox(filterBtn)
	if a.cse != nil { // only a case has a shared IOC list to add to
		iocBtn := widget.NewButtonWithIcon("", theme.ContentAddIcon(), func() {
			if err := a.appendToCaseIOCList(n.Text); err != nil {
				a.showError(err)
				return
			}
			a.refreshIOCSection()
			dialog.ShowInformation("IOC list",
				fmt.Sprintf("Added to the case IOC list %q.", caseDisplayName(a.cse.Path())), a.win)
		})
		iocBtn.Importance = widget.LowImportance
		right.Add(iocBtn)
	}
	right.Add(del)
	return container.NewBorder(nil, nil, chk, right, label)
}

// filterByString filters the current view to rows containing s anywhere, by
// setting the search box to a quoted term and applying it.
func (a *App) filterByString(s string) {
	if a.view == nil {
		return
	}
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	a.suppressFilter = true
	a.search.SetText(quoted)
	a.suppressFilter = false
	a.applySearch()
	a.rebuildFilterPanel()
}

// noteMenuItems are the "add to notes" context-menu items for the right-clicked
// cell: a time entry when the cell parses as a timestamp, an artifact entry
// otherwise. Nil when no timeline is open or the cell is empty/virtual.
func (a *App) noteMenuItems() []*fyne.MenuItem {
	if !a.notesAvailable() {
		return nil
	}
	row, col := a.hoverRow, a.hoverCol
	if row < 0 || row >= a.view.Len() || col < 0 || col >= len(a.visible) {
		return nil
	}
	ref := a.cols[a.visible[col]].ref
	if ref < 0 { // virtual columns (#, Tags, Comment)
		return nil
	}
	val := strings.TrimSpace(a.valueOf(a.view.Master(row), ref))
	if val == "" {
		return nil
	}
	if _, ok := model.ParseTime(val); ok {
		return []*fyne.MenuItem{fyne.NewMenuItem("Add time to notes", func() {
			a.addNoteAndReveal(casefile.NoteTime, val)
		})}
	}
	return []*fyne.MenuItem{fyne.NewMenuItem(fmt.Sprintf("Add %q as artifact", ellipsize(val, 40)), func() {
		a.addNoteAndReveal(casefile.NoteArtifact, val)
	})}
}

func (a *App) addNoteAndReveal(kind, text string) {
	if err := a.addNote(kind, text); err != nil {
		a.showError(err)
		return
	}
	a.refreshNotesSection()
	a.revealNotesPanel()
}

// revealNotesPanel shows the right dock, brings the notes panel forward and
// collapses the other docked panels (see revealPanel).
func (a *App) revealNotesPanel() {
	a.revealPanel(a.notesPanel)
}

// strikeThrough overlays a combining long stroke on each rune so a "done" note
// reads as struck out. Fyne has no strikethrough text style, so this is the
// portable way to get the effect.
func strikeThrough(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
		b.WriteRune('̶') // combining long stroke overlay
	}
	return b.String()
}

// ellipsize trims s to at most n runes, appending an ellipsis when it was cut.
func ellipsize(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
