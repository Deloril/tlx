package gui

import (
	"encoding/base64"
	"errors"
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
	app    *App
	master int // row this text belongs to, for actions that write back to it
}

func newSelectableLabel(app *App, master int, text string) *selectableLabel {
	s := &selectableLabel{app: app, master: master}
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
	// Base64-decode the target into this row's comment. Only offered when the
	// comment is writable (a single timeline open, not the read-only master view).
	if s.app.notesAvailable() && target != "" {
		which := "field"
		if strings.TrimSpace(sel) != "" {
			which = "selection"
		}
		items = append(items, fyne.NewMenuItem(fmt.Sprintf("Base64 decode %s into comment", which), func() {
			s.app.base64ToComment(s.master, target)
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
	// Show the menu on the canvas this label actually lives in — a pop-out window
	// has its own, so using the main window's canvas would draw the menu back in
	// the main view, sometimes hidden behind the pop-out.
	cv := fyne.CurrentApp().Driver().CanvasForObject(s)
	if cv == nil {
		cv = s.app.win.Canvas()
	}
	widget.NewPopUpMenu(fyne.NewMenu("", items...), cv).ShowAtPosition(e.AbsolutePosition)
}

// base64ToComment decodes s and appends the result to the row's comment, tagged
// with a "Base64 decodes to:" prefix so the origin is clear. A decode failure
// surfaces as an error dialog rather than writing garbage.
func (a *App) base64ToComment(master int, s string) {
	decoded, err := decodeBase64(s)
	if err != nil {
		a.showError(fmt.Errorf("base64 decode: %w", err))
		return
	}
	add := "Base64 decodes to: " + decoded
	existing := a.sess.Comment(master)
	if existing != "" {
		existing += " " + add
	} else {
		existing = add
	}
	if err := a.sess.SetComment(master, existing); err != nil {
		a.showError(err)
		return
	}
	if a.sidebarVisible && a.selectedMaster() == master {
		a.showDetail(master)
	}
	a.refreshTable()
}

// decodeBase64 decodes text that may be standard or URL-safe base64, with or
// without padding, and tolerates whitespace and line wrapping. It returns an
// error only when every variant rejects the input.
func decodeBase64(s string) (string, error) {
	s = strings.Join(strings.Fields(s), "") // drop whitespace and line breaks
	if s == "" {
		return "", errors.New("nothing to decode")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", errors.New("not valid base64")
}
