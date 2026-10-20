package main

import (
	"image"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"SpeechToText/internal/paint"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// The dictation indicator: a small dark pill near the bottom of the screen,
// drawn here into pixels and shown by overlay_windows.go without ever taking
// the focus from the app being typed into.

type overlayKind int

const (
	ovListening overlayKind = iota
	ovWorking
	ovDone
	ovError
	ovInfo
)

type overlayFrame struct {
	Kind  overlayKind
	Text  string
	Dim   bool      // muted text colour (a placeholder rather than content)
	Hint  string    // right-aligned muted text
	Clock time.Time // when set, Hint shows the time elapsed since then
	Level float64   // 0..1 microphone level while listening
	T     float64   // seconds since shown; drives the animation
}

// Geometry in CSS pixels.
const (
	ovPad     = 18.0 // transparent margin around the pill for its shadow
	ovHeight  = 46.0
	ovMinW    = 180.0
	ovMaxW    = 540.0
	ovMaxMsgW = 720.0
)

var (
	colBg     = paint.Hex("#16161a", .97)
	colBorder = paint.Hex("#2e2e35", 1)
	colText   = paint.Hex("#f4f4f5", 1)
	colMuted  = paint.Hex("#9d9da8", 1)
	colAccent = paint.Hex("#7fb0fa", 1)
	colRed    = paint.Hex("#ef4444", 1)
	colGreen  = paint.Hex("#22c55e", 1)
	colGrey   = paint.Hex("#3f3f48", 1)
	colWhite  = paint.Hex("#ffffff", 1)
)

type fontSet struct{ text, hint font.Face }

var (
	fontsMu             sync.Mutex
	fontCache           = map[float64]*fontSet{}
	sfntText, sfntPlain *opentype.Font
)

func loadSystemFont(fallback []byte, files ...string) *opentype.Font {
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	for _, f := range files {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if fnt, err := opentype.Parse(b); err == nil {
				return fnt
			}
		}
	}
	fnt, _ := opentype.Parse(fallback)
	return fnt
}

// fontsFor returns Segoe UI faces for a DPI scale (Go fonts if missing).
func fontsFor(scale float64) *fontSet {
	fontsMu.Lock()
	defer fontsMu.Unlock()
	if fs := fontCache[scale]; fs != nil {
		return fs
	}
	if sfntText == nil {
		sfntText = loadSystemFont(gomedium.TTF, "seguisb.ttf", "segoeui.ttf")
		sfntPlain = loadSystemFont(goregular.TTF, "segoeui.ttf")
	}
	face := func(f *opentype.Font, size float64) font.Face {
		fc, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: size * scale, DPI: 72, Hinting: font.HintingFull})
		return fc
	}
	fs := &fontSet{text: face(sfntText, 13.5), hint: face(sfntPlain, 13)}
	fontCache[scale] = fs
	return fs
}

func textWidth(face font.Face, s string) float64 {
	return float64(font.MeasureString(face, s)) / 64
}

// fitRight shortens s with a trailing ellipsis to fit maxW pixels.
func fitRight(face font.Face, s string, maxW float64) string {
	if textWidth(face, s) <= maxW {
		return s
	}
	r := []rune(s)
	for n := len(r) - 1; n > 0; n-- {
		if t := string(r[:n]) + "…"; textWidth(face, t) <= maxW {
			return t
		}
	}
	return "…"
}

// fitLeft keeps the end of s (the newest words of a live transcript).
func fitLeft(face font.Face, s string, maxW float64) string {
	if textWidth(face, s) <= maxW {
		return s
	}
	r := []rune(s)
	best := "…"
	for i := len(r) - 1; i >= 0; i-- {
		t := "…" + string(r[i:])
		if textWidth(face, t) > maxW {
			break
		}
		best = t
	}
	return best
}

func iconSpace(k overlayKind) float64 {
	if k == ovListening {
		return 16 + 14 + 10 + 27 + 12 // pad, dot, gap, level bars, gap
	}
	return 16 + 22 + 11
}

// overlayWidth is the pill width (CSS px) that fits the frame's content.
func overlayWidth(f overlayFrame, scale float64) float64 {
	fs := fontsFor(scale)
	w := iconSpace(f.Kind) + textWidth(fs.text, f.Text)/scale + 18
	if f.Hint != "" {
		w += 14 + textWidth(fs.hint, f.Hint)/scale
	}
	limit := ovMaxW
	if f.Kind == ovError || f.Kind == ovInfo {
		limit = ovMaxMsgW // messages are read once, not watched: allow a wider pill
	}
	return math.Min(limit, math.Max(ovMinW, math.Ceil(w)))
}

// renderOverlay draws the pill with the given width (CSS px) at a DPI scale.
func renderOverlay(f overlayFrame, pillW, scale float64) *paint.Canvas {
	s := scale
	W := int(math.Ceil((pillW + 2*ovPad) * s))
	H := int(math.Ceil((ovHeight + 2*ovPad) * s))
	c := paint.New(W, H)
	fw, fh := float64(W), float64(H)

	x0, y0 := ovPad*s, ovPad*s
	pw, ph := pillW*s, ovHeight*s
	cx, cy := x0+pw/2, y0+ph/2
	r := ph / 2

	c.Shadow(0, 0, fw, fh, paint.RoundRect(cx, cy+3*s, pw/2, ph/2, r), paint.Hex("#000000", .38), 7*s)
	c.Fill(x0-1, y0-1, x0+pw+1, y0+ph+1, paint.RoundRect(cx, cy, pw/2, ph/2, r), colBorder)
	bw := math.Max(1, s)
	c.Fill(x0, y0, x0+pw, y0+ph, paint.RoundRect(cx, cy, pw/2-bw, ph/2-bw, r-bw), colBg)

	ix := x0 + 16*s + 11*s // icon centre
	box := func(rad float64) (float64, float64, float64, float64) {
		return ix - rad - 2, cy - rad - 2, ix + rad + 2, cy + rad + 2
	}

	switch f.Kind {
	case ovListening:
		ix = x0 + 16*s + 7*s
		p := math.Mod(f.T, 1.4) / 1.4
		pr := (5 + 8*p) * s
		bx0, by0, bx1, by1 := box(pr)
		c.Fill(bx0, by0, bx1, by1, paint.Circle(ix, cy, pr), colRed.WithAlpha(float32(.35*(1-p))))
		bx0, by0, bx1, by1 = box(5 * s)
		c.Fill(bx0, by0, bx1, by1, paint.Circle(ix, cy, 5*s), colRed)

		lvl := math.Min(1, math.Max(0, f.Level))
		bx := x0 + (16+14+10)*s
		for i := range 5 {
			wob := .55 + .45*math.Sin(f.T*8.5+float64(i)*1.9)
			h := (4 + 18*lvl*wob) * s
			x := bx + float64(i)*6*s + 1.5*s
			c.Fill(x-3*s, cy-h/2-2, x+3*s, cy+h/2+2, paint.Capsule(x, cy-h/2+1.5*s, x, cy+h/2-1.5*s, 1.5*s), colAccent)
		}

	case ovWorking:
		rad := 8 * s
		head := math.Mod(f.T*2*math.Pi*1.15, 2*math.Pi)
		bx0, by0, bx1, by1 := box(rad + 2*s)
		c.FillFunc(bx0, by0, bx1, by1, paint.Ring(ix, cy, rad, 2.3*s), func(x, y float64) paint.Color {
			a := math.Atan2(y-cy, x-ix)
			d := math.Mod(head-a+4*math.Pi, 2*math.Pi) / (2 * math.Pi) // 0 at the head, growing along the tail
			return colAccent.WithAlpha(float32(math.Max(.1, 1-d*1.15)))
		})

	case ovDone, ovError, ovInfo:
		fill := map[overlayKind]paint.Color{ovDone: colGreen, ovError: colRed, ovInfo: colGrey}[f.Kind]
		bx0, by0, bx1, by1 := box(10 * s)
		c.Fill(bx0, by0, bx1, by1, paint.Circle(ix, cy, 9.5*s), fill)
		w := 1.15 * s
		switch f.Kind {
		case ovDone:
			c.Fill(bx0, by0, bx1, by1, paint.Capsule(ix-4*s, cy+.3*s, ix-1.2*s, cy+3.1*s, w), colWhite)
			c.Fill(bx0, by0, bx1, by1, paint.Capsule(ix-1.2*s, cy+3.1*s, ix+4.2*s, cy-3*s, w), colWhite)
		case ovError:
			c.Fill(bx0, by0, bx1, by1, paint.Capsule(ix, cy-4.4*s, ix, cy+.9*s, w), colWhite)
			c.Fill(bx0, by0, bx1, by1, paint.Circle(ix, cy+4.1*s, 1.3*s), colWhite)
		case ovInfo:
			c.Fill(bx0, by0, bx1, by1, paint.Capsule(ix-3.2*s, cy-3.2*s, ix+3.2*s, cy+3.2*s, w), colWhite)
			c.Fill(bx0, by0, bx1, by1, paint.Capsule(ix-3.2*s, cy+3.2*s, ix+3.2*s, cy-3.2*s, w), colWhite)
		}
	}

	fs := fontsFor(scale)
	tx := x0 + iconSpace(f.Kind)*s
	right := x0 + pw - 18*s
	if f.Hint != "" {
		hw := textWidth(fs.hint, f.Hint)
		drawText(c, fs.hint, f.Hint, right-hw, cy, colMuted)
		right -= hw + 14*s
	}
	text := fitRight(fs.text, f.Text, right-tx)
	if f.Kind == ovListening {
		text = fitLeft(fs.text, f.Text, right-tx)
	}
	col := colText
	if f.Dim {
		col = colMuted
	}
	drawText(c, fs.text, text, tx, cy, col)
	return c
}

// drawText draws s with its left edge at x, vertically centred on cy.
func drawText(c *paint.Canvas, face font.Face, s string, x, cy float64, col paint.Color) {
	if s == "" {
		return
	}
	m := face.Metrics()
	baseline := cy + float64(m.Ascent-m.Descent)/64/2
	mask := image.NewAlpha(image.Rect(0, 0, c.W, c.H))
	d := font.Drawer{Dst: mask, Src: image.Opaque, Face: face, Dot: fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.I(int(math.Round(baseline)))}}
	d.DrawString(s)
	c.DrawMask(mask, 0, 0, col)
}
