package gui

import (
	"image/color"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// rowTint is the alpha applied to a tag colour when painting a row background,
// so the highlight is a wash rather than a solid block. 0x40 is 25% more
// transparent than the original 0x55.
const rowTint = 0x40

// palettePresets are the colours offered when defining a new tag.
var palettePresets = []string{
	"#E53935", "#FDD835", "#43A047", "#1E88E5",
	"#8E24AA", "#00897B", "#FB8C00", "#6D4C41", "#78909C",
}

// parseHex turns "#RRGGBB" (or "#RGB") into an opaque colour. An unparseable
// string falls back to a neutral grey rather than erroring.
func parseHex(s string) color.NRGBA {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if len(s) == 3 { // shorthand #RGB
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return color.NRGBA{R: 0x78, G: 0x90, B: 0xA0, A: 0xFF}
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.NRGBA{R: 0x78, G: 0x90, B: 0xA0, A: 0xFF}
	}
	return color.NRGBA{R: byte(v >> 16), G: byte(v >> 8), B: byte(v), A: 0xFF}
}

// rowTintColor is the row-background wash for a tag colour.
func rowTintColor(hex string) color.NRGBA {
	c := parseHex(hex)
	c.A = rowTint
	return c
}

// readableText returns black or white, whichever contrasts with bg. Uses the
// standard relative-luminance threshold.
func readableText(bg color.NRGBA) color.Color {
	lum := 0.299*float64(bg.R) + 0.587*float64(bg.G) + 0.114*float64(bg.B)
	if lum > 150 {
		return color.Black
	}
	return color.White
}

// nrgba converts any colour to straight-alpha NRGBA.
func nrgba(c color.Color) color.NRGBA {
	if n, ok := c.(color.NRGBA); ok {
		return n
	}
	r, g, b, a := c.RGBA()
	return color.NRGBA{R: byte(r >> 8), G: byte(g >> 8), B: byte(b >> 8), A: byte(a >> 8)}
}

// selectionTint is the wash painted over a selected row. Derived from the theme
// selection colour so it reads in both light and dark modes.
func selectionTint() color.NRGBA {
	c := nrgba(theme.Color(theme.ColorNameSelection))
	c.A = 0x88
	return c
}

// over alpha-composites top over base (straight alpha) and returns the result.
func over(base, top color.NRGBA) color.NRGBA {
	ta := float64(top.A) / 255
	ba := float64(base.A) / 255
	oa := ta + ba*(1-ta)
	if oa == 0 {
		return color.NRGBA{}
	}
	mix := func(tc, bc byte) byte {
		return byte((float64(tc)*ta + float64(bc)*ba*(1-ta)) / oa)
	}
	return color.NRGBA{
		R: mix(top.R, base.R),
		G: mix(top.G, base.G),
		B: mix(top.B, base.B),
		A: byte(oa * 255),
	}
}

// colorEq reports whether two colours are identical in RGBA.
func colorEq(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// colorSquare is a small fixed-size solid colour block for use beside a label.
func colorSquare(hex string) *canvas.Rectangle {
	sq := canvas.NewRectangle(parseHex(hex))
	sq.SetMinSize(fyne.NewSize(14, 14))
	sq.CornerRadius = 3
	return sq
}

// newTagChip is a colour square next to the tag name; the whole name button
// calls onTap.
func newTagChip(name, hex string, onTap func()) fyne.CanvasObject {
	btn := widget.NewButton(name, onTap)
	btn.Alignment = widget.ButtonAlignLeading
	return container.NewHBox(colorSquare(hex), btn)
}

// swatch is a small tappable coloured rectangle. It shows a border when
// selected and calls onTap when clicked.
type swatch struct {
	widget.BaseWidget
	fill     color.Color
	selected bool
	onTap    func()
	rect     *canvas.Rectangle
	border   *canvas.Rectangle
}

func newSwatch(fill color.Color, onTap func()) *swatch {
	s := &swatch{fill: fill, onTap: onTap}
	s.ExtendBaseWidget(s)
	return s
}

func (s *swatch) setSelected(v bool) {
	s.selected = v
	s.Refresh()
}

func (s *swatch) CreateRenderer() fyne.WidgetRenderer {
	s.border = canvas.NewRectangle(color.Transparent)
	s.border.StrokeWidth = 2
	s.border.CornerRadius = 4
	s.rect = canvas.NewRectangle(s.fill)
	s.rect.CornerRadius = 3
	return &swatchRenderer{s: s, objects: []fyne.CanvasObject{s.border, s.rect}}
}

func (s *swatch) Tapped(_ *fyne.PointEvent) {
	if s.onTap != nil {
		s.onTap()
	}
}

func (s *swatch) MinSize() fyne.Size { return fyne.NewSize(28, 22) }

type swatchRenderer struct {
	s       *swatch
	objects []fyne.CanvasObject
}

func (r *swatchRenderer) Layout(size fyne.Size) {
	r.s.border.Resize(size)
	r.s.border.Move(fyne.NewPos(0, 0))
	inset := float32(0)
	if r.s.selected {
		inset = 3
	}
	r.s.rect.Resize(fyne.NewSize(size.Width-2*inset, size.Height-2*inset))
	r.s.rect.Move(fyne.NewPos(inset, inset))
}

func (r *swatchRenderer) MinSize() fyne.Size { return r.s.MinSize() }

func (r *swatchRenderer) Refresh() {
	r.s.rect.FillColor = r.s.fill
	if r.s.selected {
		r.s.border.StrokeColor = theme.Color(theme.ColorNameForeground)
	} else {
		r.s.border.StrokeColor = color.Transparent
	}
	canvas.Refresh(r.s.border)
	canvas.Refresh(r.s.rect)
	r.Layout(r.s.Size())
}

func (r *swatchRenderer) Objects() []fyne.CanvasObject { return r.objects }
func (r *swatchRenderer) Destroy()                     {}
