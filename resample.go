package main

import (
	"encoding/binary"
	"math"
)

// resampler turns mono float audio at any rate into the 24 kHz 16-bit PCM
// the Realtime API takes. A moving average as wide as the rate ratio keeps
// most of the aliasing out before linear interpolation; plenty for speech.
type resampler struct {
	ratio float64   // source samples per output sample
	pos   float64   // where the next output sample falls in buf
	buf   []float32 // filtered source samples not yet consumed
	ring  []float32 // moving-average window
	ri    int
	sum   float64
	out   []byte // PCM waiting to be cut into chunks
}

func newResampler(srcRate int) *resampler {
	r := &resampler{ratio: float64(srcRate) / sampleRate}
	r.ring = make([]float32, max(1, int(math.Round(r.ratio))))
	return r
}

// push adds mono samples and converts what it can.
func (r *resampler) push(mono []float32) {
	for _, x := range mono {
		r.sum += float64(x) - float64(r.ring[r.ri])
		r.ring[r.ri] = x
		r.ri = (r.ri + 1) % len(r.ring)
		r.buf = append(r.buf, float32(r.sum/float64(len(r.ring))))
	}
	for r.pos+1 < float64(len(r.buf)) {
		i := int(r.pos)
		f := float32(r.pos - float64(i))
		v := r.buf[i]*(1-f) + r.buf[i+1]*f
		s := int16(math.Max(-32768, math.Min(32767, float64(v)*32767)))
		r.out = binary.LittleEndian.AppendUint16(r.out, uint16(s))
		r.pos += r.ratio
	}
	drop := int(r.pos)
	r.buf = append(r.buf[:0], r.buf[drop:]...)
	r.pos -= float64(drop)
}

// take returns whole chunks of converted PCM, or everything when flush is set.
func (r *resampler) take(chunk int, flush bool) [][]byte {
	var chunks [][]byte
	for len(r.out) >= chunk || flush && len(r.out) > 0 {
		n := min(chunk, len(r.out))
		chunks = append(chunks, append([]byte(nil), r.out[:n]...))
		r.out = r.out[n:]
	}
	return chunks
}

// sampleFormat describes interleaved device audio.
type sampleFormat struct {
	channels int
	bits     int
	float    bool
}

// toMono decodes interleaved frames and averages the channels.
func (f sampleFormat) toMono(data []byte, dst []float32) []float32 {
	bytesPer := f.bits / 8
	frame := bytesPer * f.channels
	for off := 0; off+frame <= len(data); off += frame {
		var sum float32
		for c := 0; c < f.channels; c++ {
			b := data[off+c*bytesPer:]
			switch {
			case f.float && f.bits == 32:
				sum += math.Float32frombits(binary.LittleEndian.Uint32(b))
			case f.bits == 16:
				sum += float32(int16(binary.LittleEndian.Uint16(b))) / 32768
			case f.bits == 24:
				sum += float32(int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24)) / 2147483648
			case f.bits == 32:
				sum += float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
			}
		}
		dst = append(dst, sum/float32(f.channels))
	}
	return dst
}
