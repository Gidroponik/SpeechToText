// Package paint is a tiny anti-aliased rasteriser. Shapes are described by
// signed distance functions (negative inside, positive outside, in pixels),
// which is plenty for a pill-sized overlay and app icons and needs no C code.
package paint

import (
	"image"
	"math"
	"strconv"
)

// Color is a straight (non-premultiplied) colour with components in 0..1.
type Color struct{ R, G, B, A float32 }

// Hex parses "#rrggbb" with the given alpha.
func Hex(s string, a float32) Color {
	v, _ := strconv.ParseUint(s[1:], 16, 32)
	return Color{float32(v>>16&0xff) / 255, float32(v>>8&0xff) / 255, float32(v&0xff) / 255, a}
}

// WithAlpha returns c with its alpha multiplied by a.
func (c Color) WithAlpha(a float32) Color {
	c.A *= a
	return c
}

// Mix linearly interpolates between a and b.
func Mix(a, b Color, t float32) Color {
	return Color{a.R + (b.R-a.R)*t, a.G + (b.G-a.G)*t, a.B + (b.B-a.B)*t, a.A + (b.A-a.A)*t}
}

// SDF is a signed distance function in canvas pixels.
type SDF func(x, y float64) float64

// Canvas holds premultiplied RGBA pixels as floats.
type Canvas struct {
	W, H int
	Pix  []float32
}

func New(w, h int) *Canvas {
	return &Canvas{W: w, H: h, Pix: make([]float32, w*h*4)}
}

// blend composites a straight colour with the given coverage over (x, y).
func (c *Canvas) blend(x, y int, col Color, cov float32) {
	a := col.A * cov
	if a <= 0 {
		return
	}
	i := (y*c.W + x) * 4
	k := 1 - a
	c.Pix[i] = col.R*a + c.Pix[i]*k
	c.Pix[i+1] = col.G*a + c.Pix[i+1]*k
	c.Pix[i+2] = col.B*a + c.Pix[i+2]*k
	c.Pix[i+3] = a + c.Pix[i+3]*k
}

// bounds clips a float box to whole pixels inside the canvas.
func (c *Canvas) bounds(x0, y0, x1, y1 float64) (int, int, int, int) {
	return max(0, int(math.Floor(x0))), max(0, int(math.Floor(y0))),
		min(c.W, int(math.Ceil(x1))), min(c.H, int(math.Ceil(y1)))
}

// Fill paints the inside of sdf, sampled at pixel centres within the box.
func (c *Canvas) Fill(x0, y0, x1, y1 float64, sdf SDF, col Color) {
	c.FillFunc(x0, y0, x1, y1, sdf, func(float64, float64) Color { return col })
}

// FillFunc is Fill with a per-pixel colour, e.g. for gradients.
func (c *Canvas) FillFunc(x0, y0, x1, y1 float64, sdf SDF, col func(x, y float64) Color) {
	ix0, iy0, ix1, iy1 := c.bounds(x0, y0, x1, y1)
	for y := iy0; y < iy1; y++ {
		py := float64(y) + .5
		for x := ix0; x < ix1; x++ {
			px := float64(x) + .5
			cov := clamp01(.5 - sdf(px, py))
			if cov > 0 {
				c.blend(x, y, col(px, py), float32(cov))
			}
		}
	}
}

// Shadow paints a soft shadow of sdf: full alpha inside, Gaussian falloff
// with the given sigma outside.
func (c *Canvas) Shadow(x0, y0, x1, y1 float64, sdf SDF, col Color, sigma float64) {
	ix0, iy0, ix1, iy1 := c.bounds(x0, y0, x1, y1)
	for y := iy0; y < iy1; y++ {
		py := float64(y) + .5
		for x := ix0; x < ix1; x++ {
			d := max(0, sdf(float64(x)+.5, py))
			if d > sigma*3.5 {
				continue
			}
			c.blend(x, y, col, float32(math.Exp(-d*d/(2*sigma*sigma))))
		}
	}
}

// DrawMask paints col through an alpha mask (e.g. rasterised text) whose
// top-left corner lands at (ox, oy).
func (c *Canvas) DrawMask(m *image.Alpha, ox, oy int, col Color) {
	b := m.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		cy := oy + y - b.Min.Y
		if cy < 0 || cy >= c.H {
			continue
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			cx := ox + x - b.Min.X
			if cx < 0 || cx >= c.W {
				continue
			}
			if a := m.AlphaAt(x, y).A; a > 0 {
				c.blend(cx, cy, col, float32(a)/255)
			}
		}
	}
}

// BGRA writes premultiplied BGRA bytes (the layout of a 32-bit Windows DIB),
// multiplying every pixel by opacity.
func (c *Canvas) BGRA(dst []byte, opacity float32) {
	for i := 0; i < c.W*c.H; i++ {
		p := c.Pix[i*4 : i*4+4]
		dst[i*4] = byte8(p[2] * opacity)
		dst[i*4+1] = byte8(p[1] * opacity)
		dst[i*4+2] = byte8(p[0] * opacity)
		dst[i*4+3] = byte8(p[3] * opacity)
	}
}

// RGBA converts to a Go image (image.RGBA is premultiplied as well).
func (c *Canvas) RGBA() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, c.W, c.H))
	for i := 0; i < c.W*c.H; i++ {
		for k := 0; k < 4; k++ {
			img.Pix[i*4+k] = byte8(c.Pix[i*4+k])
		}
	}
	return img
}

func byte8(v float32) byte {
	return byte(min(255, max(0, v*255+.5)))
}

func clamp01(v float64) float64 { return min(1, max(0, v)) }

// ---------- shapes ----------

// RoundRect is a rectangle centred at (cx, cy) with half-size (hw, hh) and corner radius r.
func RoundRect(cx, cy, hw, hh, r float64) SDF {
	return func(x, y float64) float64 {
		qx := math.Abs(x-cx) - hw + r
		qy := math.Abs(y-cy) - hh + r
		return math.Hypot(max(qx, 0), max(qy, 0)) + min(max(qx, qy), 0) - r
	}
}

func Circle(cx, cy, r float64) SDF {
	return func(x, y float64) float64 { return math.Hypot(x-cx, y-cy) - r }
}

// Capsule is a segment from a to b with round ends of radius r (a thick line).
func Capsule(ax, ay, bx, by, r float64) SDF {
	return func(x, y float64) float64 {
		px, py := x-ax, y-ay
		dx, dy := bx-ax, by-ay
		h := clamp01((px*dx + py*dy) / (dx*dx + dy*dy))
		return math.Hypot(px-dx*h, py-dy*h) - r
	}
}

// Ring is a circle outline of radius r and the given thickness.
func Ring(cx, cy, r, thick float64) SDF {
	return func(x, y float64) float64 { return math.Abs(math.Hypot(x-cx, y-cy)-r) - thick/2 }
}

// Intersect keeps the area inside both shapes.
func Intersect(a, b SDF) SDF {
	return func(x, y float64) float64 { return max(a(x, y), b(x, y)) }
}

// Union keeps the area inside either shape.
func Union(a, b SDF) SDF {
	return func(x, y float64) float64 { return min(a(x, y), b(x, y)) }
}

// HalfPlaneBelow is the area with y > y0.
func HalfPlaneBelow(y0 float64) SDF {
	return func(_, y float64) float64 { return y0 - y }
}
