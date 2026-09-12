package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// The timeline-comments panel is a free-text field per timeline for running
// notes towards a write-up. It saves as you type: to the case database inside a
// case, or a per-file preference for a standalone timeline. Its content also
// heads an export (see model.ExportOmittingWithComment).

func timelineCommentKey(path string) string { return "timeline_comment:" + path }

// buildCommentsContent is the body of the right dock's timeline-comments panel.
func (a *App) buildCommentsContent() fyne.CanvasObject {
	a.commentEntry = widget.NewMultiLineEntry()
	a.commentEntry.Wrapping = fyne.TextWrapWord
	a.commentEntry.SetMinRowsVisible(10)
	a.loadTimelineComment()
	a.commentEntry.OnChanged = func(s string) {
		if a.suppressComment {
			return
		}
		a.saveTimelineComment(s)
	}
	if !a.notesAvailable() {
		a.commentEntry.Disable()
		return container.NewBorder(
			widget.NewLabel("Open a timeline to add comments."), nil, nil, nil, a.commentEntry)
	}
	hint := widget.NewLabel("Running notes for this timeline, saved as you type.")
	hint.Wrapping = fyne.TextWrapWord
	return container.NewBorder(hint, nil, nil, nil, a.commentEntry)
}

// loadTimelineComment fills the field from the current timeline's stored comment
// without triggering a save.
func (a *App) loadTimelineComment() {
	if a.commentEntry == nil {
		return
	}
	a.suppressComment = true
	defer func() { a.suppressComment = false }()
	if !a.notesAvailable() {
		a.commentEntry.SetText("")
		return
	}
	if a.cse != nil {
		txt, err := a.cse.TimelineComment(a.curTimeline.ID)
		if err != nil {
			a.showError(err)
			return
		}
		a.commentEntry.SetText(txt)
		return
	}
	a.commentEntry.SetText(a.fyne.Preferences().String(timelineCommentKey(a.idx.Path())))
}

func (a *App) saveTimelineComment(s string) {
	if !a.notesAvailable() {
		return
	}
	if a.cse != nil {
		if err := a.cse.SetTimelineComment(a.curTimeline.ID, s); err != nil {
			a.showError(err)
		}
		return
	}
	a.fyne.Preferences().SetString(timelineCommentKey(a.idx.Path()), s)
}

// currentTimelineComment is the open timeline's comment text, used to head an
// export. Empty when no single timeline is open (e.g. the master view).
func (a *App) currentTimelineComment() string {
	if !a.notesAvailable() || a.commentEntry == nil {
		return ""
	}
	return a.commentEntry.Text
}
