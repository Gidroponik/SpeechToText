package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Window geometry in CSS pixels, including the transparent margin around the
// card (--pad in frontend/dist/style.css): a 64px bar plus 10px on each side.
const (
	winW = 750
	barH = 84
)

// version is stamped at build time (-X main.version=1.2.3).
var version = "1.3.0"

type App struct {
	ctx       context.Context
	store     *ConfigStore
	history   *History
	hook      *keyHook
	overlay   *overlay
	dict      *dictation
	whisper   *localEngine
	win       nativeWindow
	control   *controlServer
	autostart bool    // launched by Windows at logon
	hidden    bool    // start in the tray (--hidden, or launched to run a command)
	pending   command // command-line action to run once the UI is up

	winMu    sync.Mutex // serialises window animations
	expanded bool
	barRect  rect

	capMu     sync.Mutex
	capturing bool
}

func NewApp(store *ConfigStore, hook *keyHook, ov *overlay, autostart bool) *App {
	a := &App{store: store, history: NewHistory(), hook: hook, overlay: ov, autostart: autostart}
	a.dict = newDictation(a)
	a.whisper = newLocalEngine(a)
	a.syncHotkeys()
	go func() {
		for ev := range hook.events {
			a.dict.onKey(ev)
		}
	}()
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go runTray(a)
	c, err := startControl(a)
	if err != nil {
		log.Printf("control API: %v", err)
	}
	a.control = c
	a.publishState(LiveState{Phase: "idle"})
}

// syncHotkeys hands every assigned hotkey to the keyboard hook.
func (a *App) syncHotkeys() {
	a.hook.SetBindings(hotkeyBindings(a.store.Get()))
}

// domReady places the collapsed bar and shows it, unless we were launched by
// autostart with "start minimized" enabled.
func (a *App) domReady(ctx context.Context) {
	cfg := a.store.Get()
	a.win.disableDwmCorners()
	r := a.win.centeredRect(winW, barH)
	a.win.setRect(r)
	a.barRect = r
	runtime.WindowSetAlwaysOnTop(ctx, cfg.Settings.AlwaysOnTop)
	if !(a.hidden || a.autostart && cfg.Settings.StartMinimized) {
		a.show()
	}
	if a.pending.Op != "" {
		go func() {
			if err := a.run(a.pending); err != nil {
				a.notify(err.Error())
			}
		}()
	}
	if debugHook != nil {
		debugHook(a)
	}
}

// debugHook is set only in debug builds (debug_windows.go).
var debugHook func(*App)

func (a *App) shutdown(ctx context.Context) {
	a.dict.Cancel()
	a.whisper.Stop()
	if a.control != nil {
		a.control.close()
	}
	quitTray()
}

// onSecondInstance runs a command-line action passed to a second copy of the
// exe (e.g. from a Stream Deck "Open" action), or brings the window up.
func (a *App) onSecondInstance(args []string) {
	if cmd, ok := parseCommand(args); ok {
		if err := a.run(cmd); err != nil {
			a.notify(err.Error())
		}
		return
	}
	a.show()
}

func (a *App) emit(name string, data any) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, name, data)
	}
}

func (a *App) notify(msg string) {
	log.Print(msg)
	a.emit("notice", msg)
}

// openPanel brings the window up on a panel, e.g. "api" when the key is missing.
func (a *App) openPanel(name string) {
	a.show()
	a.emit("open-panel", name)
}

// setDictationState tells the panel, the tray and Stream Deck what is going on.
func (a *App) setDictationState(phase string, spec actionSpec, since time.Time) {
	s := LiveState{Phase: phase, Action: spec.ID, Source: spec.Source, To: spec.To}
	if !since.IsZero() {
		s.Since = since.UnixMilli()
	}
	a.emit("dictation", s)
	setTrayRecording(phase == "recording")
	a.publishState(s)
}

func (a *App) publishState(s LiveState) {
	s.HasKey = a.store.Get().APIKey != ""
	if a.control != nil {
		a.control.publish(s)
	}
}

// apiConfig decrypts what is needed to call OpenAI.
func (a *App) apiConfig(cfg Config) (apiConfig, error) {
	key, err := openString(cfg.APIKey)
	if err != nil {
		return apiConfig{}, errors.New("не удалось расшифровать API-ключ (сменился пользователь Windows?) — сохраните ключ заново")
	}
	proxy, err := openString(cfg.Proxy)
	if err != nil {
		return apiConfig{}, errors.New("не удалось расшифровать адрес прокси — укажите его заново")
	}
	return apiConfig{Key: key, BaseURL: cfg.BaseURL, Proxy: proxy}, nil
}

// ---------- window ----------

// show brings the window back from the tray with a short slide-up.
func (a *App) show() {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	if a.win.isVisible() {
		runtime.WindowUnminimise(a.ctx)
		a.win.focus()
		return
	}
	r := a.win.rect()
	off := int32(14 * a.win.scale())
	a.win.setRect(rect{r.Left, r.Top + off, r.Right, r.Bottom + off})
	runtime.EventsEmit(a.ctx, "appear")
	runtime.WindowShow(a.ctx)
	a.win.focus()
	a.win.animate(r, 260*time.Millisecond)
}

func (a *App) toggleVisible() {
	if runtime.WindowIsNormal(a.ctx) && a.win.isVisible() {
		runtime.EventsEmit(a.ctx, "request-hide")
		return
	}
	a.show()
}

// Expand grows the window to height (CSS px), centred on screen. Used for the
// full panel and, with a smaller height, for dialogs.
func (a *App) Expand(height int) {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	if !a.expanded {
		a.barRect = a.win.rect()
		a.expanded = true
	}
	if target := a.win.centeredRect(winW, height); target != a.win.rect() {
		a.win.animate(target, 380*time.Millisecond)
	}
}

// Collapse shrinks the window back to the bar at its previous position.
func (a *App) Collapse() {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	if !a.expanded {
		return
	}
	a.expanded = false
	if !a.win.isVisible() {
		a.win.setRect(a.barRect)
		return
	}
	a.win.animate(a.barRect, 320*time.Millisecond)
}

// HideToTray hides the window; the tray icon brings it back.
func (a *App) HideToTray() {
	runtime.WindowHide(a.ctx)
}

func (a *App) Quit() {
	runtime.Quit(a.ctx)
}

// ---------- state ----------

type StateView struct {
	HasKey  bool     `json:"hasKey"`
	Offline bool     `json:"offline"` // dictation works without a key
	Hotkey  []string `json:"hotkey"`
	Mode    string   `json:"mode"`
	Version string   `json:"version"`
}

func (a *App) GetState() StateView {
	cfg := a.store.Get()
	return StateView{
		HasKey: cfg.APIKey != "", Offline: cfg.Local.Mic && cfg.Local.missing() == "",
		Hotkey: cfg.Hotkey.Parts(), Mode: cfg.Mode, Version: version,
	}
}

// ---------- dictation panel ----------

type RecordView struct {
	Hotkey []string `json:"hotkey"`
	Mode   string   `json:"mode"`
	MicID  string   `json:"micId"`
	Mics   []Mic    `json:"mics"`
}

func (a *App) GetRecordView() (RecordView, error) {
	cfg := a.store.Get()
	mics, err := listMics()
	if mics == nil {
		mics = []Mic{}
	}
	return RecordView{Hotkey: cfg.Hotkey.Parts(), Mode: cfg.Mode, MicID: cfg.MicID, Mics: mics}, err
}

type CaptureResult struct {
	Target    string   `json:"target"` // action id the hotkey is for
	Hotkey    []string `json:"hotkey"`
	Error     string   `json:"error"`
	Cancelled bool     `json:"cancelled"`
}

// CaptureHotkey records the next key combination pressed anywhere and
// assigns it to an action ("dictate", "listen", "tr:<id>"); the outcome
// arrives as a "hotkey-captured" event.
func (a *App) CaptureHotkey(target string) {
	a.capMu.Lock()
	if a.capturing {
		a.capMu.Unlock()
		return
	}
	a.capturing = true
	a.capMu.Unlock()

	a.hook.BeginCapture()
	go func() {
		var hk *Hotkey
		select {
		case hk = <-a.hook.captured:
		case <-time.After(15 * time.Second):
			a.hook.CancelCapture()
			<-a.hook.captured
		}
		a.capMu.Lock()
		a.capturing = false
		a.capMu.Unlock()

		res := CaptureResult{Target: target, Cancelled: hk == nil}
		if hk != nil {
			if err := a.assignHotkey(target, *hk); err != nil {
				res.Error = err.Error()
			} else {
				res.Hotkey = hk.Parts()
			}
		}
		a.syncHotkeys()
		setTrayHotkey(a.store.Get().Hotkey)
		a.emit("hotkey-captured", res)
	}()
}

func (a *App) assignHotkey(target string, hk Hotkey) error {
	if err := hk.validate(); err != nil {
		return err
	}
	if owner := hotkeyOwner(a.store.Get(), hk, target); owner != "" {
		return fmt.Errorf("«%s» уже занято: %s", hk, owner)
	}
	found := false
	err := a.store.Update(func(c *Config) { found = setActionHotkey(c, target, hk) })
	if err == nil && !found {
		err = errors.New("действие не найдено")
	}
	return err
}

// ClearHotkey unassigns an action's hotkey (it stays usable from Stream Deck).
func (a *App) ClearHotkey(target string) error {
	err := a.store.Update(func(c *Config) { setActionHotkey(c, target, Hotkey{}) })
	a.syncHotkeys()
	return err
}

func (a *App) CancelHotkeyCapture() {
	a.hook.CancelCapture()
}

// TestStart records from the panel ("mic", "system", or "local": the
// microphone through whisper); the result comes back as "test-result"
// instead of being typed into another app. False when recording didn't start.
func (a *App) TestStart(source string) bool {
	id := "dictate"
	if source == "system" {
		id = "listen"
	}
	spec, _ := resolveAction(a.store.Get(), id, actionSpec{})
	spec.Local = source == "local"
	a.dict.Start(phIdle, spec, true)
	a.dict.mu.Lock()
	defer a.dict.mu.Unlock()
	return a.dict.phase == phRecording && a.dict.test
}

func (a *App) TestStop() {
	a.dict.Stop()
}

// ---------- system audio panel ----------

type ListenView struct {
	Hotkey    []string   `json:"hotkey"`
	Device    string     `json:"device"`
	Devices   []Mic      `json:"devices"`
	Style     string     `json:"style"`
	To        string     `json:"to"`
	Output    string     `json:"output"`
	Languages []Language `json:"languages"`
}

func (a *App) GetListenView() (ListenView, error) {
	l := a.store.Get().Listen
	devs, err := listOutputs()
	return ListenView{Hotkey: l.Hotkey.Parts(), Device: l.Device, Devices: devs, Style: l.Style, To: l.To, Output: l.Output, Languages: languages}, err
}

// SetListenOption stores a system-audio setting: device, style, to, output.
func (a *App) SetListenOption(key, value string) error {
	switch key {
	case "style":
		if !slices.Contains(choices["style"], value) {
			return fmt.Errorf("недопустимый стиль %q", value)
		}
	case "to":
		if value != "" && !langKnown(value) {
			return fmt.Errorf("неизвестный язык %q", value)
		}
	case "output":
		if !validOutput(value) {
			return fmt.Errorf("недопустимое значение %q", value)
		}
	case "device":
	default:
		return fmt.Errorf("неизвестная настройка %q", key)
	}
	return a.store.Update(func(c *Config) {
		switch key {
		case "style":
			c.Listen.Style = value
		case "to":
			c.Listen.To = value
		case "output":
			c.Listen.Output = value
		case "device":
			c.Listen.Device = value
		}
	})
}

// ---------- translation panel ----------

type TranslationItem struct {
	ID     string   `json:"id"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Hotkey []string `json:"hotkey"`
}

type TranslationsView struct {
	Items     []TranslationItem `json:"items"`
	Languages []Language        `json:"languages"`
}

func (a *App) GetTranslations() TranslationsView {
	v := TranslationsView{Items: []TranslationItem{}, Languages: languages}
	for _, t := range a.store.Get().Translations {
		v.Items = append(v.Items, TranslationItem{ID: t.ID, From: t.From, To: t.To, Hotkey: t.Hotkey.Parts()})
	}
	return v
}

// AddTranslation creates a translation without a hotkey and returns its id.
func (a *App) AddTranslation(from, to string) (string, error) {
	if !langKnown(to) || (from != "" && !langKnown(from)) {
		return "", errors.New("выберите языки")
	}
	if from == to {
		return "", errors.New("языки совпадают")
	}
	b := make([]byte, 4)
	_, _ = cryptorand.Read(b)
	id := hex.EncodeToString(b)
	return id, a.store.Update(func(c *Config) {
		c.Translations = append(c.Translations, TranslateBind{ID: id, From: from, To: to})
	})
}

func (a *App) RemoveTranslation(id string) error {
	err := a.store.Update(func(c *Config) {
		c.Translations = slices.DeleteFunc(c.Translations, func(t TranslateBind) bool { return t.ID == id })
	})
	a.syncHotkeys()
	return err
}

// ---------- API panel ----------

type APIView struct {
	HasKey    bool     `json:"hasKey"`
	KeyHint   string   `json:"keyHint"`
	KeyError  string   `json:"keyError"`
	Delay     string   `json:"delay"`
	Languages []string `json:"languages"`
	Keywords  string   `json:"keywords"`
	BaseURL   string   `json:"baseUrl"`
	Proxy     string   `json:"proxy"` // password masked
	Model     string   `json:"model"`
	PerMin    float64  `json:"perMin"`
}

func keyHint(key string) string {
	if len(key) <= 8 {
		return "••••"
	}
	prefix := "sk-"
	if strings.HasPrefix(key, "sk-proj-") {
		prefix = "sk-proj-"
	}
	return prefix + "…" + key[len(key)-4:]
}

func maskProxy(p string) string {
	u, err := url.Parse(p)
	if err != nil || u.User == nil {
		return p
	}
	if _, has := u.User.Password(); !has {
		return p
	}
	u.User = url.User(u.User.Username())
	return strings.Replace(u.String(), "@", ":••••@", 1)
}

func (a *App) GetAPIView() APIView {
	cfg := a.store.Get()
	v := APIView{
		HasKey: cfg.APIKey != "", Delay: cfg.Delay, Languages: cfg.Languages,
		Keywords: strings.Join(cfg.Keywords, "\n"), BaseURL: cfg.BaseURL,
		Model: transcribeModel, PerMin: transcribePerMin,
	}
	if v.Languages == nil {
		v.Languages = []string{}
	}
	if key, err := openString(cfg.APIKey); err != nil {
		v.KeyError = "Ключ не расшифровывается под этим пользователем — сохраните его заново"
	} else if key != "" {
		v.KeyHint = keyHint(key)
	}
	if p, err := openString(cfg.Proxy); err == nil {
		v.Proxy = maskProxy(p)
	}
	return v
}

type KeySaved struct {
	Hint    string `json:"hint"`
	Warning string `json:"warning"`
}

// SaveAPIKey checks the key with OpenAI and stores it encrypted. A key that
// OpenAI rejects is not saved; one that can't be checked (no network, region
// block) is saved with a warning so a proxy can be set up afterwards.
func (a *App) SaveAPIKey(key string) (KeySaved, error) {
	key = strings.TrimSpace(key)
	if len(key) < 20 || strings.ContainsAny(key, " \t\r\n") {
		return KeySaved{}, errors.New("это не похоже на API-ключ OpenAI (sk-…)")
	}
	cfg := a.store.Get()
	api, _ := a.apiConfig(cfg)
	api.Key = key
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := KeySaved{Hint: keyHint(key)}
	if err := checkKey(ctx, api); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == 401 || ae.Code == "invalid_api_key") {
			return KeySaved{}, err
		}
		res.Warning = "Ключ сохранён, но проверить его не удалось: " + err.Error()
	}
	sealed, err := sealString(key)
	if err != nil {
		return KeySaved{}, fmt.Errorf("шифрование ключа: %w", err)
	}
	return res, a.store.Update(func(c *Config) { c.APIKey = sealed })
}

func (a *App) DeleteAPIKey() error {
	return a.store.Update(func(c *Config) { c.APIKey = "" })
}

// CheckAPI verifies the saved key, network settings and model access.
func (a *App) CheckAPI() (string, error) {
	api, err := a.apiConfig(a.store.Get())
	if err != nil {
		return "", err
	}
	if api.Key == "" {
		return "", errors.New("ключ не сохранён")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := checkKey(ctx, api); err != nil {
		return "", err
	}
	return "Ключ работает, модель " + transcribeModel + " доступна", nil
}

// SetNetwork stores the API address and proxy ("" = none).
func (a *App) SetNetwork(baseURL, proxy string) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("адрес API должен начинаться с https://")
	}
	proxy = strings.TrimSpace(proxy)
	cfg := a.store.Get()
	if strings.Contains(proxy, "••••") {
		// The masked value came back unchanged: keep the stored password.
		old, _ := openString(cfg.Proxy)
		if maskProxy(old) != proxy {
			return errors.New("введите пароль прокси заново")
		}
		proxy = old
	}
	if proxy != "" {
		if _, err := (apiConfig{Proxy: proxy}).proxyFunc(); err != nil {
			return err
		}
		// The WebSocket client (gorilla) supports exactly these two.
		if u, _ := url.Parse(proxy); !slices.Contains([]string{"http", "socks5"}, u.Scheme) {
			return errors.New("прокси: поддерживаются http:// и socks5://")
		}
	}
	sealed, err := sealString(proxy)
	if err != nil {
		return err
	}
	return a.store.Update(func(c *Config) { c.BaseURL, c.Proxy = baseURL, sealed })
}

// ---------- text panel ----------

type TextView struct {
	Style            string         `json:"style"`
	Model            string         `json:"model"`
	Models           []cleanupModel `json:"models"`
	CustomPrompt     string         `json:"customPrompt"`
	NoFinalDot       bool           `json:"noFinalDot"`
	Insert           string         `json:"insert"`
	RestoreClipboard bool           `json:"restoreClipboard"`
	PressEnter       bool           `json:"pressEnter"`
}

func (a *App) GetTextView() TextView {
	cfg := a.store.Get()
	return TextView{
		Style: cfg.Style, Model: findModel(cfg.Model).ID, Models: cleanupModels, CustomPrompt: cfg.CustomPrompt,
		NoFinalDot: cfg.NoFinalDot, Insert: cfg.Insert, RestoreClipboard: cfg.RestoreClipboard, PressEnter: cfg.PressEnter,
	}
}

var choices = map[string][]string{
	"mode":   {"toggle", "hold"},
	"delay":  {"minimal", "low", "medium", "high", "xhigh"},
	"style":  {"raw", "clean", "polish", "formal"},
	"insert": {"paste", "type", "clipboard"},
}

// SetOption stores a text setting.
func (a *App) SetOption(key, value string) error {
	if allowed, ok := choices[key]; ok && !slices.Contains(allowed, value) {
		return fmt.Errorf("недопустимое значение %q", value)
	}
	if key == "model" && findModel(value).ID != value {
		return fmt.Errorf("неизвестная модель %q", value)
	}
	return a.store.Update(func(c *Config) {
		switch key {
		case "mode":
			c.Mode = value
		case "delay":
			c.Delay = value
		case "style":
			c.Style = value
		case "insert":
			c.Insert = value
		case "model":
			c.Model = value
		case "mic":
			c.MicID = value
		case "customPrompt":
			c.CustomPrompt = strings.TrimSpace(value)
		case "keywords":
			c.Keywords = cleanKeywords(strings.Split(value, "\n"))
		case "languages":
			c.Languages = nil
			for _, l := range strings.Split(value, ",") {
				if l = strings.TrimSpace(l); langCode.MatchString(l) {
					c.Languages = append(c.Languages, l)
				}
			}
		}
	})
}

// SetFlag stores an on/off setting.
func (a *App) SetFlag(key string, on bool) error {
	switch key {
	case "autorun":
		return setAutorun(on)
	case "alwaysOnTop":
		runtime.WindowSetAlwaysOnTop(a.ctx, on)
	case "localMic", "localSystem":
		if msg := a.store.Get().Local.missing(); on && msg != "" {
			return errors.New(msg)
		}
	}
	return a.store.Update(func(c *Config) {
		switch key {
		case "noFinalDot":
			c.NoFinalDot = on
		case "restoreClipboard":
			c.RestoreClipboard = on
		case "pressEnter":
			c.PressEnter = on
		case "startMinimized":
			c.Settings.StartMinimized = on
		case "alwaysOnTop":
			c.Settings.AlwaysOnTop = on
		case "sounds":
			c.Settings.Sounds = on
		case "keepHistory":
			c.Settings.KeepHistory = on
		case "localMic":
			c.Local.Mic = on
		case "localSystem":
			c.Local.System = on
		}
	})
}

// ---------- offline panel ----------

type LocalModelView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Note        string `json:"note"`
	Size        int64  `json:"size"`
	Installed   bool   `json:"installed"`
	Recommended bool   `json:"recommended"` // for the device chosen
}

type LocalView struct {
	Mic      bool             `json:"mic"`
	System   bool             `json:"system"`
	Model    string           `json:"model"`
	GPU      bool             `json:"gpu"`
	Load     string           `json:"load"`
	Threads  map[string]int   `json:"threads"` // load → CPU threads
	Models   []LocalModelView `json:"models"`
	GPUName  string           `json:"gpuName"`  // "" without an NVIDIA card
	CUDA     bool             `json:"cuda"`     // the CUDA build is installed
	CUDASize int64            `json:"cudaSize"` // download
	CUDADisk int64            `json:"cudaDisk"` // unpacked
	Missing  string           `json:"missing"`  // what keeps it from running, "" = ready
	Download *DownloadView    `json:"download"`
	Loaded   bool             `json:"loaded"` // a model is in memory now
	Dir      string           `json:"dir"`
	Disk     int64            `json:"disk"` // bytes the files take
	HasKey   bool             `json:"hasKey"`
}

func (a *App) GetLocalView() LocalView {
	cfg := a.store.Get()
	l := cfg.Local
	if l.Load == "" {
		l.Load = "normal"
	}
	v := LocalView{
		Mic: l.Mic, System: l.System, Model: l.Model, GPU: l.GPU, Load: l.Load,
		Threads: map[string]int{"low": whisperThreads("low"), "normal": whisperThreads("normal"), "max": whisperThreads("max")},
		GPUName: nvidiaGPU(), CUDA: runtimeCUDA.installed(), CUDASize: runtimeCUDA.Size,
		Missing: l.missing(), Download: a.whisper.progress(), Loaded: a.whisper.srv.loaded() != "",
		Dir: whisperDir(), HasKey: cfg.APIKey != "",
	}
	for _, m := range whisperModels {
		installed := m.installed()
		rec := m.For == "gpu" && l.GPU || m.For == "cpu" && !l.GPU
		v.Models = append(v.Models, LocalModelView{ID: m.ID, Label: m.Label, Note: m.Note, Size: m.Size, Installed: installed, Recommended: rec})
		if installed {
			v.Disk += m.Size
		}
	}
	for _, p := range []whisperPackage{runtimeCPU, runtimeCUDA, vadModel} {
		if p.installed() {
			v.Disk += p.diskSize()
		}
	}
	if v.CUDA {
		v.CUDADisk = runtimeCUDA.diskSize()
	}
	return v
}

// LocalDownload fetches a model (target = its id) or the CUDA build ("cuda")
// in the background; progress comes as "local-progress" events and the end
// as "local-changed" (with an error message, or "").
func (a *App) LocalDownload(target string) error {
	return a.whisper.Download(target)
}

func (a *App) LocalCancelDownload() {
	a.whisper.CancelDownload()
}

// LocalRemove deletes a model or the CUDA build.
func (a *App) LocalRemove(target string) error {
	return a.whisper.Remove(target)
}

// LocalUnload frees the memory the model takes until the next dictation.
func (a *App) LocalUnload() error {
	if a.dict.busyLocal() {
		return errors.New("сейчас идёт распознавание")
	}
	a.whisper.srv.Stop()
	return nil
}

// SetLocalOption stores an offline setting: model, gpu (on | off), load.
func (a *App) SetLocalOption(key, value string) error {
	switch key {
	case "model":
		if m, ok := findWhisperModel(value); !ok || !m.installed() {
			return errors.New("сначала скачайте эту модель")
		}
	case "gpu":
		if value == "on" && !runtimeCUDA.installed() {
			return errors.New("сначала скачайте сборку для видеокарты")
		}
	case "load":
		if !slices.Contains([]string{"low", "normal", "max"}, value) {
			return fmt.Errorf("недопустимое значение %q", value)
		}
	default:
		return fmt.Errorf("неизвестная настройка %q", key)
	}
	return a.store.Update(func(c *Config) {
		switch key {
		case "model":
			c.Local.Model = value
		case "gpu":
			c.Local.GPU = value == "on"
		case "load":
			c.Local.Load = value
		}
	})
}

// ---------- history ----------

type HistoryView struct {
	Items []HistoryItem `json:"items"`
	Month Usage         `json:"month"`
	Keep  bool          `json:"keep"`
}

func (a *App) GetHistory() HistoryView {
	items := a.history.Items()
	if items == nil {
		items = []HistoryItem{}
	}
	return HistoryView{Items: items, Month: a.history.Month(time.Now()), Keep: a.store.Get().Settings.KeepHistory}
}

func (a *App) ClearHistory() {
	a.history.Clear()
}

// CopyText puts text from the history on the clipboard.
func (a *App) CopyText(text string) error {
	return setClipboardText(text, false)
}

// ---------- settings ----------

type SettingsView struct {
	Settings Settings `json:"settings"`
	Autorun  bool     `json:"autorun"`
	Version  string   `json:"version"`
	Deck     DeckView `json:"deck"`
}

func (a *App) GetSettings() SettingsView {
	return SettingsView{Settings: a.store.Get().Settings, Autorun: autorunEnabled(), Version: version, Deck: deckStatus()}
}

// ---------- result window ----------

// ResultView is a finished transcript shown in the panel (system audio by
// default), on top of everything, to read and copy.
type ResultView struct {
	Title     string  `json:"title"`
	Text      string  `json:"text"`
	Raw       string  `json:"raw"` // the recognised original, when it differs
	Seconds   float64 `json:"seconds"`
	Note      string  `json:"note"`
	HideAfter bool    `json:"hideAfter"` // the window was in the tray: go back there on close
}

func (a *App) showResult(r ResultView) {
	r.HideAfter = !a.win.isVisible()
	runtime.WindowSetAlwaysOnTop(a.ctx, true)
	a.show()
	a.emit("show-result", r)
}

// ResultClosed restores the usual always-on-top setting.
func (a *App) ResultClosed() {
	runtime.WindowSetAlwaysOnTop(a.ctx, a.store.Get().Settings.AlwaysOnTop)
}
