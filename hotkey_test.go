package main

import (
	"strings"
	"testing"
)

type keyStep struct {
	vk   uint32
	down bool
}

func press(vks ...uint32) []keyStep {
	var s []keyStep
	for _, vk := range vks {
		s = append(s, keyStep{vk, true})
	}
	return s
}

func release(vks ...uint32) []keyStep {
	var s []keyStep
	for _, vk := range vks {
		s = append(s, keyStep{vk, false})
	}
	return s
}

func first(r hookResult) keyEvent {
	if len(r.events) == 0 {
		return evNone
	}
	return r.events[0].ev
}

func run(m *keyMachine, steps ...[]keyStep) (events []keyEvent, swallowed []uint32) {
	for _, group := range steps {
		for _, st := range group {
			r := m.handle(st.vk, st.down)
			for _, e := range r.events {
				events = append(events, e.ev)
			}
			if r.swallow {
				swallowed = append(swallowed, st.vk)
			}
		}
	}
	return
}

func TestComboPressAndRelease(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{{"d", Hotkey{Ctrl: true, Shift: true, Key: vkSpace}}})

	ev, sw := run(m, press(vkLControl, vkLShift, vkSpace, vkSpace, vkSpace), release(vkSpace, vkLShift, vkLControl))
	if len(ev) != 2 || ev[0] != evDown || ev[1] != evUp {
		t.Fatalf("events = %v, want [down up]", ev)
	}
	// The main key (including auto-repeat) never reaches the app; modifiers do.
	if len(sw) != 4 {
		t.Fatalf("swallowed %v, want Space ×4", sw)
	}
	for _, vk := range sw {
		if vk != vkSpace {
			t.Fatalf("swallowed modifier %x", vk)
		}
	}
}

func TestComboNeedsExactModifiers(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{{"d", Hotkey{Ctrl: true, Shift: true, Key: vkSpace}}})
	if ev, sw := run(m, press(vkLControl, vkSpace), release(vkSpace, vkLControl)); len(ev) != 0 || len(sw) != 0 {
		t.Fatalf("Ctrl+Space triggered: %v %v", ev, sw)
	}
	if ev, _ := run(m, press(vkRControl, vkRShift, vkLMenu, vkSpace), release(vkSpace)); len(ev) != 0 {
		t.Fatalf("Ctrl+Shift+Alt+Space triggered: %v", ev)
	}
	run(m, release(vkLMenu))
	if ev, _ := run(m, press(vkSpace), release(vkSpace)); len(ev) != 2 {
		t.Fatalf("right-hand Ctrl+Shift+Space did not trigger: %v", ev)
	}
}

func TestAltComboRequestsMenuMask(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{{"d", Hotkey{Alt: true, Key: 'Q'}}})
	m.handle(vkLMenu, true)
	if r := m.handle('Q', true); !r.mask || first(r) != evDown {
		t.Fatalf("Alt+Q: %+v, want down with mask", r)
	}
}

func TestLoneModifier(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{{"d", Hotkey{Key: vkRControl}}})
	if !(Hotkey{Key: vkRControl}).modifierOnly() {
		t.Fatal("Right Ctrl should be modifier-only")
	}

	ev, sw := run(m, press(vkRControl, vkRControl), release(vkRControl))
	if len(ev) != 2 || ev[0] != evDown || ev[1] != evUp || len(sw) != 0 {
		t.Fatalf("tap: events %v swallowed %v", ev, sw)
	}

	// Right Ctrl + C is a shortcut: interrupt, and no release event.
	ev, sw = run(m, press(vkRControl, 'C'), release('C', vkRControl))
	if len(ev) != 2 || ev[0] != evDown || ev[1] != evInterrupt || len(sw) != 0 {
		t.Fatalf("shortcut: events %v swallowed %v", ev, sw)
	}

	// With another modifier already held it is not the hotkey.
	if ev, _ := run(m, press(vkLShift, vkRControl), release(vkRControl, vkLShift)); len(ev) != 0 {
		t.Fatalf("Shift+RCtrl triggered: %v", ev)
	}
}

func TestEscapeWhenArmed(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{{"d", Hotkey{Ctrl: true, Key: 'M'}}})
	if r := m.handle(vkEscape, true); r.swallow || first(r) != evNone {
		t.Fatalf("Esc unarmed: %+v", r)
	}
	m.escArmed = true
	if r := m.handle(vkEscape, true); !r.swallow || first(r) != evEscape {
		t.Fatalf("Esc armed: %+v", r)
	}
}

func TestSyncModifiersRecoversFromMissedRelease(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{{"d", Hotkey{Ctrl: true, Key: 'M'}}})
	run(m, press(vkLMenu)) // released on the secure desktop: never seen
	m.syncModifiers([]uint32{vkLControl})
	if r := m.handle('M', true); first(r) != evDown {
		t.Fatalf("after sync: %+v", r)
	}
}

func TestCapture(t *testing.T) {
	m := newKeyMachine()

	m.beginCapture()
	var got *Hotkey
	for _, st := range append(press(vkLControl, vkLMenu, 'D'), release('D', vkLMenu, vkLControl)...) {
		r := m.handle(st.vk, st.down)
		if !r.swallow && st.down {
			t.Fatalf("key %x leaked during capture", st.vk)
		}
		if r.capDone {
			got = r.captured
		}
	}
	if got == nil || !got.Ctrl || !got.Alt || got.Shift || got.Key != 'D' {
		t.Fatalf("captured %+v, want Ctrl+Alt+D", got)
	}
	if m.capturing {
		t.Fatal("still capturing")
	}

	// A lone modifier is taken on release.
	m.beginCapture()
	run(m, press(vkRMenu))
	r := m.handle(vkRMenu, false)
	if !r.capDone || r.captured == nil || r.captured.Key != vkRMenu || r.captured.mods() != 0 {
		t.Fatalf("lone Right Alt: %+v", r)
	}

	// Two modifiers and nothing else: keep waiting.
	m.beginCapture()
	run(m, press(vkLControl, vkLShift))
	if r := m.handle(vkLShift, false); r.capDone {
		t.Fatalf("Ctrl+Shift alone captured: %+v", r)
	}
	run(m, release(vkLControl))
	if !m.capturing {
		t.Fatal("capture ended without a key")
	}

	// Esc cancels.
	if r := m.handle(vkEscape, true); !r.capDone || r.captured != nil {
		t.Fatalf("Esc: %+v", r)
	}
}

func TestValidate(t *testing.T) {
	ok := []Hotkey{
		{Ctrl: true, Shift: true, Key: vkSpace},
		{Key: vkF1 + 8},
		{Key: vkRControl},
		{Key: vkPause},
		{Win: true, Key: 'H'},
	}
	for _, h := range ok {
		if err := h.validate(); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	bad := []Hotkey{{}, {Key: 'A'}, {Key: vkSpace}, {Key: vkEscape}, {Ctrl: true, Key: vkEscape}}
	for _, h := range bad {
		if err := h.validate(); err == nil {
			t.Errorf("%+v accepted", h)
		}
	}
}

func TestHotkeyNames(t *testing.T) {
	cases := map[string]Hotkey{
		"Ctrl + Shift + Space": {Ctrl: true, Shift: true, Key: vkSpace},
		"Right Ctrl":           {Key: vkRControl},
		"Ctrl + Alt + F9":      {Ctrl: true, Alt: true, Key: vkF1 + 8},
		"Win + Num 5":          {Win: true, Key: 0x65},
	}
	for want, h := range cases {
		if got := h.String(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if !strings.Contains(Hotkey{}.String(), "не") {
		t.Error("empty hotkey should read as unassigned")
	}
}

func TestSeveralBindings(t *testing.T) {
	m := newKeyMachine()
	m.setBindings([]bindSpec{
		{"dictate", Hotkey{Ctrl: true, Shift: true, Key: vkSpace}},
		{"listen", Hotkey{Ctrl: true, Shift: true, Key: 'L'}},
		{"tr:1", Hotkey{Key: vkRMenu}},
		{"unset", Hotkey{}},
	})
	if len(m.binds) != 3 {
		t.Fatalf("%d bindings, unset ones must be skipped", len(m.binds))
	}
	m.handle(vkLControl, true)
	m.handle(vkLShift, true)
	r := m.handle('L', true)
	if len(r.events) != 1 || r.events[0] != (boundEvent{"listen", evDown}) || !r.swallow {
		t.Fatalf("Ctrl+Shift+L: %+v", r)
	}
	m.handle('L', false)
	m.handle(vkLShift, false)
	m.handle(vkLControl, false)

	// The lone Right Alt binding is interrupted by a combo that other bindings don't know.
	m.handle(vkRMenu, true)
	if r := m.handle('E', true); len(r.events) != 1 || r.events[0] != (boundEvent{"tr:1", evInterrupt}) || r.swallow {
		t.Fatalf("RAlt+E: %+v", r)
	}

	m.escArmed = true
	if r := m.handle(vkEscape, true); len(r.events) != 1 || r.events[0] != (boundEvent{"", evEscape}) {
		t.Fatalf("Esc: %+v", r)
	}
}
