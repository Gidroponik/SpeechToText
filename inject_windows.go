package main

import (
	"errors"
	"os"
	"runtime"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Putting the finished text into whatever input has the focus (a Telegram
// chat, a browser field, Word…): paste through the clipboard and put the
// user's clipboard back, or type it as Unicode keystrokes.

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// clipOwner is the window that owns clipboard data we set (the overlay).
var clipOwner windows.HWND

type clipItem struct {
	format uint32
	data   []byte
}

// openClipboard retries for a moment: another app may be holding it.
func openClipboard(owner windows.HWND) error {
	for range 25 {
		if r, _, _ := procOpenClipboard.Call(uintptr(owner)); r != 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("буфер обмена занят другой программой")
}

// gdiFormat reports clipboard formats held as GDI handles rather than
// memory, which cannot be copied byte for byte.
func gdiFormat(f uint32) bool {
	switch f {
	case 2, 3, 9, 14, 0x80, 0x82, 0x83, 0x8E: // BITMAP, METAFILEPICT, PALETTE, ENHMETAFILE, OWNERDISPLAY, DSP*
		return true
	}
	return false
}

// snapshotClipboard copies every memory-based format currently on the clipboard.
func snapshotClipboard() ([]clipItem, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if openClipboard(0) != nil {
		return nil, false
	}
	defer procCloseClipboard.Call()
	var items []clipItem
	total := 0
	for f := uint32(0); ; {
		r, _, _ := procEnumClipboardFormats.Call(uintptr(f))
		if f = uint32(r); f == 0 {
			break
		}
		if gdiFormat(f) {
			continue
		}
		h, _, _ := procGetClipboardData.Call(uintptr(f))
		if h == 0 {
			continue
		}
		size, _, _ := procGlobalSize.Call(h)
		p, _, _ := procGlobalLock.Call(h)
		if p == 0 {
			continue
		}
		data := append([]byte(nil), unsafe.Slice((*byte)(osPtr(p)), size)...)
		procGlobalUnlock.Call(h)
		if total += len(data); total > 64<<20 {
			return nil, false // too big to juggle; leave the clipboard as we found it
		}
		items = append(items, clipItem{f, data})
	}
	return items, true
}

func globalCopy(data []byte) uintptr {
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(max(len(data), 1)))
	if h == 0 {
		return 0
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return 0
	}
	copy(unsafe.Slice((*byte)(osPtr(p)), len(data)), data)
	procGlobalUnlock.Call(h)
	return h
}

func setClipboardItems(items []clipItem) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openClipboard(clipOwner); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	for _, it := range items {
		h := globalCopy(it.data)
		if h == 0 {
			continue
		}
		if r, _, _ := procSetClipboardData.Call(uintptr(it.format), h); r == 0 {
			procGlobalFree.Call(h)
		}
	}
	return nil
}

func registerFormat(name string) uint32 {
	p, _ := windows.UTF16PtrFromString(name)
	r, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(p)))
	return uint32(r)
}

func utf16Bytes(s string) []byte {
	u := append(utf16.Encode([]rune(s)), 0)
	return unsafe.Slice((*byte)(unsafe.Pointer(&u[0])), len(u)*2)
}

// setClipboardText puts text on the clipboard. Transient text is kept out of
// Win+V history and cloud clipboard sync.
func setClipboardText(text string, transient bool) error {
	items := []clipItem{{cfUnicodeText, utf16Bytes(text)}}
	if transient {
		zero := []byte{0, 0, 0, 0}
		items = append(items,
			clipItem{registerFormat("ExcludeClipboardContentFromMonitorProcessing"), zero},
			clipItem{registerFormat("CanIncludeInClipboardHistory"), zero},
			clipItem{registerFormat("CanUploadToCloudClipboard"), zero})
	}
	return setClipboardItems(items)
}

func clipboardSeq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// waitModifiersReleased gives the user a moment to let go of the hotkey, so
// a held Shift doesn't turn Ctrl+V into Ctrl+Shift+V or capitalise typing.
func waitModifiersReleased(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if len(heldModifiers()) == 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func sendCtrlV() {
	sendInputs([]keyInput{keyDown(vkControl), keyDown(vkV), keyUp(vkV), keyUp(vkControl)})
}

func pressEnter() {
	sendInputs([]keyInput{keyDown(vkReturn), keyUp(vkReturn)})
}

// typeText sends text as Unicode keystrokes. Line breaks become Shift+Enter,
// which is a new line (not "send") in messengers and plain Enter elsewhere.
func typeText(text string) {
	var batch []keyInput
	flush := func() {
		sendInputs(batch)
		batch = batch[:0]
		time.Sleep(4 * time.Millisecond)
	}
	for _, r := range []rune(text) {
		switch r {
		case '\r':
			continue
		case '\n':
			flush()
			sendInputs([]keyInput{keyDown(vkLShift), keyDown(vkReturn), keyUp(vkReturn), keyUp(vkLShift)})
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			batch = append(batch,
				keyInput{Type: inputKeyboard, Scan: u, Flags: keyeventfUnicode, ExtraInfo: injectedMarker},
				keyInput{Type: inputKeyboard, Scan: u, Flags: keyeventfUnicode | keyeventfKeyUp, ExtraInfo: injectedMarker})
		}
		if len(batch) >= 64 {
			flush()
		}
	}
	flush()
}

// processElevated reports whether the process behind a window runs as
// administrator: a normal app cannot send keystrokes into it (UIPI).
func processElevated(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok) != nil {
		return false
	}
	defer tok.Close()
	return tok.IsElevated()
}

type insertOptions struct {
	Mode             string // paste | type | clipboard
	RestoreClipboard bool
	PressEnter       bool
}

// insertText delivers text to the focused input of target (the window that
// was active when dictation started) and describes what happened.
func insertText(text string, o insertOptions, target windows.HWND) (string, error) {
	if o.Mode == "clipboard" {
		if err := setClipboardText(text, false); err != nil {
			return "", err
		}
		return "Скопировано в буфер", nil
	}

	fg := foregroundWindow()
	if windowPID(fg) == uint32(os.Getpid()) {
		// Our own panel is in front; the text belongs to the app used before.
		if r, _, _ := procIsWindow.Call(uintptr(target)); r == 0 || windowPID(target) == uint32(os.Getpid()) {
			if err := setClipboardText(text, false); err != nil {
				return "", err
			}
			return "Скопировано в буфер", nil
		}
		procSetForegroundWindow.Call(uintptr(target))
		time.Sleep(120 * time.Millisecond)
		fg = target
	}
	if !windows.GetCurrentProcessToken().IsElevated() && processElevated(windowPID(fg)) {
		if err := setClipboardText(text, false); err != nil {
			return "", err
		}
		return "Окно запущено от администратора — текст в буфере, нажмите Ctrl+V", nil
	}

	waitModifiersReleased(2 * time.Second)

	if o.Mode == "type" {
		typeText(text)
	} else {
		var saved []clipItem
		restore := false
		if o.RestoreClipboard {
			saved, restore = snapshotClipboard()
		}
		if err := setClipboardText(text, true); err != nil {
			return "", err
		}
		seq := clipboardSeq()
		sendCtrlV()
		if restore {
			go func() {
				// Give the app time to read the clipboard, then put the
				// user's data back unless something else replaced it.
				time.Sleep(700 * time.Millisecond)
				if clipboardSeq() == seq {
					_ = setClipboardItems(saved)
				}
			}()
		}
	}
	if o.PressEnter {
		time.Sleep(150 * time.Millisecond)
		pressEnter()
	}
	return "Вставлено", nil
}
