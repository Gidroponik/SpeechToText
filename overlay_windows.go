package main

import (
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// overlay is a click-through, always-on-top layered window that never
// activates, so the dictation indicator can't steal the focus from the input
// the text is going into. It runs its own message loop on a dedicated thread
// and renders at ~60 fps only while visible.
type overlay struct {
	mu      sync.Mutex
	frame   overlayFrame
	visible bool
	hideAt  time.Time
	wake    windows.Handle
	hwnd    windows.HWND
}

const (
	wsPopup           = 0x80000000
	wsExLayered       = 0x00080000
	wsExTransparent   = 0x00000020
	wsExTopmost       = 0x00000008
	wsExToolWindow    = 0x00000080
	wsExNoActivate    = 0x08000000
	swHide            = 0
	swShowNoActivate  = 4
	ulwAlpha          = 0x2
	hwndTopmost       = ^uintptr(0) // (HWND)-1
	swpNoSize         = 0x0001
	swpNoMove         = 0x0002
	monitorDefPrimary = 0x1
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPels, YPels  int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type winSize struct{ CX, CY int32 }

type blendFunction struct{ Op, Flags, SourceAlpha, AlphaFormat byte }

func startOverlay() (*overlay, error) {
	o := &overlay{}
	ready := make(chan error)
	go o.run(ready)
	return o, <-ready
}

// Show displays f; a positive hideAfter hides it again after that long.
func (o *overlay) Show(f overlayFrame, hideAfter time.Duration) {
	o.mu.Lock()
	o.frame, o.visible = f, true
	o.hideAt = time.Time{}
	if hideAfter > 0 {
		o.hideAt = time.Now().Add(hideAfter)
	}
	o.mu.Unlock()
	windows.SetEvent(o.wake)
}

// Update changes the content without touching visibility (the render loop
// picks it up on its next frame).
func (o *overlay) Update(fn func(*overlayFrame)) {
	o.mu.Lock()
	fn(&o.frame)
	o.mu.Unlock()
}

func (o *overlay) Hide() {
	o.mu.Lock()
	o.visible = false
	o.mu.Unlock()
	windows.SetEvent(o.wake)
}

// targetMonitor picks the work area and DPI scale of the monitor with the
// focused window, where the user is looking.
func targetMonitor() (rect, float64) {
	mon, _, _ := procMonitorFromWindow.Call(uintptr(foregroundWindow()), monitorDefNearestW)
	if mon == 0 {
		mon, _, _ = procMonitorFromWindow.Call(0, monitorDefPrimary)
	}
	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	var dx, dy uint32
	scale := 1.0
	if r, _, _ := procGetDpiForMonitor.Call(mon, 0, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy))); r == 0 && dx > 0 {
		scale = float64(dx) / 96
	}
	return mi.work, scale
}

func (o *overlay) run(ready chan error) {
	runtime.LockOSThread()

	inst := moduleHandle()
	cls, _ := windows.UTF16PtrFromString("SpeechToTextOverlay")
	wc := wndClassEx{WndProc: procDefWindowProcW.Addr(), Instance: inst, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	h, _, err := procCreateWindowExW.Call(wsExLayered|wsExTransparent|wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)), wsPopup, 0, 0, 1, 1, 0, 0, uintptr(inst), 0)
	if h == 0 {
		ready <- err
		return
	}
	o.hwnd = windows.HWND(h)
	clipOwner = o.hwnd
	if o.wake, err = windows.CreateEvent(nil, 0, 0, nil); err != nil {
		ready <- err
		return
	}
	ready <- nil

	screen, _, _ := procGetDC.Call(0)
	memDC, _, _ := procCreateCompatibleDC.Call(screen)
	var (
		dib, oldBmp  uintptr
		bits         unsafe.Pointer
		dibW, dibH   int
		shown        bool
		alpha, width float64
		level        float64
		shownAt      time.Time
		work         rect
		scale        = 1.0
		last         = time.Now()
	)

	for {
		timeout := uintptr(waitInfinite)
		if shown {
			timeout = 16
		}
		procMsgWaitForMultipleObjects.Call(1, uintptr(unsafe.Pointer(&o.wake)), 0, timeout, qsAllInput)
		pumpMessages()

		now := time.Now()
		dt := math.Min(now.Sub(last).Seconds(), .05)
		last = now

		o.mu.Lock()
		if o.visible && !o.hideAt.IsZero() && now.After(o.hideAt) {
			o.visible = false
		}
		f, visible := o.frame, o.visible
		o.mu.Unlock()
		if !f.Clock.IsZero() {
			f.Hint = fmtDuration(now.Sub(f.Clock))
		}

		if visible && !shown {
			work, scale = targetMonitor()
			width = overlayWidth(f, scale)
			shownAt, alpha, level = now, 0, 0
		}
		if !visible && !shown {
			continue
		}

		if visible {
			alpha = math.Min(1, alpha+dt/.16)
		} else {
			alpha = math.Max(0, alpha-dt/.14)
			if alpha == 0 {
				procShowWindow.Call(h, swHide)
				shown = false
				continue
			}
		}
		target := overlayWidth(f, scale)
		width += (target - width) * math.Min(1, dt*14)
		if math.Abs(target-width) < .5 {
			width = target
		}
		level += (f.Level - level) * math.Min(1, dt*18)
		f.Level = level
		f.T = now.Sub(shownAt).Seconds()

		c := renderOverlay(f, width, scale)
		if c.W != dibW || c.H != dibH {
			if dib != 0 {
				procSelectObject.Call(memDC, oldBmp)
				procDeleteObject.Call(dib)
			}
			bi := bitmapInfoHeader{Size: 40, Width: int32(c.W), Height: -int32(c.H), Planes: 1, BitCount: 32}
			dib, _, _ = procCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
			if dib == 0 {
				continue
			}
			oldBmp, _, _ = procSelectObject.Call(memDC, dib)
			dibW, dibH = c.W, c.H
		}
		c.BGRA(unsafe.Slice((*byte)(bits), c.W*c.H*4), 1)

		e := easeOutQuart(alpha)
		pos := point{
			X: work.Left + (work.w()-int32(c.W))/2,
			Y: work.Bottom - int32(c.H) - int32(40*scale) + int32((1-e)*10*scale),
		}
		sz := winSize{int32(c.W), int32(c.H)}
		src := point{}
		blend := blendFunction{SourceAlpha: byte(alpha * 255), AlphaFormat: 1}
		procUpdateLayeredWindow.Call(h, screen, uintptr(unsafe.Pointer(&pos)), uintptr(unsafe.Pointer(&sz)),
			memDC, uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)
		if !shown {
			procShowWindow.Call(h, swShowNoActivate)
			procSetWindowPos.Call(h, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
			shown = true
		}
	}
}
