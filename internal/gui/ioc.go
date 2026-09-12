package gui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"tlx/internal/model"
)

// iocTag is applied to every row that matches an indicator; iocTagColor is its
// palette colour (purple, distinct from the bad/suspicious/good presets).
const (
	iocTag      = "ioc-hit"
	iocTagColor = "#8E24AA"
)

// iocPrefKey is the preferences key that holds a standalone timeline's IOC list,
// keyed by file path so each timeline keeps its own list.
func iocPrefKey(path string) string { return "ioc_list:" + path }

// iocListText returns the active IOC list: the case's list inside a case, or the
// standalone list for the open file otherwise.
func (a *App) iocListText() (string, error) {
	if a.cse != nil {
		return a.cse.IOCList()
	}
	return a.iocList, nil
}

// setIOCListText stores the IOC list where it belongs: on the case inside a
// case, or in the per-file preference for a standalone timeline.
func (a *App) setIOCListText(text string) error {
	if a.cse != nil {
		return a.cse.SetIOCList(text)
	}
	a.iocList = text
	if a.idx != nil {
		a.fyne.Preferences().SetString(iocPrefKey(a.idx.Path()), text)
	}
	return nil
}

// loadStandaloneIOCList restores the open file's saved IOC list from
// preferences. It is a no-op inside a case (the case keeps its own list).
func (a *App) loadStandaloneIOCList() {
	a.iocList = ""
	if a.cse == nil && a.idx != nil {
		a.iocList = a.fyne.Preferences().String(iocPrefKey(a.idx.Path()))
	}
}

// showIOCList opens the active IOC list for editing. Saving stores it (on the
// case in a case, or per-file for a standalone timeline) and runs it against the
// open timeline. In a case the list also runs automatically on each import.
func (a *App) showIOCList() {
	if a.idx == nil || a.masterMode {
		a.showError(fmt.Errorf("open a timeline first"))
		return
	}
	cur, err := a.iocListText()
	if err != nil {
		a.showError(err)
		return
	}
	entry := widget.NewMultiLineEntry()
	entry.SetText(cur)
	entry.SetMinRowsVisible(14)
	entry.Wrapping = fyne.TextWrapOff

	hint := widget.NewLabel(
		"One indicator per line. A plain string matches any column; wrap it in " +
			"/…/ for a regex. Start a line with a backtick to write a filter query, " +
			"e.g. `Summary=psexec AND Timestamp between 2024 and 2025. Blank lines " +
			"and lines starting with # are ignored. Matching is case-insensitive. " +
			"Rows that hit are tagged " + iocTag + ".")
	hint.Wrapping = fyne.TextWrapWord

	content := container.NewBorder(hint, nil, nil, nil, entry)
	d := dialog.NewCustomConfirm("IOC list", "Save & run", "Cancel", content, func(ok bool) {
		if !ok {
			return
		}
		if err := a.setIOCListText(entry.Text); err != nil {
			a.showError(err)
			return
		}
		if a.view != nil && !a.masterMode {
			a.runIOCs(false)
		} else {
			dialog.ShowInformation("IOC list", "Saved. Open a timeline to run it.", a.win)
		}
	}, a.win)
	d.Resize(a.dialogSize(720, 620))
	d.Show()
}

// runIOCs compiles the active IOC list, scans the open timeline over its full
// row set (ignoring any active filter) and tags every matching row iocTag.
// Tagging needs a writable session, so a read-only session is raised to
// Investigator for the tagging and restored afterwards — the user asked for
// these tags, so this is an explicit annotation.
//
// When auto is true the call is a background run for a freshly imported
// timeline: it stays silent on a clean result and persists the new tags to the
// case so they survive without a manual save.
func (a *App) runIOCs(auto bool) {
	if a.view == nil || a.idx == nil || a.masterMode {
		if !auto {
			a.showError(fmt.Errorf("open a timeline first"))
		}
		return
	}
	text, err := a.iocListText()
	if err != nil {
		if !auto {
			a.showError(err)
		}
		return
	}
	if strings.TrimSpace(text) == "" {
		if !auto {
			dialog.ShowInformation("IOCs", "The IOC list is empty. Add indicators from the IOC list dialog.", a.win)
		}
		return
	}

	// Compile and scan against a fresh full view so an active filter does not
	// hide matches.
	scanView := model.NewView(a.idx, a.sess)
	set := scanView.CompileIOCs(text, false)
	hits, err := scanView.ScanIOCs(set)
	if err != nil {
		if !auto {
			a.showError(err)
		}
		return
	}

	prev := a.sess.Mode()
	if prev == model.ReadOnly {
		a.sess.SetMode(model.Investigator)
	}
	a.sess.DefineTag(iocTag, iocTagColor)
	tagged := 0
	for _, m := range hits {
		if err := a.sess.AddTag(m, iocTag); err != nil {
			a.showError(err)
			break
		}
		tagged++
	}
	if prev == model.ReadOnly {
		a.sess.SetMode(prev)
	}

	// On import, persist straight to the case so the tags stick.
	if auto && tagged > 0 && a.cse != nil && a.curTimeline != nil {
		if err := a.cse.SaveAnnotations(*a.curTimeline, a.sess.Snapshot(), a.idx); err != nil {
			a.showError(err)
		} else {
			a.sess.MarkSaved()
		}
	}

	a.refreshTable()
	if a.selectedMaster() >= 0 {
		a.showDetail(a.selectedMaster())
	}

	if auto && tagged == 0 && len(set.Errors) == 0 {
		return // quiet background run, nothing to report
	}
	a.reportIOCs(set, tagged, auto)
}

// reportIOCs shows the outcome of an IOC run: how many indicators ran, how many
// rows were tagged, and any lines that failed to compile.
func (a *App) reportIOCs(set *model.IOCSet, tagged int, auto bool) {
	var b strings.Builder
	if auto && a.curTimeline != nil {
		fmt.Fprintf(&b, "Imported timeline %q.\n", a.curTimeline.Name)
	}
	fmt.Fprintf(&b, "%s ran; %s tagged %s.",
		plural(set.Count(), "indicator"), plural(tagged, "row"), iocTag)

	if len(set.Errors) == 0 {
		dialog.ShowInformation("IOCs", b.String(), a.win)
		return
	}

	b.WriteString("\n\nLines skipped:\n")
	for _, e := range set.Errors {
		fmt.Fprintf(&b, "  line %d: %s — %s\n", e.Line, e.Text, e.Err)
	}
	lbl := widget.NewLabel(b.String())
	lbl.Wrapping = fyne.TextWrapWord
	d := dialog.NewCustom("IOCs", "Close", container.NewVScroll(lbl), a.win)
	d.Resize(a.dialogSize(560, 480))
	d.Show()
}
