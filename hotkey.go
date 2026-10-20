package main

import (
	"errors"
	"fmt"
	"strings"
)

// Windows virtual-key codes used by the hotkey logic.
const (
	vkBack     = 0x08
	vkTab      = 0x09
	vkReturn   = 0x0D
	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12 // Alt
	vkPause    = 0x13
	vkCapital  = 0x14
	vkEscape   = 0x1B
	vkSpace    = 0x20
	vkInsert   = 0x2D
	vkDelete   = 0x2E
	vkLWin     = 0x5B
	vkRWin     = 0x5C
	vkApps     = 0x5D
	vkF1       = 0x70
	vkF24      = 0x87
	vkNumLock  = 0x90
	vkScroll   = 0x91
	vkLShift   = 0xA0
	vkRShift   = 0xA1
	vkLControl = 0xA2
	vkRControl = 0xA3
	vkLMenu    = 0xA4
	vkRMenu    = 0xA5
	vkV        = 0x56
)

// Modifier bits, independent of the left/right side.
const (
	modCtrl uint8 = 1 << iota
	modShift
	modAlt
	modWin
)

// Hotkey is a key with optional modifiers, or a single modifier key on its
// own (e.g. Right Ctrl) when Key is a side-specific modifier and no flags are set.
type Hotkey struct {
	Ctrl  bool   `json:"ctrl"`
	Shift bool   `json:"shift"`
	Alt   bool   `json:"alt"`
	Win   bool   `json:"win"`
	Key   uint32 `json:"key"` // virtual-key code, 0 = not set
}

func (h Hotkey) mods() uint8 {
	var m uint8
	if h.Ctrl {
		m |= modCtrl
	}
	if h.Shift {
		m |= modShift
	}
	if h.Alt {
		m |= modAlt
	}
	if h.Win {
		m |= modWin
	}
	return m
}

func hotkeyFrom(mods uint8, key uint32) Hotkey {
	return Hotkey{Ctrl: mods&modCtrl != 0, Shift: mods&modShift != 0, Alt: mods&modAlt != 0, Win: mods&modWin != 0, Key: key}
}

// modifierOnly reports a hotkey that is a lone modifier key such as Right Ctrl.
func (h Hotkey) modifierOnly() bool {
	_, ok := modifierBit(h.Key)
	return ok && h.mods() == 0
}

// modifierBit maps a modifier virtual key to its bit.
func modifierBit(vk uint32) (uint8, bool) {
	switch vk {
	case vkControl, vkLControl, vkRControl:
		return modCtrl, true
	case vkShift, vkLShift, vkRShift:
		return modShift, true
	case vkMenu, vkLMenu, vkRMenu:
		return modAlt, true
	case vkLWin, vkRWin:
		return modWin, true
	}
	return 0, false
}

// sideKey maps generic modifier codes to their left-hand variant, so the
// state below is always tracked per physical key.
func sideKey(vk uint32) uint32 {
	switch vk {
	case vkShift:
		return vkLShift
	case vkControl:
		return vkLControl
	case vkMenu:
		return vkLMenu
	}
	return vk
}

var keyNames = map[uint32]string{
	vkBack: "Backspace", vkTab: "Tab", vkReturn: "Enter", vkPause: "Pause", vkCapital: "CapsLock",
	vkEscape: "Esc", vkSpace: "Space", 0x21: "PgUp", 0x22: "PgDn", 0x23: "End", 0x24: "Home",
	0x25: "←", 0x26: "↑", 0x27: "→", 0x28: "↓", 0x2C: "PrtSc", vkInsert: "Insert", vkDelete: "Delete",
	vkLWin: "Left Win", vkRWin: "Right Win", vkApps: "Menu",
	0x6A: "Num *", 0x6B: "Num +", 0x6D: "Num -", 0x6E: "Num .", 0x6F: "Num /",
	vkNumLock: "NumLock", vkScroll: "ScrollLock",
	vkLShift: "Left Shift", vkRShift: "Right Shift", vkLControl: "Left Ctrl", vkRControl: "Right Ctrl",
	vkLMenu: "Left Alt", vkRMenu: "Right Alt",
	0xA6: "Browser Back", 0xA7: "Browser Forward", 0xA8: "Browser Refresh", 0xA9: "Browser Stop",
	0xAA: "Browser Search", 0xAB: "Favorites", 0xAC: "Browser Home",
	0xAD: "Mute", 0xAE: "Volume Down", 0xAF: "Volume Up", 0xB0: "Next Track", 0xB1: "Prev Track",
	0xB2: "Stop Media", 0xB3: "Play/Pause", 0xB4: "Mail", 0xB5: "Media", 0xB6: "App 1", 0xB7: "App 2",
	0xBA: ";", 0xBB: "=", 0xBC: ",", 0xBD: "-", 0xBE: ".", 0xBF: "/", 0xC0: "`",
	0xDB: "[", 0xDC: `\`, 0xDD: "]", 0xDE: "'", 0xE2: `\`,
}

func keyName(vk uint32) string {
	switch {
	case vk >= '0' && vk <= '9', vk >= 'A' && vk <= 'Z':
		return string(rune(vk))
	case vk >= 0x60 && vk <= 0x69:
		return fmt.Sprintf("Num %d", vk-0x60)
	case vk >= vkF1 && vk <= vkF24:
		return fmt.Sprintf("F%d", vk-vkF1+1)
	}
	if n, ok := keyNames[vk]; ok {
		return n
	}
	return fmt.Sprintf("Key %02X", vk)
}

// Parts lists the hotkey as keycaps, e.g. ["Ctrl", "Shift", "Space"].
func (h Hotkey) Parts() []string {
	if h.Key == 0 {
		return nil
	}
	var p []string
	if h.Ctrl {
		p = append(p, "Ctrl")
	}
	if h.Alt {
		p = append(p, "Alt")
	}
	if h.Shift {
		p = append(p, "Shift")
	}
	if h.Win {
		p = append(p, "Win")
	}
	return append(p, keyName(h.Key))
}

func (h Hotkey) String() string {
	if h.Key == 0 {
		return "не назначена"
	}
	return strings.Join(h.Parts(), " + ")
}

// standalone keys are safe to bind without a modifier: they never type text.
func standalone(vk uint32) bool {
	if _, ok := modifierBit(vk); ok {
		return true
	}
	switch {
	case vk >= vkF1 && vk <= vkF24, vk >= 0xA6 && vk <= 0xB7:
		return true
	}
	switch vk {
	case vkPause, vkScroll, vkInsert, vkApps, vkCapital, vkNumLock:
		return true
	}
	return false
}

// validate rejects bindings that would break normal typing.
func (h Hotkey) validate() error {
	switch {
	case h.Key == 0:
		return errors.New("клавиша не назначена")
	case h.Key == vkEscape:
		return errors.New("Esc занят: им отменяется запись")
	case h.mods() == 0 && !standalone(h.Key):
		return fmt.Errorf("«%s» без модификатора перестанет печататься. Добавьте Ctrl, Alt, Shift или Win — или выберите F1–F24, Pause, Insert, правый Ctrl/Alt", keyName(h.Key))
	}
	return nil
}

// ---------- key state machine ----------

type keyEvent int

const (
	evNone      keyEvent = iota
	evDown               // the hotkey was pressed
	evUp                 // the hotkey was released
	evInterrupt          // another key was pressed while a lone-modifier hotkey was held
	evEscape             // Esc while armed (during dictation)
)

// boundEvent is a hotkey event for one binding ("" for Esc).
type boundEvent struct {
	id string
	ev keyEvent
}

// bindSpec assigns a hotkey to an action id ("dictate", "listen", "tr:…").
type bindSpec struct {
	id string
	hk Hotkey
}

type binding struct {
	bindSpec
	active    bool // hotkey is held
	interrupt bool // lone-modifier hotkey was combined with another key
}

type hookResult struct {
	swallow  bool         // hide this keystroke from other apps
	events   []boundEvent //
	mask     bool         // inject a dummy key so a released Alt/Win doesn't open a menu
	captured *Hotkey      // capture finished with this hotkey
	capDone  bool         // capture finished (captured == nil means cancelled)
}

// keyMachine turns raw low-level keyboard events into hotkey events. It is
// pure logic; hotkey_windows.go feeds it from the keyboard hook.
type keyMachine struct {
	binds    []*binding
	down     map[uint32]bool // physical modifier keys currently held
	escArmed bool

	capturing bool
	capMods   map[uint32]bool // modifier keys pressed during capture
}

func newKeyMachine() *keyMachine {
	return &keyMachine{down: map[uint32]bool{}}
}

// setBindings replaces every hotkey; unset ones (Key 0) are skipped.
func (m *keyMachine) setBindings(specs []bindSpec) {
	m.binds = nil
	for _, s := range specs {
		if s.hk.Key != 0 {
			m.binds = append(m.binds, &binding{bindSpec: s})
		}
	}
}

func (m *keyMachine) mods() uint8 {
	var b uint8
	for vk, on := range m.down {
		if on {
			bit, _ := modifierBit(vk)
			b |= bit
		}
	}
	return b
}

// syncModifiers replaces the tracked modifier state, e.g. with the real one
// after keys were released on the secure desktop where the hook is blind.
func (m *keyMachine) syncModifiers(held []uint32) {
	clear(m.down)
	for _, vk := range held {
		m.down[vk] = true
	}
}

func (m *keyMachine) beginCapture() {
	m.capturing = true
	m.capMods = map[uint32]bool{}
	for _, b := range m.binds {
		b.active = false
	}
}

func (m *keyMachine) handle(vk uint32, isDown bool) (r hookResult) {
	vk = sideKey(vk)
	_, isMod := modifierBit(vk)
	if isMod {
		m.down[vk] = isDown
	}
	if m.capturing {
		return m.capture(vk, isDown, isMod)
	}
	if isDown && vk == vkEscape && m.escArmed {
		return hookResult{swallow: true, events: []boundEvent{{"", evEscape}}}
	}
	for _, b := range m.binds {
		ev, swallow, mask := m.step(b, vk, isDown)
		if ev != evNone {
			r.events = append(r.events, boundEvent{b.id, ev})
		}
		r.swallow = r.swallow || swallow
		r.mask = r.mask || mask
	}
	return r
}

// step advances one binding.
func (m *keyMachine) step(b *binding, vk uint32, isDown bool) (ev keyEvent, swallow, mask bool) {
	hk := b.hk
	if hk.modifierOnly() {
		// Modifiers are never swallowed, so Right Ctrl + C still copies.
		switch {
		case vk == hk.Key && isDown && !b.active:
			if m.mods()&^m.bitOf(vk) == 0 {
				b.active, b.interrupt = true, false
				ev = evDown
			}
		case vk == hk.Key && !isDown && b.active:
			b.active = false
			if !b.interrupt {
				ev = evUp
			}
		case isDown && b.active && !b.interrupt && vk != hk.Key:
			b.interrupt = true
			ev = evInterrupt
		}
		return
	}
	if vk != hk.Key {
		return
	}
	switch {
	case isDown && b.active: // auto-repeat
		swallow = true
	case isDown && m.mods() == hk.mods():
		b.active = true
		ev, swallow, mask = evDown, true, hk.Alt || hk.Win
	case !isDown && b.active:
		b.active = false
		ev, swallow = evUp, true
	}
	return
}

func (m *keyMachine) bitOf(vk uint32) uint8 {
	b, _ := modifierBit(vk)
	return b
}

// capture records the next combination. Everything is swallowed meanwhile so
// e.g. Win or Alt don't open menus. A lone modifier is taken on release when
// nothing else was pressed with it.
func (m *keyMachine) capture(vk uint32, isDown, isMod bool) hookResult {
	r := hookResult{swallow: true}
	switch {
	case isDown && isMod:
		m.capMods[vk] = true
	case isDown && vk == vkEscape && m.mods() == 0:
		m.capturing = false
		r.capDone = true
	case isDown:
		h := hotkeyFrom(m.mods(), vk)
		m.capturing = false
		r.capDone, r.captured = true, &h
	case isMod && m.mods() == 0:
		// All modifiers released without a main key.
		if len(m.capMods) == 1 {
			h := Hotkey{Key: vk}
			m.capturing = false
			r.capDone, r.captured = true, &h
		}
		clear(m.capMods)
	}
	return r
}
