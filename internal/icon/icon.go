// Package icon draws the app icon (a white microphone on a rounded square)
// at any size and packs sizes into a Windows .ico file.
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"

	"SpeechToText/internal/paint"
)

// Draw renders the icon at size×size pixels. Recording switches the blue
// background to red, which the tray uses while the microphone is live.
func Draw(size int, recording bool) *image.RGBA {
	s := float64(size)
	c := paint.New(size, size)

	from, to := paint.Hex("#5b9bfa", 1), paint.Hex("#2563eb", 1)
	if recording {
		from, to = paint.Hex("#f87171", 1), paint.Hex("#dc2626", 1)
	}
	m := s * .03
	bg := paint.RoundRect(s/2, s/2, s/2-m, s/2-m, s*.25)
	c.FillFunc(0, 0, s, s, bg, func(x, y float64) paint.Color {
		return paint.Mix(from, to, float32((x+y)/(2*s)))
	})

	// Strokes stay at least ~1.3px so the glyph survives 16px tray icons.
	stroke := max(s*.062, 1.3)
	white := paint.Hex("#ffffff", 1)
	cx := s / 2

	body := paint.Capsule(cx, s*.285, cx, s*.47, max(s*.112, 2.2))
	c.Fill(0, 0, s, s, body, white)

	holder := paint.Intersect(paint.Ring(cx, s*.46, s*.19, stroke), paint.HalfPlaneBelow(s*.46))
	c.Fill(0, 0, s, s, holder, white)
	// Round caps on the holder's tips.
	c.Fill(0, 0, s, s, paint.Circle(cx-s*.19, s*.46, stroke/2), white)
	c.Fill(0, 0, s, s, paint.Circle(cx+s*.19, s*.46, stroke/2), white)

	c.Fill(0, 0, s, s, paint.Capsule(cx, s*.65, cx, s*.75, stroke/2), white)
	c.Fill(0, 0, s, s, paint.Capsule(cx-s*.1, s*.755, cx+s*.1, s*.755, stroke/2), white)
	return c.RGBA()
}

// ICO packs PNG-compressed images of the given sizes into an .ico file
// (supported by Windows since Vista).
func ICO(recording bool, sizes ...int) []byte {
	var imgs [][]byte
	for _, sz := range sizes {
		var b bytes.Buffer
		_ = png.Encode(&b, Draw(sz, recording))
		imgs = append(imgs, b.Bytes())
	}
	var out bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&out, le, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, sz := range sizes {
		dim := byte(sz)
		if sz >= 256 {
			dim = 0 // 0 means 256 in an ICONDIRENTRY
		}
		out.Write([]byte{dim, dim, 0, 0})
		_ = binary.Write(&out, le, [2]uint16{1, 32})
		_ = binary.Write(&out, le, [2]uint32{uint32(len(imgs[i])), uint32(offset)})
		offset += len(imgs[i])
	}
	for _, b := range imgs {
		out.Write(b)
	}
	return out.Bytes()
}
