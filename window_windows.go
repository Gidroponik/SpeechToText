package main

import (
	"math"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetDpiForWindow          = user32.NewProc("GetDpiForWindow")
	procMonitorFromWindow        = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW          = user32.NewProc("GetMonitorInfoW")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
)

var (
	dwmapi                    = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	procDwmFlush              = dwmapi.NewProc("DwmFlush")
)

const (
	dwmwaWindowCornerPreference = 33
	dwmwcpDoNotRound            = 1
	swpNoZOrder                 = 0x0004
	swpNoActivate               = 0x0010
	monitorDefNearest           = 0x2
)

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) w() int32 { return r.Right - r.Left }
func (r rect) h() int32 { return r.Bottom - r.Top }

type monitorInfo struct {
	cbSize  uint32
	monitor rect
	work    rect
	flags   uint32
}

// nativeWindow drives the Wails main window through Win32 so that move and
// resize happen atomically in one SetWindowPos call per animation frame.
type nativeWindow struct {
	hwnd windows.HWND
}

var (
	enumMu    sync.Mutex
	enumFound windows.HWND
)

// enumCallback finds this process's Wails window (class "wailsWindow").
var enumCallback = syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
	if pid != uint32(os.Getpid()) {
		return 1
	}
	buf := make([]uint16, 64)
	procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if windows.UTF16ToString(buf) != "wailsWindow" {
		return 1
	}
	enumFound = hwnd
	return 0
})

func (n *nativeWindow) handle() windows.HWND {
	enumMu.Lock()
	defer enumMu.Unlock()
	if n.hwnd == 0 {
		enumFound = 0
		procEnumWindows.Call(enumCallback, 0)
		n.hwnd = enumFound
	}
	return n.hwnd
}

func (n *nativeWindow) rect() rect {
	var r rect
	procGetWindowRect.Call(uintptr(n.handle()), uintptr(unsafe.Pointer(&r)))
	return r
}

func (n *nativeWindow) scale() float64 {
	dpi, _, _ := procGetDpiForWindow.Call(uintptr(n.handle()))
	if dpi == 0 {
		return 1
	}
	return float64(dpi) / 96
}

func (n *nativeWindow) workArea() rect {
	mon, _, _ := procMonitorFromWindow.Call(uintptr(n.handle()), monitorDefNearest)
	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	return mi.work
}

func (n *nativeWindow) setRect(r rect) {
	procSetWindowPos.Call(uintptr(n.handle()), 0, uintptr(r.Left), uintptr(r.Top),
		uintptr(r.w()), uintptr(r.h()), swpNoZOrder|swpNoActivate)
}

// disableDwmCorners stops Windows 11 from clipping the window to its own
// small rounding; the card's corners are drawn by CSS instead.
func (n *nativeWindow) disableDwmCorners() {
	pref := uint32(dwmwcpDoNotRound)
	procDwmSetWindowAttribute.Call(uintptr(n.handle()), dwmwaWindowCornerPreference, uintptr(unsafe.Pointer(&pref)), 4)
}

func (n *nativeWindow) isVisible() bool {
	r, _, _ := procIsWindowVisible.Call(uintptr(n.handle()))
	return r != 0
}

func (n *nativeWindow) focus() {
	procSetForegroundWindow.Call(uintptr(n.handle()))
}

// animate moves/resizes the window from its current bounds to target. Each
// frame waits for the compositor (DwmFlush), so steps land on monitor
// refreshes instead of drifting against them.
func (n *nativeWindow) animate(target rect, d time.Duration) {
	from := n.rect()
	start := time.Now()
	for {
		t := math.Min(1, float64(time.Since(start))/float64(d))
		e := easeOutQuart(t)
		lerp := func(a, b int32) int32 { return a + int32(math.Round(float64(b-a)*e)) }
		n.setRect(rect{lerp(from.Left, target.Left), lerp(from.Top, target.Top), lerp(from.Right, target.Right), lerp(from.Bottom, target.Bottom)})
		if t >= 1 {
			return
		}
		if r, _, _ := procDwmFlush.Call(); r != 0 {
			time.Sleep(8 * time.Millisecond) // composition unavailable
		}
	}
}

func easeOutQuart(t float64) float64 { return 1 - math.Pow(1-t, 4) }

// centeredRect returns a w×h (CSS px) rectangle centred on the window's monitor.
func (n *nativeWindow) centeredRect(w, h int) rect {
	s := n.scale()
	pw, ph := int32(float64(w)*s), int32(float64(h)*s)
	wa := n.workArea()
	ph = min(ph, wa.h())
	x := wa.Left + (wa.w()-pw)/2
	y := wa.Top + (wa.h()-ph)/2
	return rect{x, y, x + pw, y + ph}
}
