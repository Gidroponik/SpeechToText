package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Microphone capture through waveIn: it hands out 16-bit mono PCM at exactly
// the 24 kHz the Realtime API expects and resamples from whatever the device
// runs at, with no extra libraries.

var (
	winmm                       = windows.NewLazySystemDLL("winmm.dll")
	procWaveInGetNumDevs        = winmm.NewProc("waveInGetNumDevs")
	procWaveInMessage           = winmm.NewProc("waveInMessage")
	procWaveInOpen              = winmm.NewProc("waveInOpen")
	procWaveInPrepareHeader     = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader   = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer         = winmm.NewProc("waveInAddBuffer")
	procWaveInStart             = winmm.NewProc("waveInStart")
	procWaveInReset             = winmm.NewProc("waveInReset")
	procWaveInClose             = winmm.NewProc("waveInClose")
	drvQueryFunctionInstanceID  = uintptr(0x0811)
	drvQueryFunctionInstanceIDN = uintptr(0x0812)
)

const (
	sampleRate    = 24000
	chunkMs       = 100
	chunkBytes    = sampleRate * 2 * chunkMs / 1000
	numBuffers    = 8
	waveMapper    = 0xFFFFFFFF
	callbackEvent = 0x00050000
	waveMapped    = 0x0004
	whdrDone      = 0x0001
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

// waveHdr mirrors WAVEHDR.
type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

type recorder struct {
	h      uintptr
	event  windows.Handle
	hdrs   [numBuffers]waveHdr
	bufs   [numBuffers][chunkBytes]byte
	pin    runtime.Pinner // the driver writes into hdrs/bufs asynchronously
	onData func(pcm []byte)
	stop   chan struct{}
	done   chan struct{}
}

func mmError(op string, code uintptr) error {
	switch code {
	case 2, 6:
		return fmt.Errorf("микрофон не найден (%s: %d)", op, code)
	case 4:
		return fmt.Errorf("микрофон занят другой программой (%s: %d)", op, code)
	case 32:
		return fmt.Errorf("микрофон не поддерживает формат записи (%s: %d)", op, code)
	}
	return fmt.Errorf("ошибка микрофона (%s: %d)", op, code)
}

// waveInIndex finds the waveIn device number of a capture endpoint.
func waveInIndex(endpoint string) (uint32, bool) {
	n, _, _ := procWaveInGetNumDevs.Call()
	for i := uintptr(0); i < n; i++ {
		var size uint32
		if r, _, _ := procWaveInMessage.Call(i, drvQueryFunctionInstanceIDN, uintptr(unsafe.Pointer(&size)), 0); r != 0 || size == 0 {
			continue
		}
		buf := make([]uint16, size/2+1)
		if r, _, _ := procWaveInMessage.Call(i, drvQueryFunctionInstanceID, uintptr(unsafe.Pointer(&buf[0])), uintptr(size)); r != 0 {
			continue
		}
		if strings.EqualFold(windows.UTF16ToString(buf), endpoint) {
			return uint32(i), true
		}
	}
	return 0, false
}

// startRecorder opens the microphone (endpoint id, "" = system default) and
// calls onData with ~100 ms PCM chunks from its own goroutine. found is
// false when the requested microphone is missing and the default is used.
func startRecorder(endpoint string, onData func(pcm []byte)) (r *recorder, found bool, err error) {
	dev, found := uint32(waveMapper), true
	if endpoint != "" {
		if i, ok := waveInIndex(endpoint); ok {
			dev = i
		} else {
			found = false
		}
	}
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, found, err
	}
	r = &recorder{event: ev, onData: onData, stop: make(chan struct{}), done: make(chan struct{})}
	r.pin.Pin(r)

	format := waveFormatEx{FormatTag: 1, Channels: 1, SamplesPerSec: sampleRate, AvgBytesPerSec: sampleRate * 2, BlockAlign: 2, BitsPerSample: 16}
	flags := uintptr(callbackEvent)
	if dev != waveMapper {
		flags |= waveMapped // let the mapper convert to 24 kHz
	}
	if c, _, _ := procWaveInOpen.Call(uintptr(unsafe.Pointer(&r.h)), uintptr(dev), uintptr(unsafe.Pointer(&format)), uintptr(ev), 0, flags); c != 0 {
		r.pin.Unpin()
		windows.CloseHandle(ev)
		return nil, found, mmError("open", c)
	}
	hdrSize := unsafe.Sizeof(waveHdr{})
	for i := range r.hdrs {
		h := &r.hdrs[i]
		h.Data = uintptr(unsafe.Pointer(&r.bufs[i][0]))
		h.BufferLength = chunkBytes
		procWaveInPrepareHeader.Call(r.h, uintptr(unsafe.Pointer(h)), hdrSize)
		procWaveInAddBuffer.Call(r.h, uintptr(unsafe.Pointer(h)), hdrSize)
	}
	if c, _, _ := procWaveInStart.Call(r.h); c != 0 {
		r.close()
		return nil, found, mmError("start", c)
	}
	go r.loop()
	return r, found, nil
}

func (r *recorder) loop() {
	defer close(r.done)
	hdrSize := unsafe.Sizeof(waveHdr{})
	next := 0
	for {
		windows.WaitForSingleObject(r.event, 100)
		stopping := false
		select {
		case <-r.stop:
			stopping = true
			// Returns every queued buffer, including the partly filled one,
			// so the last words are not lost.
			procWaveInReset.Call(r.h)
		default:
		}
		for range numBuffers {
			h := &r.hdrs[next]
			if atomic.LoadUint32(&h.Flags)&whdrDone == 0 {
				break
			}
			if n := atomic.LoadUint32(&h.BytesRecorded); n > 0 {
				r.onData(append([]byte(nil), r.bufs[next][:n]...))
			}
			atomic.StoreUint32(&h.BytesRecorded, 0)
			atomic.StoreUint32(&h.Flags, atomic.LoadUint32(&h.Flags)&^whdrDone)
			if !stopping {
				procWaveInAddBuffer.Call(r.h, uintptr(unsafe.Pointer(h)), hdrSize)
			}
			next = (next + 1) % numBuffers
		}
		if stopping {
			r.close()
			return
		}
	}
}

func (r *recorder) close() {
	hdrSize := unsafe.Sizeof(waveHdr{})
	procWaveInReset.Call(r.h) // queued buffers cannot be unprepared
	for i := range r.hdrs {
		procWaveInUnprepareHeader.Call(r.h, uintptr(unsafe.Pointer(&r.hdrs[i])), hdrSize)
	}
	procWaveInClose.Call(r.h)
	windows.CloseHandle(r.event)
	r.pin.Unpin()
}

// Stop ends recording; every captured chunk has been delivered when it returns.
func (r *recorder) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
		windows.SetEvent(r.event)
	}
	<-r.done
}
