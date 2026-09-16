package gui

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
)

// A plain Enter fires OnSubmitted without inserting a newline; Ctrl+Enter inserts
// a newline without submitting. This is the reverse of Fyne's Entry default.
func TestSubmitEntryEnterVsCtrlEnter(t *testing.T) {
	test.NewApp()
	defer test.NewApp() // reset for other tests

	e := newSubmitEntry()
	var got string
	fired := 0
	e.OnSubmitted = func(s string) { got = s; fired++ }

	// Render it so the entry has a text provider for newline insertion.
	w := test.NewWindow(e)
	defer w.Close()
	w.Resize(fyne.NewSize(300, 120))
	w.Canvas().Focus(e)
	e.SetText("hello")

	// Plain Enter: submit, no newline.
	e.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn})
	if fired != 1 || got != "hello" {
		t.Fatalf("plain Enter: fired=%d got=%q, want 1 %q", fired, got, "hello")
	}
	if strings.Contains(e.Text, "\n") {
		t.Fatalf("plain Enter must not insert a newline, text=%q", e.Text)
	}

	// Ctrl+Enter: newline, no submit. Fyne's driver delivers this combo as a
	// CustomShortcut, not through TypedKey, so drive that path.
	e.TypedShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyReturn, Modifier: fyne.KeyModifierControl})
	if fired != 1 {
		t.Fatalf("Ctrl+Enter must not submit, fired=%d", fired)
	}
	if !strings.Contains(e.Text, "\n") {
		t.Fatalf("Ctrl+Enter must insert a newline, text=%q", e.Text)
	}

	// A plain Enter still submits.
	e.TypedKey(&fyne.KeyEvent{Name: fyne.KeyEnter})
	if fired != 2 {
		t.Fatalf("plain Enter must submit, fired=%d", fired)
	}
}

// A box with no OnSubmitted keeps Enter as a plain newline, so free-text fields
// (comments panel, per-line value lists) are unaffected.
func TestSubmitEntryNewlineWithoutHandler(t *testing.T) {
	test.NewApp()
	defer test.NewApp()

	e := newSubmitEntry()
	w := test.NewWindow(e)
	defer w.Close()
	w.Resize(fyne.NewSize(300, 120))
	w.Canvas().Focus(e)
	e.SetText("a")

	e.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn})
	if !strings.Contains(e.Text, "\n") {
		t.Fatalf("Enter with no OnSubmitted must insert a newline, text=%q", e.Text)
	}
}
