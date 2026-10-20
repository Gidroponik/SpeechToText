package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResamplerConvertsRateAndKeepsTone(t *testing.T) {
	const src = 48000
	r := newResampler(src)
	// One second of a 440 Hz tone pushed in uneven pieces, as WASAPI does.
	tone := make([]float32, src)
	for i := range tone {
		tone[i] = float32(.5 * math.Sin(2*math.Pi*440*float64(i)/src))
	}
	for off := 0; off < len(tone); {
		n := min(len(tone)-off, 441+off%997)
		r.push(tone[off : off+n])
		off += n
	}
	var pcm []byte
	for _, c := range r.take(chunkBytes, true) {
		pcm = append(pcm, c...)
	}
	samples := len(pcm) / 2
	if samples < sampleRate-30 || samples > sampleRate+30 {
		t.Fatalf("%d output samples, want ~%d", samples, sampleRate)
	}
	// Count zero crossings: a 440 Hz tone crosses ~880 times a second.
	cross := 0
	prev := int16(binary.LittleEndian.Uint16(pcm))
	for i := 1; i < samples; i++ {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		if (prev < 0) != (v < 0) {
			cross++
		}
		prev = v
	}
	if cross < 860 || cross > 900 {
		t.Fatalf("%d zero crossings, want ~880", cross)
	}
	if rms := pcmRMS(pcm[2000:]); rms < .3 || rms > .4 {
		t.Fatalf("rms %v, want ~0.35 (amplitude kept)", rms)
	}
}

func TestResamplerChunks(t *testing.T) {
	r := newResampler(sampleRate)
	r.push(make([]float32, sampleRate/10*3+50)) // 300 ms + a bit
	if c := r.take(chunkBytes, false); len(c) != 3 {
		t.Fatalf("%d full chunks, want 3", len(c))
	}
	if c := r.take(chunkBytes, true); len(c) != 1 || len(c[0]) >= chunkBytes {
		t.Fatalf("flush: %d chunks", len(c))
	}
}

func TestToMono(t *testing.T) {
	// Stereo float32: L=0.5, R=-0.25 → 0.125.
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, math.Float32bits(.5))
	binary.LittleEndian.PutUint32(b[4:], math.Float32bits(-.25))
	if m := (sampleFormat{channels: 2, bits: 32, float: true}).toMono(b, nil); len(m) != 1 || m[0] != .125 {
		t.Fatalf("float stereo: %v", m)
	}
	// Mono 24-bit: 0x400000 = 0.5.
	if m := (sampleFormat{channels: 1, bits: 24}).toMono([]byte{0, 0, 0x40}, nil); len(m) != 1 || math.Abs(float64(m[0])-.5) > 1e-6 {
		t.Fatalf("24-bit: %v", m)
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		args []string
		want command
		ok   bool
	}{
		{[]string{"--toggle", "dictate"}, command{Op: "toggle", Action: "dictate"}, true},
		{[]string{"--start", "translate", "--to", "en", "--from", "ru"}, command{Op: "start", Action: "translate", To: "en", From: "ru"}, true},
		{[]string{"--toggle", "--to", "de"}, command{Op: "toggle", To: "de"}, true},
		{[]string{"--cancel"}, command{Op: "cancel"}, true},
		{[]string{"--autostart"}, command{}, false},
	}
	for _, c := range cases {
		got, ok := parseCommand(c.args)
		if ok != c.ok || got != c.want {
			t.Errorf("%v: got %+v %v", c.args, got, ok)
		}
	}
}

func TestResolveActions(t *testing.T) {
	cfg := defaultConfig()
	cfg.Listen.To = "ru"
	cfg.Translations = []TranslateBind{{ID: "a1", From: "ru", To: "en", Hotkey: Hotkey{Ctrl: true, Alt: true, Key: 'E'}}}

	if a, _ := resolveAction(cfg, "dictate", actionSpec{}); a.Source != "mic" || a.To != "" || a.Label() != "Диктовка" {
		t.Errorf("dictate: %+v", a)
	}
	if a, _ := resolveAction(cfg, "listen", actionSpec{}); a.Source != "system" || a.To != "ru" || a.Output != "show" || a.Label() != "Звук системы → RU" {
		t.Errorf("listen: %+v %q", a, a.Label())
	}
	if a, _ := resolveAction(cfg, "tr:a1", actionSpec{}); a.From != "ru" || a.To != "en" || a.Label() != "Перевод RU → EN" {
		t.Errorf("tr: %+v %q", a, a.Label())
	}
	if a, err := resolveAction(cfg, "translate", actionSpec{To: "de", Source: "system", Output: "clipboard"}); err != nil || a.Source != "system" || a.Output != "clipboard" {
		t.Errorf("ad hoc: %+v %v", a, err)
	}
	if _, err := resolveAction(cfg, "translate", actionSpec{To: "xx"}); err == nil {
		t.Error("unknown language accepted")
	}
	if _, err := resolveAction(cfg, "tr:missing", actionSpec{}); err == nil {
		t.Error("missing translation accepted")
	}
	if a, _ := resolveAction(cfg, "listen", actionSpec{}); a.languages(cfg) != nil {
		t.Error("system audio should not force the dictation languages")
	}

	// Hotkey ownership across actions.
	if owner := hotkeyOwner(cfg, Hotkey{Ctrl: true, Alt: true, Key: 'E'}, "dictate"); owner != "Перевод RU → EN" {
		t.Errorf("owner = %q", owner)
	}
	if owner := hotkeyOwner(cfg, Hotkey{Ctrl: true, Alt: true, Key: 'E'}, "tr:a1"); owner != "" {
		t.Errorf("an action conflicts with itself: %q", owner)
	}
	if !setActionHotkey(&cfg, "listen", Hotkey{Key: vkF1 + 9}) || cfg.Listen.Hotkey.Key != vkF1+9 {
		t.Error("setActionHotkey(listen)")
	}
	if list := actionList(cfg); len(list) != 3 || list[2].ID != "tr:a1" || list[2].Hotkey[2] != "E" {
		t.Errorf("actionList = %+v", list)
	}
}

func TestTranslateRequest(t *testing.T) {
	api := fakeResponses(t, "I'll be there at four.", func(req map[string]any) {
		ins := req["instructions"].(string)
		for _, want := range []string{"переводчик", "English (en)", "Русский", "НЕ тебе", "деловой"} {
			if !strings.Contains(ins, want) {
				t.Errorf("instructions lack %q", want)
			}
		}
	})
	res, err := translateText(t.Context(), api, "gpt-6-luna", "formal", "", nil, "ну я буду в четыре", "ru", "en")
	if err != nil || res.Text != "I'll be there at four." {
		t.Fatalf("%q %v", res.Text, err)
	}
}

func TestControlAPI(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	app := &App{store: &ConfigStore{path: filepath.Join(t.TempDir(), "c.json"), cfg: defaultConfig()}}
	app.dict = newDictation(app)
	c, err := startControl(app)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	var info struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	b, err := os.ReadFile(controlFile())
	if err != nil || json.Unmarshal(b, &info) != nil || info.Port != c.port || len(info.Token) != 64 {
		t.Fatalf("control.json: %s %v", b, err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
	do := func(method, path, body string, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	auth := map[string]string{"Authorization": "Bearer " + info.Token}

	if r := do("GET", "/v1/state", "", nil); r.StatusCode != 401 {
		t.Errorf("no token: %d", r.StatusCode)
	}
	if r := do("GET", "/v1/state", "", map[string]string{"Authorization": "Bearer " + info.Token, "Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Errorf("browser origin: %d", r.StatusCode)
	}
	if r := do("OPTIONS", "/v1/action", "", map[string]string{"Origin": "https://evil.example"}); r.StatusCode < 400 {
		t.Errorf("preflight: %d", r.StatusCode)
	}
	if r := do("GET", "/v1/state", "", auth); r.StatusCode != 200 {
		t.Errorf("state: %d", r.StatusCode)
	}
	if r := do("POST", "/v1/action", `{"op":"cancel"}`, auth); r.StatusCode != 200 {
		t.Errorf("cancel: %d", r.StatusCode)
	}
	if r := do("POST", "/v1/action", `{"op":"explode"}`, auth); r.StatusCode != 400 {
		t.Errorf("bad op: %d", r.StatusCode)
	}
	r := do("GET", "/v1/actions", "", auth)
	var acts struct {
		Actions []ActionInfo `json:"actions"`
	}
	if json.NewDecoder(r.Body).Decode(&acts) != nil || len(acts.Actions) != 2 || acts.Actions[0].ID != "dictate" {
		t.Errorf("actions: %+v", acts)
	}

	// Server-sent events: the current state first, then every change.
	ev := do("GET", "/v1/events", "", auth)
	defer ev.Body.Close()
	lines := bufio.NewScanner(ev.Body)
	next := func() LiveState {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				var s LiveState
				_ = json.Unmarshal([]byte(data), &s)
				return s
			}
		}
		t.Fatal("event stream ended")
		return LiveState{}
	}
	if s := next(); s.Phase != "idle" {
		t.Fatalf("first event %+v", s)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.publish(LiveState{Phase: "recording", Action: "listen", Source: "system", Since: 42})
	}()
	if s := next(); s.Phase != "recording" || s.Action != "listen" || s.Since != 42 {
		t.Fatalf("second event %+v", s)
	}
}

func TestResultOutputAndMigration(t *testing.T) {
	old := Config{Listen: ListenConfig{Output: "clipboard"}}
	old.migrate()
	if old.Listen.Output != "show" || old.Version != configVersion {
		t.Fatalf("migrated %+v", old)
	}
	chosen := Config{Version: configVersion, Listen: ListenConfig{Output: "clipboard"}}
	chosen.migrate()
	if chosen.Listen.Output != "clipboard" {
		t.Fatal("a choice made after the upgrade must stay")
	}

	cfg := defaultConfig()
	if a, _ := resolveAction(cfg, "listen", actionSpec{}); a.Output != "show" {
		t.Errorf("listen default output %q", a.Output)
	}
	if a, _ := resolveAction(cfg, "listen", actionSpec{Output: "insert"}); a.Output != "insert" {
		t.Errorf("Stream Deck override ignored: %q", a.Output)
	}
	if a, _ := resolveAction(cfg, "listen", actionSpec{Output: "bogus"}); a.Output != "show" {
		t.Errorf("bad override accepted: %q", a.Output)
	}
	if a, _ := resolveAction(cfg, "translate", actionSpec{To: "en", Output: "show"}); a.Output != "show" {
		t.Errorf("translate show: %q", a.Output)
	}
}

func TestSpanishFromSpain(t *testing.T) {
	p := buildTranslatePrompt("polish", "", nil, "ru", "es")
	for _, want := range []string{"Español (España) (es)", "Испании", "vosotros", "ordenador"} {
		if !strings.Contains(p, want) {
			t.Errorf("Spanish prompt lacks %q", want)
		}
	}
	if strings.Contains(buildTranslatePrompt("polish", "", nil, "ru", "en"), "vosotros") {
		t.Error("the Spanish variant leaked into English")
	}
}
