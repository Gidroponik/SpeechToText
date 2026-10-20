package main

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

const (
	whKeyboardLL = 13
	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104
)

// kbdLLHookStruct mirrors KBDLLHOOKSTRUCT.
type kbdLLHookStruct struct {
	VkCode    uint32
	ScanCode  uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// keyHook is a system-wide low-level keyboard hook. Unlike RegisterHotKey it
// sees key releases (needed for push-to-talk), can bind a lone Right Ctrl and
// can swallow the hotkey so it doesn't type into the focused app.
type keyHook struct {
	mu       sync.Mutex
	m        *keyMachine
	events   chan boundEvent
	captured chan *Hotkey
}

// theHook is reached from the hook callback, which cannot carry context.
var theHook *keyHook

var hookProc = syscall.NewCallback(func(nCode, wParam, lParam uintptr) uintptr {
	if int32(nCode) == 0 && theHook != nil {
		kb := (*kbdLLHookStruct)(osPtr(lParam))
		if kb.ExtraInfo != injectedMarker && theHook.process(kb.VkCode, wParam == wmKeyDown || wParam == wmSysKeyDown) {
			return 1
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
})

func startKeyHook() (*keyHook, error) {
	h := &keyHook{m: newKeyMachine(), events: make(chan boundEvent, 32), captured: make(chan *Hotkey, 1)}
	theHook = h
	ready := make(chan error)
	go func() {
		// The hook is called on this thread, which must pump messages. It
		// does nothing else, so the callback is never late (Windows silently
		// drops hooks that take too long).
		runtime.LockOSThread()
		mod := moduleHandle()
		hhk, _, err := procSetWindowsHookExW.Call(whKeyboardLL, hookProc, uintptr(mod), 0)
		if hhk == 0 {
			ready <- err
			return
		}
		ready <- nil
		var m winMsg
		for {
			if r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
				return
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	return h, <-ready
}

// process runs inside the hook callback: it must be quick and never block.
func (h *keyHook) process(vk uint32, down bool) bool {
	h.mu.Lock()
	if down && !h.m.capturing {
		if _, isMod := modifierBit(vk); !isMod {
			h.m.syncModifiers(heldModifiers())
		}
	}
	r := h.m.handle(vk, down)
	h.mu.Unlock()

	for _, e := range r.events {
		select {
		case h.events <- e:
		default:
		}
	}
	if r.capDone {
		select {
		case h.captured <- r.captured:
		default:
		}
	}
	if r.mask {
		go sendInputs([]keyInput{keyDown(vkMaskKey), keyUp(vkMaskKey)})
	}
	return r.swallow
}

func heldModifiers() []uint32 {
	var held []uint32
	for _, vk := range []uint32{vkLShift, vkRShift, vkLControl, vkRControl, vkLMenu, vkRMenu, vkLWin, vkRWin} {
		if keyHeld(int(vk)) {
			held = append(held, vk)
		}
	}
	return held
}

func (h *keyHook) SetBindings(specs []bindSpec) {
	h.mu.Lock()
	h.m.setBindings(specs)
	h.mu.Unlock()
}

// SetEscArmed makes Esc cancel dictation (and stops it reaching the app).
func (h *keyHook) SetEscArmed(on bool) {
	h.mu.Lock()
	h.m.escArmed = on
	h.mu.Unlock()
}

// BeginCapture records the next key combination; the result arrives on
// h.captured (nil when cancelled with Esc).
func (h *keyHook) BeginCapture() {
	h.mu.Lock()
	select {
	case <-h.captured: // drop a stale result
	default:
	}
	h.m.beginCapture()
	h.mu.Unlock()
}

func (h *keyHook) CancelCapture() {
	h.mu.Lock()
	wasCapturing := h.m.capturing
	h.m.capturing = false
	h.mu.Unlock()
	if wasCapturing {
		select {
		case h.captured <- nil:
		default:
		}
	}
}
