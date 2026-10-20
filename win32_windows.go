package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 entry points shared by the keyboard hook, the overlay and text
// insertion (window_windows.go declares the ones the main window uses).
var (
	procSetWindowsHookExW          = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx             = user32.NewProc("CallNextHookEx")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procPeekMessageW               = user32.NewProc("PeekMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procMsgWaitForMultipleObjects  = user32.NewProc("MsgWaitForMultipleObjects")
	procSendInput                  = user32.NewProc("SendInput")
	procMapVirtualKeyW             = user32.NewProc("MapVirtualKeyW")
	procGetAsyncKeyState           = user32.NewProc("GetAsyncKeyState")
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procIsWindow                   = user32.NewProc("IsWindow")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procUpdateLayeredWindow        = user32.NewProc("UpdateLayeredWindow")
	procGetDC                      = user32.NewProc("GetDC")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procEnumClipboardFormats       = user32.NewProc("EnumClipboardFormats")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")

	gdi32                  = windows.NewLazySystemDLL("gdi32.dll")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")

	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalSize       = kernel32.NewProc("GlobalSize")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	shcore               = windows.NewLazySystemDLL("shcore.dll")
	procGetDpiForMonitor = shcore.NewProc("GetDpiForMonitor")
)

// winMsg mirrors MSG.
type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

type point struct{ X, Y int32 }

// keyInput mirrors INPUT with a KEYBDINPUT (40 bytes on 64-bit Windows).
type keyInput struct {
	Type      uint32
	_         uint32
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
	_         [8]byte
}

const (
	inputKeyboard      = 1
	keyeventfExtended  = 0x0001
	keyeventfKeyUp     = 0x0002
	keyeventfUnicode   = 0x0004
	injectedMarker     = 0x53545454 // "STTT": our own synthetic keys, ignored by the hook
	vkMaskKey          = 0xE8       // unassigned; used to keep Alt/Win from opening menus
	pmRemove           = 0x0001
	qsAllInput         = 0x04FF
	waitInfinite       = 0xFFFFFFFF
	monitorDefNearestW = 0x2
)

func keyDown(vk uint16) keyInput {
	scan, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	return keyInput{Type: inputKeyboard, Vk: vk, Scan: uint16(scan), ExtraInfo: injectedMarker}
}

func keyUp(vk uint16) keyInput {
	k := keyDown(vk)
	k.Flags |= keyeventfKeyUp
	return k
}

func sendInputs(in []keyInput) int {
	if len(in) == 0 {
		return 0
	}
	n, _, _ := procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
	return int(n)
}

// osPtr turns an address handed out by Windows (hook data, global memory),
// which the Go garbage collector never moves or frees, into a pointer.
func osPtr(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

func moduleHandle() windows.Handle {
	h, _, _ := procGetModuleHandleW.Call(0)
	return windows.Handle(h)
}

func keyHeld(vk int) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

func foregroundWindow() windows.HWND {
	h, _, _ := procGetForegroundWindow.Call()
	return windows.HWND(h)
}

func windowPID(h windows.HWND) uint32 {
	var pid uint32
	procGetWindowThreadProcessId.Call(uintptr(h), uintptr(unsafe.Pointer(&pid)))
	return pid
}

// pumpMessages dispatches everything queued for the calling thread.
func pumpMessages() {
	var m winMsg
	for {
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
