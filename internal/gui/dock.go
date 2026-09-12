package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// dockPanel is a titled section whose body collapses and expands by tapping its
// header. A floatable panel also carries a button that pops the body out into
// its own window; that window has a "Dock to right" button, and closing it
// re-docks, so the panel is never lost. The body object is persistent and moves
// between the dock card and the float window rather than being rebuilt, so any
// widget state (a half-typed query, a selected row's detail) survives the move.
//
// The left sidebar uses non-floatable panels for its Views/Case/IOC sections;
// the right dock uses floatable ones for Filter and Details.
type dockPanel struct {
	app       *App
	title     string
	floatable bool

	body     fyne.CanvasObject // persistent content, reparented on dock/float
	bodyWrap *fyne.Container   // holds body while docked; empty while floating
	toggle   *widget.Button    // header: disclosure arrow + title, taps to collapse
	floatBtn *widget.Button    // header: pop out (floatable only)
	header   *fyne.Container
	root     *fyne.Container // the whole card: header over bodyWrap

	expanded bool
	floating bool
	win      fyne.Window

	// onChange fires after any dock/float/collapse change so the owner can
	// re-lay-out its container (e.g. rebuild the right dock's card list).
	onChange func()
}

func newDockPanel(a *App, title string, floatable bool) *dockPanel {
	p := &dockPanel{app: a, title: title, floatable: floatable, expanded: true}
	p.bodyWrap = container.NewStack()
	p.toggle = widget.NewButton("", p.toggleExpanded)
	p.toggle.Alignment = widget.ButtonAlignLeading
	p.toggle.Importance = widget.LowImportance
	if floatable {
		p.floatBtn = widget.NewButtonWithIcon("", theme.ViewFullScreenIcon(), p.float)
		p.floatBtn.Importance = widget.LowImportance
		p.header = container.NewBorder(nil, nil, nil, p.floatBtn, p.toggle)
	} else {
		p.header = container.NewBorder(nil, nil, nil, nil, p.toggle)
	}
	p.root = container.NewVBox(p.header, p.bodyWrap)
	p.updateToggle()
	return p
}

// setBody installs (or replaces) the panel content, placing it wherever the
// panel currently lives.
func (p *dockPanel) setBody(o fyne.CanvasObject) {
	p.body = o
	if p.floating {
		if p.win != nil {
			p.win.SetContent(p.floatContent())
		}
		return
	}
	p.bodyWrap.Objects = []fyne.CanvasObject{o}
	if p.expanded {
		p.bodyWrap.Show()
	} else {
		p.bodyWrap.Hide()
	}
	p.bodyWrap.Refresh()
}

func (p *dockPanel) updateToggle() {
	arrow := "▾ " // ▾ open
	if !p.expanded {
		arrow = "▸ " // ▸ closed
	}
	p.toggle.SetText(arrow + p.title)
}

func (p *dockPanel) toggleExpanded() { p.setExpanded(!p.expanded) }

func (p *dockPanel) setExpanded(exp bool) {
	p.expanded = exp
	if !p.floating {
		if exp {
			p.bodyWrap.Show()
		} else {
			p.bodyWrap.Hide()
		}
		p.bodyWrap.Refresh()
	}
	p.updateToggle()
	if p.onChange != nil {
		p.onChange()
	}
}

// float moves the body out into its own window. The dock card drops out of the
// stack (see onChange); the window's Dock button and its close handler both
// re-dock.
func (p *dockPanel) float() {
	if p.floating || !p.floatable {
		return
	}
	p.floating = true
	p.bodyWrap.Objects = nil
	p.bodyWrap.Refresh()
	p.win = p.app.fyne.NewWindow(p.title + " — Timeline explorer")
	p.win.SetContent(p.floatContent())
	p.win.Resize(fyne.NewSize(520, 560))
	p.win.SetCloseIntercept(p.dock) // closing re-docks rather than losing the panel
	p.win.Show()
	if p.onChange != nil {
		p.onChange()
	}
}

// floatContent wraps the body with a title bar and a Dock button for the
// free-floating window.
func (p *dockPanel) floatContent() fyne.CanvasObject {
	dockBtn := widget.NewButtonWithIcon("Dock to right", theme.NavigateBackIcon(), p.dock)
	bar := container.NewBorder(nil, nil,
		widget.NewLabelWithStyle(p.title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		dockBtn, nil)
	return container.NewBorder(bar, nil, nil, nil, container.NewVScroll(p.body))
}

// dock returns a floating body to the dock card and closes its window.
func (p *dockPanel) dock() {
	if !p.floating {
		return
	}
	p.floating = false
	if p.win != nil {
		// Release the body from the window before closing so it isn't parented
		// to a canvas that is about to be destroyed.
		p.win.SetContent(container.NewWithoutLayout())
		p.win.Close()
		p.win = nil
	}
	p.expanded = true
	p.bodyWrap.Objects = []fyne.CanvasObject{p.body}
	p.bodyWrap.Show()
	p.bodyWrap.Refresh()
	p.updateToggle()
	if p.onChange != nil {
		p.onChange()
	}
}

// focus brings the panel forward for typing: docked, it expands; floating, it
// raises its window.
func (p *dockPanel) focus() {
	if p.floating {
		if p.win != nil {
			p.win.RequestFocus()
		}
		return
	}
	if !p.expanded {
		p.setExpanded(true)
	}
}

// canvas returns the canvas the body currently lives on, for anchoring pop-ups.
func (p *dockPanel) canvas() fyne.Canvas {
	if p.floating && p.win != nil {
		return p.win.Canvas()
	}
	return p.app.win.Canvas()
}
