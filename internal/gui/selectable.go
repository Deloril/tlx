package gui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/casefile"
	"tlx/internal/model"
)

// selectableLabel shows read-only text you can select and copy. It is a
// widget.Entry with editing suppressed, so the caret and drag-selection work but
// the value can't be changed. Right-clicking offers to add the highlighted
// substring (or the whole field when nothing is highlighted) to the notes lists.
//
// It exists so a long field like Summary can be shown in the Details panel and
// have just a highlighted part of it captured as an artifact.
type selectableLabel struct {
	widget.Entry
	app *App
}

func newSelectableLabel(app *App, text string) *selectableLabel {
	s := &selectableLabel{app: app}
	s.MultiLine = true
	s.Wrapping = fyne.TextWrapWord
	s.ExtendBaseWidget(s)
	s.SetText(text)
	return s
}

// TypedRune drops typed characters so the text stays fixed.
func (s *selectableLabel) TypedRune(rune) {}

// TypedKey allows navigation and selection keys but blocks the ones that would
// mutate the text.
func (s *selectableLabel) TypedKey(k *fyne.KeyEvent) {
	switch k.Name {
	case fyne.KeyBackspace, fyne.KeyDelete, fyne.KeyReturn, fyne.KeyEnter, fyne.KeyTab:
		return
	}
	s.Entry.TypedKey(k)
}

// TypedShortcut keeps copy and select-all but drops paste and cut.
func (s *selectableLabel) TypedShortcut(sc fyne.Shortcut) {
	switch sc.(type) {
	case *fyne.ShortcutPaste, *fyne.ShortcutCut:
		return
	}
	s.Entry.TypedShortcut(sc)
}

// TappedSecondary replaces the entry's cut/copy/paste menu with note actions on
// the selected text (or the whole field), plus a plain copy.
func (s *selectableLabel) TappedSecondary(e *fyne.PointEvent) {
	sel := s.SelectedText()
	target := strings.TrimSpace(sel)
	if target == "" {
		target = strings.TrimSpace(s.Text)
	}

	var items []*fyne.MenuItem
	if s.app.notesAvailable() && target != "" {
		which := "field"
		if strings.TrimSpace(sel) != "" {
			which = "selection"
		}
		if _, ok := model.ParseTime(target); ok {
			items = append(items, fyne.NewMenuItem(fmt.Sprintf("Add %s to times", which), func() {
				s.app.addNoteAndReveal(casefile.NoteTime, target)
			}))
		}
		items = append(items, fyne.NewMenuItem(fmt.Sprintf("Add %s as artifact", which), func() {
			s.app.addNoteAndReveal(casefile.NoteArtifact, target)
		}))
	}
	if strings.TrimSpace(sel) != "" {
		items = append(items, fyne.NewMenuItem("Copy", func() {
			s.app.win.Clipboard().SetContent(sel)
		}))
	}
	if len(items) == 0 {
		return
	}
	widget.NewPopUpMenu(fyne.NewMenu("", items...), s.app.win.Canvas()).ShowAtPosition(e.AbsolutePosition)
}
