package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"unsafe"
)

var procPlaySoundW = winmm.NewProc("PlaySoundW")

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
)

// Short synthesized cues; PlaySound reads them asynchronously, so they live
// for the whole run.
var (
	soundStart = tones(0.22, [2]float64{660, 70}, [2]float64{990, 90})
	soundStop  = tones(0.22, [2]float64{990, 70}, [2]float64{660, 90})
	soundError = tones(0.20, [2]float64{392, 110}, [2]float64{294, 160})
)

// tones builds a WAV of consecutive notes ({frequency Hz, length ms}) with
// soft attack and release so they don't click.
func tones(volume float64, notes ...[2]float64) []byte {
	const rate = 44100
	var pcm []int16
	for _, n := range notes {
		count := int(rate * n[1] / 1000)
		for i := range count {
			t := float64(i) / rate
			env := math.Min(1, math.Min(float64(i)/(rate*.006), float64(count-i)/(rate*.025)))
			v := math.Sin(2*math.Pi*n[0]*t) + .25*math.Sin(4*math.Pi*n[0]*t)
			pcm = append(pcm, int16(v/1.25*env*volume*32767))
		}
	}
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("RIFF")
	_ = binary.Write(&b, le, uint32(36+len(pcm)*2))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, le, struct {
		Size             uint32
		Format, Channels uint16
		Rate, ByteRate   uint32
		Align, Bits      uint16
	}{16, 1, 1, rate, rate * 2, 2, 16})
	b.WriteString("data")
	_ = binary.Write(&b, le, uint32(len(pcm)*2))
	_ = binary.Write(&b, le, pcm)
	return b.Bytes()
}

func playSound(wav []byte) {
	procPlaySoundW.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndMemory|sndAsync|sndNoDefault)
}
