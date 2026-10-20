package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

// dictation runs one take at a time: record (microphone or system audio) and
// stream to gpt-live-transcribe → clean up or translate the text → insert it
// where the cursor is or copy it.

type dictPhase int

const (
	phIdle dictPhase = iota
	phArming
	phRecording
	phProcessing
)

const (
	armDelay       = 220 * time.Millisecond // a lone modifier must be held this long to count as push-to-talk
	minRecording   = 400 * time.Millisecond
	maxMicTake     = 10 * time.Minute
	maxSystemTake  = 55 * time.Minute // a Realtime session lasts at most an hour
	maxRetryBuffer = sampleRate * 2 * 60 * 20
	silencePeak    = 0.004  // RMS: a take that never gets louder had a muted microphone
	systemSilence  = 0.0005 // system audio is only "silent" when nothing played at all
)

// audioSource is a running capture: the microphone or system audio.
type audioSource interface{ Stop() }

type dictation struct {
	app *App

	mu        sync.Mutex
	phase     dictPhase
	spec      actionSpec
	test      bool // started from the panel: the result goes there, not into an app
	armTimer  *time.Timer
	autoStop  *time.Timer
	src       audioSource
	live      *liveSession
	capturing bool
	pcm       []byte // kept for one retry if the live connection breaks
	total     int    // audio bytes captured
	seg       segmenter
	peak      float64
	started   time.Time
	target    windows.HWND
	cancel    context.CancelFunc
	cfg       Config
	api       apiConfig
}

func newDictation(app *App) *dictation {
	return &dictation{app: app}
}

// onKey maps a hotkey event to its action, honouring the push-to-talk mode
// for microphone actions (system audio always toggles).
func (d *dictation) onKey(e boundEvent) {
	if e.ev == evEscape {
		d.Cancel()
		return
	}
	cfg := d.app.store.Get()
	spec, err := resolveAction(cfg, e.id, actionSpec{})
	if err != nil {
		return
	}
	var hk Hotkey
	for _, b := range hotkeyBindings(cfg) {
		if b.id == e.id {
			hk = b.hk
		}
	}
	lone := hk.modifierOnly()
	hold := cfg.Mode == "hold" && spec.Source == "mic"
	switch e.ev {
	case evInterrupt: // the lone modifier was part of an ordinary shortcut
		d.mu.Lock()
		ph := d.phase
		d.mu.Unlock()
		if ph == phArming {
			d.disarm()
		} else if hold && ph == phRecording {
			d.Cancel()
		}
	case evDown:
		switch {
		case lone && hold:
			d.arm(spec)
		case lone: // toggles on release, once it is clear it wasn't a shortcut
		case hold:
			d.Start(phIdle, spec, false)
		default:
			d.Toggle(spec)
		}
	case evUp:
		switch {
		case lone && hold:
			if !d.disarm() {
				d.Stop()
			}
		case lone:
			d.Toggle(spec)
		case hold:
			d.Stop()
		}
	}
}

// Toggle starts spec when idle and stops whatever is recording.
func (d *dictation) Toggle(spec actionSpec) {
	d.mu.Lock()
	ph := d.phase
	d.mu.Unlock()
	switch ph {
	case phIdle:
		d.Start(phIdle, spec, false)
	case phRecording:
		d.Stop()
	}
}

func (d *dictation) arm(spec actionSpec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.phase != phIdle {
		return
	}
	d.phase = phArming
	d.armTimer = time.AfterFunc(armDelay, func() { d.Start(phArming, spec, false) })
}

// disarm drops a pending push-to-talk start; false when there was none.
func (d *dictation) disarm() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.phase != phArming {
		return false
	}
	d.armTimer.Stop()
	d.phase = phIdle
	return true
}

// Start begins recording if the dictation is in the expected phase.
func (d *dictation) Start(expect dictPhase, spec actionSpec, test bool) {
	ov := d.app.overlay
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.phase != expect {
		return
	}
	d.phase = phIdle

	cfg := d.app.store.Get()
	api, err := d.app.apiConfig(cfg)
	if err != nil || api.Key == "" {
		msg := "Добавьте API-ключ OpenAI в разделе «API»"
		if err != nil {
			msg = err.Error()
		}
		d.fail(msg)
		d.app.openPanel("api")
		return
	}

	d.cfg, d.api, d.spec, d.test = cfg, api, spec, test
	d.target = foregroundWindow()
	d.pcm = make([]byte, 0, sampleRate*2*10) // the previous take's buffer may be huge: let it go
	d.total, d.peak = 0, 0
	d.seg = segmenter{}
	d.started = time.Now()
	d.live = startLiveSession(api, transcribeOptions{Delay: cfg.Delay, Languages: spec.languages(cfg), Keywords: cfg.Keywords}, d.onPartial)
	d.capturing = true

	var src audioSource
	var found bool
	limit := maxMicTake
	if spec.Source == "system" {
		limit = maxSystemTake
		var lb *loopback
		if lb, found, err = startLoopback(cfg.Listen.Device, d.onData); err == nil {
			src = lb
		}
	} else {
		var rec *recorder
		if rec, found, err = startRecorder(cfg.MicID, d.onData); err == nil {
			src = rec
		}
	}
	if err != nil {
		d.capturing = false
		d.live.Close()
		d.live = nil
		if spec.Source == "system" {
			d.fail("Звук системы: " + err.Error())
		} else {
			d.fail("Микрофон: " + err.Error())
		}
		return
	}
	if !found {
		if spec.Source == "system" {
			d.app.notify("Выбранное устройство вывода не найдено — записывается устройство по умолчанию")
		} else {
			d.app.notify("Выбранный микрофон не найден — запись идёт с микрофона по умолчанию")
		}
	}
	d.src = src
	d.phase = phRecording
	d.autoStop = time.AfterFunc(limit, d.Stop)

	d.app.hook.SetEscArmed(true)
	// The start cue would end up in a system-audio recording, so it only plays for the microphone.
	if cfg.Settings.Sounds && spec.Source == "mic" {
		playSound(soundStart)
	}
	ov.Show(overlayFrame{Kind: ovListening, Text: spec.placeholder(), Dim: true, Clock: d.started}, 0)
	d.app.setDictationState("recording", spec, d.started)
}

// languages is the spoken-language hint for transcription.
func (a actionSpec) languages(cfg Config) []string {
	switch {
	case a.From != "":
		return []string{a.From}
	case a.Source == "system":
		return nil // videos and calls can be in any language
	}
	return cfg.Languages
}

func (a actionSpec) placeholder() string {
	switch {
	case a.Source == "system":
		return "Слушаю звук системы…"
	case a.To != "":
		return "Говорите — переведу на " + langNative(a.To) + "…"
	}
	return "Говорите…"
}

// fail reports a problem that stopped a take before it started.
func (d *dictation) fail(msg string) {
	if d.app.store.Get().Settings.Sounds {
		playSound(soundError)
	}
	d.app.overlay.Show(overlayFrame{Kind: ovError, Text: msg}, 4500*time.Millisecond)
	d.app.notify(msg)
}

func (d *dictation) onData(pcm []byte) {
	rms := pcmRMS(pcm)
	d.mu.Lock()
	if !d.capturing {
		d.mu.Unlock()
		return
	}
	if len(d.pcm)+len(pcm) <= maxRetryBuffer {
		d.pcm = append(d.pcm, pcm...)
	}
	d.total += len(pcm)
	d.peak = math.Max(d.peak, rms)
	commit := d.seg.push(rms, len(pcm))
	live, test := d.live, d.test
	d.mu.Unlock()

	live.Append(pcm)
	if commit {
		live.Commit()
	}
	level := levelOf(rms)
	d.app.overlay.Update(func(f *overlayFrame) {
		if f.Kind == ovListening {
			f.Level = level
		}
	})
	if test {
		d.app.emit("test-level", level)
	}
}

func (d *dictation) onPartial(text string) {
	if text = strings.TrimSpace(text); text == "" {
		return
	}
	d.app.overlay.Update(func(f *overlayFrame) {
		if f.Kind == ovListening {
			f.Text, f.Dim = text, false
		}
	})
}

// Stop ends recording and processes the take in the background.
func (d *dictation) Stop() {
	d.mu.Lock()
	if d.phase != phRecording {
		d.mu.Unlock()
		return
	}
	d.phase = phProcessing
	d.autoStop.Stop()
	src := d.src
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.mu.Unlock()

	src.Stop() // delivers the last chunk through onData

	d.mu.Lock()
	d.capturing, d.src = false, nil
	job := take{
		live: d.live, pcm: d.pcm, total: d.total, peak: d.peak, tail: d.seg.speech,
		spec: d.spec, cfg: d.cfg, api: d.api, test: d.test, target: d.target,
	}
	if d.total > len(d.pcm) {
		job.pcm = nil // too long to resend: no retry
	}
	d.mu.Unlock()
	if job.cfg.Settings.Sounds {
		playSound(soundStop)
	}
	d.app.setDictationState("processing", job.spec, time.Time{})
	go d.process(ctx, job)
}

// Cancel aborts recording or processing; nothing is inserted.
func (d *dictation) Cancel() {
	d.mu.Lock()
	switch d.phase {
	case phArming:
		d.armTimer.Stop()
		d.phase = phIdle
		d.mu.Unlock()
		return
	case phRecording:
		d.phase = phProcessing
		d.autoStop.Stop()
		src, live := d.src, d.live
		d.mu.Unlock()
		src.Stop()
		live.Close()
		d.mu.Lock()
		d.capturing, d.src = false, nil
		d.mu.Unlock()
		d.finish()
	case phProcessing:
		if d.cancel != nil {
			d.cancel()
		}
		d.mu.Unlock()
	default:
		d.mu.Unlock()
		return
	}
	d.app.overlay.Show(overlayFrame{Kind: ovInfo, Text: "Отменено"}, 900*time.Millisecond)
}

func (d *dictation) finish() {
	d.mu.Lock()
	d.phase, d.cancel, d.live = phIdle, nil, nil
	d.mu.Unlock()
	d.app.hook.SetEscArmed(false)
	d.app.setDictationState("idle", actionSpec{}, time.Time{})
}

type take struct {
	live   *liveSession
	pcm    []byte // nil when the take was too long to keep for a retry
	total  int
	peak   float64
	tail   bool // the audio after the last commit contains speech
	spec   actionSpec
	cfg    Config
	api    apiConfig
	test   bool
	target windows.HWND
}

// TestResult is what the panel's test recording shows.
type TestResult struct {
	Raw   string `json:"raw"`
	Text  string `json:"text"`
	Note  string `json:"note"`
	Error string `json:"error"`
}

func (d *dictation) process(ctx context.Context, t take) {
	ov := d.app.overlay
	defer d.finish()
	defer t.live.Close()

	failed := func(msg string) {
		if ctx.Err() != nil {
			return
		}
		if t.cfg.Settings.Sounds {
			playSound(soundError)
		}
		ov.Show(overlayFrame{Kind: ovError, Text: msg}, 4500*time.Millisecond)
		if t.test {
			d.app.emit("test-result", TestResult{Error: msg})
		} else {
			d.app.notify(msg)
		}
	}
	info := func(msg string, dur time.Duration) {
		if ctx.Err() != nil {
			return // cancelled: the overlay already says so
		}
		ov.Show(overlayFrame{Kind: ovInfo, Text: msg}, dur)
		if t.test {
			d.app.emit("test-result", TestResult{Error: msg})
		}
	}

	system := t.spec.Source == "system"
	length := audioDuration(t.total)
	switch {
	case system && t.peak < systemSilence:
		info("Звук не воспроизводился — включите видео или созвон и попробуйте снова", 3*time.Second)
		return
	case length < minRecording:
		info("Слишком коротко", 1200*time.Millisecond)
		return
	case t.peak < silencePeak:
		failed("Микрофон молчит — проверьте устройство и доступ: Параметры → Конфиденциальность → Микрофон")
		return
	}

	ov.Show(overlayFrame{Kind: ovWorking, Text: "Распознаю…"}, 0)
	opts := transcribeOptions{Delay: t.cfg.Delay, Languages: t.spec.languages(t.cfg), Keywords: t.cfg.Keywords}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second+length/10)
	raw, err := t.live.Finish(fctx, t.tail)
	cancel()
	var ae *apiError
	if err != nil && ctx.Err() == nil && !errors.As(err, &ae) && t.pcm != nil {
		// The live connection broke (network hiccup, VPN switch…): the whole
		// recording is still here, so send it once more in one piece.
		ov.Update(func(f *overlayFrame) { f.Text = "Повторная попытка…" })
		rctx, cancel := context.WithTimeout(ctx, 60*time.Second+length/2)
		raw, err = transcribeAll(rctx, t.api, opts, t.pcm)
		cancel()
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		failed(err.Error())
		return
	}
	if raw = strings.TrimSpace(raw); raw == "" {
		info("Речь не распознана", 1600*time.Millisecond)
		return
	}

	cost := length.Minutes() * transcribePerMin
	text, note := raw, ""
	style := t.spec.Style
	if style == "" {
		style = t.cfg.Style
	}
	if t.spec.To != "" || style != "raw" {
		label, verb := "Улучшаю текст…", cleanupText
		if t.spec.To != "" {
			label = "Перевожу на " + langNative(t.spec.To) + "…"
			verb = func(ctx context.Context, api apiConfig, model, style, custom string, kw []string, raw string) (cleanupResult, error) {
				return translateText(ctx, api, model, style, custom, kw, raw, t.spec.From, t.spec.To)
			}
		}
		ov.Update(func(f *overlayFrame) { f.Text = label })
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second+time.Duration(utf8.RuneCountInString(raw))*10*time.Millisecond)
		res, err := verb(cctx, t.api, t.cfg.Model, style, t.cfg.CustomPrompt, t.cfg.Keywords, raw)
		cancel()
		cost += res.Cost
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil && t.spec.To != "":
			note = "Перевод не удался, вставлен исходный текст: " + err.Error()
		case err != nil:
			note = "Текст без обработки: " + err.Error()
		default:
			text = res.Text
		}
	}
	if t.cfg.NoFinalDot && !system {
		text = dropFinalDot(text)
	}

	outcome, shown := "Готово", false
	switch {
	case t.test:
		d.app.emit("test-result", TestResult{Raw: raw, Text: text, Note: note})
	case ctx.Err() != nil:
		return
	case t.spec.Output == "show":
		d.app.showResult(ResultView{Title: t.spec.Label(), Text: text, Raw: raw, Seconds: length.Seconds(), Note: note})
		outcome, shown = "Текст готов", true // the window carries the note
	case t.spec.Output == "clipboard":
		if err := setClipboardText(text, false); err != nil {
			failed("Не удалось скопировать текст: " + err.Error())
			return
		}
		outcome = "Скопировано в буфер"
	default:
		outcome, err = insertText(text, insertOptions{Mode: t.cfg.Insert, RestoreClipboard: t.cfg.RestoreClipboard, PressEnter: t.cfg.PressEnter}, t.target)
		if err != nil {
			failed("Не удалось вставить текст: " + err.Error())
			return
		}
	}
	switch {
	case note != "" && !shown:
		ov.Show(overlayFrame{Kind: ovInfo, Text: note}, 4*time.Second)
		d.app.notify(note)
	case len([]rune(outcome)) > 24:
		ov.Show(overlayFrame{Kind: ovInfo, Text: outcome}, 4*time.Second)
		d.app.notify(outcome)
	default:
		ov.Show(overlayFrame{Kind: ovDone, Text: outcome}, 1100*time.Millisecond)
	}

	kind := ""
	if t.spec.ID != "dictate" {
		kind = t.spec.Label()
	}
	d.app.history.Add(HistoryItem{Time: time.Now(), Kind: kind, Raw: raw, Text: text, Seconds: length.Seconds(), Cost: cost, Note: note}, t.cfg.Settings.KeepHistory)
	d.app.emit("history-changed", nil)
}

// ---------- audio helpers ----------

func audioDuration(bytes int) time.Duration {
	return time.Duration(bytes/2) * time.Second / sampleRate
}

func pcmRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := range n {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// levelOf maps RMS to a 0..1 meter value on a dB scale (-58..-14 dBFS).
func levelOf(rms float64) float64 {
	db := 20 * math.Log10(rms+1e-9)
	return math.Min(1, math.Max(0, (db+58)/44))
}

func fmtDuration(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// segmenter is a small client-side VAD. gpt-live-transcribe has no server
// turn detection, so long dictations are committed in pieces at natural
// pauses: each piece is transcribed while the user keeps talking and the end
// of a long take arrives as fast as a short one.
type segmenter struct {
	floor  float64 // noise floor (RMS): follows dips at once, rises slowly
	voice  float64 // typical speech level (RMS)
	bytes  int     // audio since the last commit
	quiet  int     // trailing quiet audio
	speech bool    // current segment contains speech
}

const (
	segMinBytes   = sampleRate * 2 * 15     // never split before 15 s
	segQuietBytes = sampleRate * 2 * 7 / 10 // … and only at a 0.7 s pause
	segMaxBytes   = sampleRate * 2 * 120    // hard cap for a pause-less monologue
)

// push accounts for a chunk and reports whether to commit after it.
func (s *segmenter) push(rms float64, n int) bool {
	if s.floor == 0 || rms < s.floor {
		s.floor = math.Max(rms, 1e-4)
	} else {
		s.floor += (rms - s.floor) * .002
	}
	if s.voice > 0 {
		// Steady loud speech must never become the "noise floor".
		s.floor = math.Min(s.floor, s.voice*.25)
	}
	s.bytes += n
	switch {
	case rms > math.Max(s.floor*3, .006):
		s.speech, s.quiet = true, 0
		if s.voice == 0 {
			s.voice = rms
		}
		s.voice += (rms - s.voice) * .1
	case rms < math.Max(s.floor*2, .004) && (s.voice == 0 || rms < s.voice*.35):
		s.quiet += n
	default:
		s.quiet = 0
	}
	if s.speech && (s.bytes >= segMinBytes && s.quiet >= segQuietBytes || s.bytes >= segMaxBytes) {
		s.bytes, s.quiet, s.speech = 0, 0, false
		return true
	}
	return false
}
