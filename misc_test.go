package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"SpeechToText/internal/icon"
)

func TestSecretRoundTrip(t *testing.T) {
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	sealed, err := sealString(key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "abcdef") {
		t.Fatal("sealed value contains the plain key")
	}
	plain, err := openString(sealed)
	if err != nil || plain != key {
		t.Fatalf("open = %q, %v", plain, err)
	}
	// A tampered blob must not decrypt.
	tampered := []byte(sealed)
	tampered[len(tampered)/2] ^= 1
	if p, err := openString(string(tampered)); err == nil && p == key {
		t.Fatal("tampered blob decrypted")
	}
	if s, _ := sealString(""); s != "" {
		t.Fatal("empty secret should stay empty")
	}
}

func TestKeyHintAndProxyMask(t *testing.T) {
	if h := keyHint("sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"); h != "sk-proj-…6789" {
		t.Errorf("hint = %q", h)
	}
	if m := maskProxy("socks5://user:secret@10.0.0.1:1080"); strings.Contains(m, "secret") || !strings.Contains(m, "user:••••@10.0.0.1:1080") {
		t.Errorf("mask = %q", m)
	}
	if m := maskProxy("http://10.0.0.1:3128"); m != "http://10.0.0.1:3128" {
		t.Errorf("mask = %q", m)
	}
}

func TestSegmenter(t *testing.T) {
	chunk := chunkBytes // 100 ms
	var s segmenter
	feed := func(rms float64, ms int) (commits int) {
		for range ms / chunkMs {
			if s.push(rms, chunk) {
				commits++
			}
		}
		return
	}
	// Background noise, then speech shorter than the minimum segment and a pause: no split.
	if n := feed(.002, 1000) + feed(.08, 8000) + feed(.002, 1500); n != 0 {
		t.Fatalf("short take split %d times", n)
	}
	// Speech past 15 s followed by a pause commits once.
	if n := feed(.08, 9000) + feed(.002, 1000); n != 1 {
		t.Fatalf("long take: %d commits", n)
	}
	if s.speech {
		t.Fatal("segment after commit should start without speech")
	}
	// A monologue without pauses is still cut at the hard cap.
	if n := feed(.08, 125000); n != 1 {
		t.Fatalf("pause-less monologue: %d commits", n)
	}
}

func TestAudioHelpers(t *testing.T) {
	pcm := make([]byte, 4800)
	for i := 0; i < len(pcm)/2; i++ {
		v := int16(16384 * math.Sin(float64(i)/10))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	if r := pcmRMS(pcm); r < .3 || r > .4 {
		t.Errorf("rms = %v, want ~0.35", r)
	}
	if audioDuration(sampleRate*2*3) != 3*time.Second {
		t.Error("duration")
	}
	if levelOf(0) != 0 || levelOf(1) != 1 {
		t.Error("level bounds")
	}
	if fmtDuration(75*time.Second) != "1:15" {
		t.Error("fmtDuration")
	}
}

func TestSounds(t *testing.T) {
	for _, w := range [][]byte{soundStart, soundStop, soundError} {
		if string(w[:4]) != "RIFF" || string(w[8:16]) != "WAVEfmt " || int(binary.LittleEndian.Uint32(w[4:])) != len(w)-8 {
			t.Fatal("bad WAV header")
		}
	}
}

func TestIconICO(t *testing.T) {
	ico := icon.ICO(true, 16, 32, 256)
	if binary.LittleEndian.Uint16(ico[2:]) != 1 || binary.LittleEndian.Uint16(ico[4:]) != 3 {
		t.Fatal("bad ICONDIR")
	}
	// Last entry: 256 px is stored as 0 and points at a PNG.
	e := ico[6+16*2:]
	if e[0] != 0 || e[1] != 0 {
		t.Fatal("256px entry size")
	}
	off := binary.LittleEndian.Uint32(e[12:])
	if _, err := png.Decode(bytes.NewReader(ico[off:])); err != nil {
		t.Fatal(err)
	}
}

// TestOverlayRender draws every indicator state. Set STT_SHOTS=dir to keep
// the PNGs for a visual check.
func TestOverlayRender(t *testing.T) {
	frames := map[string]overlayFrame{
		"listening": {Kind: ovListening, Text: "Говорите…", Dim: true, Hint: "0:03", Level: .6, T: .4},
		"live":      {Kind: ovListening, Text: "ну короче я хотел сказать что завтра встречаемся в три часа у входа в метро и потом идём", Hint: "0:12", Level: .9, T: 1.1},
		"working":   {Kind: ovWorking, Text: "Улучшаю текст…", T: .3},
		"done":      {Kind: ovDone, Text: "Вставлено"},
		"error":     {Kind: ovError, Text: "OpenAI недоступен из вашего региона — включите VPN или укажите прокси в разделе API"},
		"info":      {Kind: ovInfo, Text: "Отменено"},
	}
	dir := os.Getenv("STT_SHOTS")
	for name, f := range frames {
		for _, scale := range []float64{1, 1.5} {
			w := overlayWidth(f, scale)
			if w < ovMinW || w > ovMaxMsgW {
				t.Fatalf("%s: width %v", name, w)
			}
			c := renderOverlay(f, w, scale)
			img := c.RGBA()
			// The pill's centre is opaque, the corner (shadow margin) is not.
			if a := img.RGBAAt(c.W/2, c.H/2).A; a < 240 {
				t.Fatalf("%s@%v: centre alpha %d", name, scale, a)
			}
			if a := img.RGBAAt(0, 0).A; a != 0 {
				t.Fatalf("%s@%v: corner alpha %d", name, scale, a)
			}
			if dir != "" {
				var b bytes.Buffer
				_ = png.Encode(&b, img)
				_ = os.WriteFile(filepath.Join(dir, name+map[float64]string{1: "", 1.5: "@1.5x"}[scale]+".png"), b.Bytes(), 0o644)
			}
		}
	}
}
