package main

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// System audio capture: WASAPI loopback records exactly what an output
// device plays (a call, a video, a voice message), converted to the same
// 24 kHz PCM the microphone path produces.

var (
	iidIAudioClient        = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	subtypeIEEEFloat       = windows.GUID{Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
)

const (
	audclntShareModeShared  = 0
	audclntStreamLoopback   = 0x00020000
	audclntBufferSilent     = 0x2
	waveFormatPCM           = 1
	waveFormatFloat         = 3
	waveFormatExtensible    = 0xFFFE
	loopbackBuffer100ns     = 2_000_000 // 200 ms device buffer
	loopbackPollInterval    = 15 * time.Millisecond
	audclntEDeviceInvalided = 0x88890004
)

type loopback struct {
	stop chan struct{}
	done chan struct{}
}

type loopStart struct {
	found bool
	err   error
}

// startLoopback records an output device (endpoint id, "" = default) and
// calls onData with ~100 ms PCM chunks. found is false when the requested
// device is missing and the default one is used.
func startLoopback(endpoint string, onData func(pcm []byte)) (*loopback, bool, error) {
	l := &loopback{stop: make(chan struct{}), done: make(chan struct{})}
	ready := make(chan loopStart, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(l.done)
		if err := comThread(func(enum *comObject) { l.run(enum, endpoint, onData, ready) }); err != nil {
			ready <- loopStart{true, err}
		}
	}()
	r := <-ready
	if r.err != nil {
		return nil, r.found, r.err
	}
	return l, r.found, nil
}

func hresult(op string, hr uintptr) error {
	if int32(hr) >= 0 {
		return nil
	}
	if uint32(hr) == audclntEDeviceInvalided {
		return errors.New("устройство вывода отключено")
	}
	return fmt.Errorf("%s: 0x%08X", op, uint32(hr))
}

func (l *loopback) run(enum *comObject, endpoint string, onData func([]byte), ready chan<- loopStart) {
	found := true
	var dev *comObject
	if endpoint != "" {
		p, _ := windows.UTF16PtrFromString(endpoint)
		if int32(enum.call(5, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&dev)))) < 0 {
			dev, found = nil, false
		}
	}
	if dev == nil {
		if int32(enum.call(4, eRender, eConsole, uintptr(unsafe.Pointer(&dev)))) < 0 {
			ready <- loopStart{found, errors.New("в системе нет устройства вывода звука")}
			return
		}
	}
	defer dev.release()

	var client *comObject
	if err := hresult("Activate", dev.call(3, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&client)))); err != nil {
		ready <- loopStart{found, err}
		return
	}
	defer client.release()

	var mixAddr uintptr
	if err := hresult("GetMixFormat", client.call(8, uintptr(unsafe.Pointer(&mixAddr)))); err != nil {
		ready <- loopStart{found, err}
		return
	}
	defer procCoTaskMemFree.Call(mixAddr)
	mix := (*waveFormatEx)(osPtr(mixAddr))
	format := sampleFormat{channels: int(mix.Channels), bits: int(mix.BitsPerSample), float: mix.FormatTag == waveFormatFloat}
	if mix.FormatTag == waveFormatExtensible {
		sub := (*windows.GUID)(unsafe.Add(osPtr(mixAddr), 24))
		format.float = *sub == subtypeIEEEFloat
	}
	if format.channels == 0 || format.bits%8 != 0 || format.bits == 0 {
		ready <- loopStart{found, fmt.Errorf("неподдерживаемый формат звука (%d бит, %d каналов)", format.bits, format.channels)}
		return
	}
	block := format.channels * format.bits / 8

	if err := hresult("Initialize", client.call(3, audclntShareModeShared, audclntStreamLoopback, loopbackBuffer100ns, 0, mixAddr, 0)); err != nil {
		ready <- loopStart{found, err}
		return
	}
	var capture *comObject
	if err := hresult("GetService", client.call(14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&capture)))); err != nil {
		ready <- loopStart{found, err}
		return
	}
	defer capture.release()
	if err := hresult("Start", client.call(10)); err != nil {
		ready <- loopStart{found, err}
		return
	}
	defer client.call(11)
	ready <- loopStart{found, nil}

	rs := newResampler(int(mix.SamplesPerSec))
	var mono []float32
	tick := time.NewTicker(loopbackPollInterval)
	defer tick.Stop()
	for {
		for {
			var packet uint32
			if int32(capture.call(5, uintptr(unsafe.Pointer(&packet)))) < 0 || packet == 0 {
				break
			}
			var data uintptr
			var frames, flags uint32
			if int32(capture.call(3, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0)) < 0 {
				break
			}
			mono = mono[:0]
			if flags&audclntBufferSilent != 0 || data == 0 {
				mono = append(mono, make([]float32, frames)...)
			} else {
				mono = format.toMono(unsafe.Slice((*byte)(osPtr(data)), int(frames)*block), mono)
			}
			capture.call(4, uintptr(frames))
			rs.push(mono)
		}
		select {
		case <-l.stop:
			for _, c := range rs.take(chunkBytes, true) {
				onData(c)
			}
			return
		case <-tick.C:
			for _, c := range rs.take(chunkBytes, false) {
				onData(c)
			}
		}
	}
}

// Stop ends capture; every chunk has been delivered when it returns.
func (l *loopback) Stop() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	<-l.done
}
