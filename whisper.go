package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Offline recognition with whisper.cpp. Nothing of it ships in the exe: on
// request the app downloads the official whisper.cpp build (for the CPU, or
// with CUDA for NVIDIA cards) and a model, checks their SHA-256 and keeps
// them in %LOCALAPPDATA%\SpeechToText\whisper. whisper-server then runs on
// 127.0.0.1 only while it is needed (whisper_windows.go).

// whisperRelease pins the whisper.cpp build (v1.9.5). The hashes below are
// the ones GitHub and Hugging Face publish for these files.
const whisperRelease = "b5454"

// whisperPackage is one downloadable piece: a runtime (an archive unpacked
// into a folder) or a model file.
type whisperPackage struct {
	ID     string // cpu | cuda | vad | a model id
	Label  string
	File   string // the runtime folder or the model file name
	URL    string
	Size   int64
	SHA256 string
}

func releaseAsset(name string) string {
	return "https://github.com/ggml-org/whisper.cpp/releases/download/" + whisperRelease + "/" + name
}

func hfFile(repo, name string) string {
	return "https://huggingface.co/" + repo + "/resolve/main/" + name
}

var (
	runtimeCPU = whisperPackage{ID: "cpu", Label: "whisper.cpp для процессора", File: "cpu",
		URL: releaseAsset("whisper-bin-x64.zip"), Size: 8928640,
		SHA256: "6ba69e3482d7826214f90a6a9c84ca07782aec1e1d0c6a7c30c994fd5d816ccb"}
	runtimeCUDA = whisperPackage{ID: "cuda", Label: "whisper.cpp для видеокарты NVIDIA", File: "cuda",
		URL: releaseAsset("whisper-bin-win-cuda-12.4.0-x64.zip"), Size: 684913404,
		SHA256: "afef0b881c500958921c3f5523b50e59ee2ec9b6f5cbd25b324c51ed308a957a"}
	// Silero VAD: whisper skips silence and music instead of inventing text
	// for them, and long takes are no longer cut mid-sentence at 30 s.
	vadModel = whisperPackage{ID: "vad", Label: "Детектор речи", File: "ggml-silero-v6.2.0.bin",
		URL: hfFile("ggml-org/whisper-vad", "ggml-silero-v6.2.0.bin"), Size: 885098,
		SHA256: "2aa269b785eeb53a82983a20501ddf7c1d9c48e33ab63a41391ac6c9f7fb6987"}
)

type whisperModel struct {
	whisperPackage
	Note string
	For  string // "gpu" | "cpu": the model to recommend for that device
	Beam int    // beam search width; 0 = greedy. Smaller models misspell names without it
}

var whisperModels = []whisperModel{
	{whisperPackage{ID: "large-v3-turbo-q5_0", Label: "Large v3 Turbo", File: "ggml-large-v3-turbo-q5_0.bin",
		URL: hfFile("ggerganov/whisper.cpp", "ggml-large-v3-turbo-q5_0.bin"), Size: 574041195,
		SHA256: "394221709cd5ad1f40c46e6031ca61bce88931e6e088c188294c6d5a55ffa7e2"},
		"Лучшее качество: пунктуация, «ё», имена и термины. На видеокарте — доли секунды, на процессоре медленно", "gpu", 0},
	{whisperPackage{ID: "small-q5_1", Label: "Small", File: "ggml-small-q5_1.bin",
		URL: hfFile("ggerganov/whisper.cpp", "ggml-small-q5_1.bin"), Size: 190085487,
		SHA256: "ae85e4a935d7a567bd102fe55afc16bb595bdb618e11b2fc7591bc08120411bb"},
		"Баланс для процессора: быстро, но чаще ошибается в именах и терминах", "cpu", 5},
	{whisperPackage{ID: "base-q5_1", Label: "Base", File: "ggml-base-q5_1.bin",
		URL: hfFile("ggerganov/whisper.cpp", "ggml-base-q5_1.bin"), Size: 59707625,
		SHA256: "422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898"},
		"Самая лёгкая и быстрая, ошибок заметно больше — для слабых компьютеров", "", 5},
}

func findWhisperModel(id string) (whisperModel, bool) {
	for _, m := range whisperModels {
		if m.ID == id {
			return m, true
		}
	}
	return whisperModel{}, false
}

func whisperDir() string {
	dir, err := os.UserCacheDir() // %LOCALAPPDATA%: big files that needn't roam
	if err != nil {
		return filepath.Join(configDir(), "whisper")
	}
	return filepath.Join(dir, "SpeechToText", "whisper")
}

func (p whisperPackage) isRuntime() bool { return p.ID == "cpu" || p.ID == "cuda" }

// path is where the installed package lives.
func (p whisperPackage) path() string {
	if p.isRuntime() {
		return filepath.Join(whisperDir(), p.File)
	}
	return filepath.Join(whisperDir(), "models", p.File)
}

// installed reports whether the package is on disk and complete. Hashes are
// checked when downloading; a model of the right size is taken as is.
func (p whisperPackage) installed() bool {
	if p.isRuntime() {
		b, err := os.ReadFile(filepath.Join(p.path(), "release.txt"))
		return err == nil && strings.TrimSpace(string(b)) == whisperRelease
	}
	st, err := os.Stat(p.path())
	return err == nil && st.Size() == p.Size
}

// diskSize is what the installed package takes on disk.
func (p whisperPackage) diskSize() int64 {
	var n int64
	_ = filepath.WalkDir(p.path(), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// LocalConfig is offline recognition with whisper.cpp.
type LocalConfig struct {
	Mic    bool   `json:"mic"`    // dictation and translations
	System bool   `json:"system"` // system audio
	Model  string `json:"model"`  // a whisperModels id
	GPU    bool   `json:"gpu"`    // the CUDA build instead of the CPU one
	Load   string `json:"load"`   // CPU threads: low | normal | max

	// SetupDone: the first-run setup for a user without an API key has run
	// (or was cancelled), so it isn't started again.
	SetupDone bool `json:"setupDone"`
}

// needsSetup: there is no key to use OpenAI and offline recognition hasn't
// been set up, so the app should get it going on its own.
func needsSetup(cfg Config) bool {
	return cfg.APIKey == "" && !cfg.Local.SetupDone && cfg.Local.missing() != ""
}

func (l LocalConfig) usedFor(source string) bool {
	if source == "system" {
		return l.System
	}
	return l.Mic
}

func (l LocalConfig) runtime() whisperPackage {
	if l.GPU {
		return runtimeCUDA
	}
	return runtimeCPU
}

// missing says what keeps offline recognition from running ("" = ready).
func (l LocalConfig) missing() string {
	m, ok := findWhisperModel(l.Model)
	switch {
	case !ok && slices.ContainsFunc(whisperModels, func(m whisperModel) bool { return m.installed() }):
		return "Выберите модель Whisper в разделе «Офлайн»"
	case !ok || !m.installed():
		return "Модель Whisper не скачана — откройте раздел «Офлайн»"
	case !l.runtime().installed():
		return "Не скачан " + l.runtime().Label + " — откройте раздел «Офлайн»"
	}
	return ""
}

// label names the engine in the history and the test result.
func (l LocalConfig) label() string {
	m, _ := findWhisperModel(l.Model)
	where := "процессор"
	if l.GPU {
		where = "видеокарта"
	}
	return "Whisper " + m.Label + ", " + where
}

// whisperThreads maps the load setting to a thread count: whisper gains
// little from SMT siblings, so "normal" is one thread per physical core.
func whisperThreads(load string) int {
	n := runtime.NumCPU()
	cores := n
	if n >= 4 {
		cores = n / 2
	}
	switch load {
	case "low":
		return max(2, cores/2)
	case "max":
		return n
	}
	return max(2, cores)
}

// ---------- downloads ----------

// runtimeFile picks what whisper-server needs out of a release archive,
// which also carries a few dozen tools and demos.
func runtimeFile(name string) bool {
	switch name {
	case "whisper-server.exe", "whisper.dll", "ggml.dll", "ggml-base.dll", "ggml-cuda.dll",
		"cudart64_12.dll", "cublas64_12.dll", "cublasLt64_12.dll":
		return true
	}
	return strings.HasPrefix(name, "ggml-cpu-") && strings.HasSuffix(name, ".dll")
}

type progressReader struct {
	r  io.Reader
	fn func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.fn(int64(n))
	}
	return n, err
}

// fetch downloads p into dest, checking its size and SHA-256 on the way;
// a file that doesn't match is deleted, never used.
func fetch(ctx context.Context, client *http.Client, p whisperPackage, dest string, progress func(n int64)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("нет связи с %s: %v", req.URL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s ответил %s", req.URL.Host, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), &progressReader{r: io.LimitReader(resp.Body, p.Size+1), fn: progress})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil && ctx.Err() != nil:
		err = ctx.Err()
	case err != nil:
		err = fmt.Errorf("загрузка прервалась: %v", err)
	case n != p.Size:
		err = fmt.Errorf("файл оборвался: %d из %d байт", n, p.Size)
	case hex.EncodeToString(h.Sum(nil)) != p.SHA256:
		err = errors.New("контрольная сумма не совпала — файл повреждён или подменён")
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// unpackRuntime extracts the files whisper-server needs into dir. Only base
// names are used, so no archive entry can land outside dir.
func unpackRuntime(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) { // names are reduced to base names below anyway
		return fmt.Errorf("архив повреждён: %v", err)
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	found := false
	for _, f := range zr.File {
		name := f.Name[strings.LastIndexAny(f.Name, `/\`)+1:]
		if f.FileInfo().IsDir() || !runtimeFile(name) {
			continue
		}
		found = found || name == "whisper-server.exe"
		if err := unpackFile(f, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if !found {
		return errors.New("в архиве нет whisper-server.exe")
	}
	return nil
}

func unpackFile(f *zip.File, dest string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// install downloads p and puts it in place, replacing an older copy.
func install(ctx context.Context, client *http.Client, p whisperPackage, progress func(int64)) error {
	if !p.isRuntime() {
		return fetch(ctx, client, p, p.path(), progress)
	}
	archive := p.path() + ".zip"
	if err := fetch(ctx, client, p, archive, progress); err != nil {
		return err
	}
	defer os.Remove(archive)
	staging := p.path() + ".new"
	os.RemoveAll(staging)
	err := unpackRuntime(archive, staging)
	if err == nil {
		err = os.WriteFile(filepath.Join(staging, "release.txt"), []byte(whisperRelease), 0o644)
	}
	if err == nil {
		if err = os.RemoveAll(p.path()); err != nil {
			err = fmt.Errorf("старые файлы заняты: %v", err)
		}
	}
	if err == nil {
		err = os.Rename(staging, p.path())
	}
	if err != nil {
		os.RemoveAll(staging)
	}
	return err
}

// ---------- engine ----------

// localEngine is the offline side of the app: downloads, and the server
// that recognises speech.
type localEngine struct {
	app *App
	srv *whisperServer

	mu     sync.Mutex
	dl     *DownloadView // the running download, nil when idle
	cancel context.CancelFunc
	sent   time.Time // last progress event
}

// DownloadView is the progress the offline panel shows.
type DownloadView struct {
	Target string `json:"target"` // a model id, "cuda", or "setup" (the first-run setup)
	Label  string `json:"label"`  // what is happening now
	Done   int64  `json:"done"`
	Total  int64  `json:"total"`
}

// LocalResult ends a download: the "local-changed" event.
type LocalResult struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
}

// download is one run of fetching: what to fetch and what to do after.
type download struct {
	target string
	plan   []whisperPackage
	adopt  func(*Config)   // settings to apply once everything is in
	done   string          // the message on success
	ended  func(err error) // called last, whatever the outcome
}

// downloadPlan lists what targets (model ids or "cuda") need that isn't on
// disk yet. The CPU build goes with every model: it is small and the
// fallback; the speech detector goes with everything.
func downloadPlan(targets ...string) ([]whisperPackage, error) {
	var plan []whisperPackage
	need := func(p whisperPackage) {
		if !p.installed() && !slices.ContainsFunc(plan, func(q whisperPackage) bool { return q.ID == p.ID }) {
			plan = append(plan, p)
		}
	}
	for _, t := range targets {
		if t == "cuda" {
			if nvidiaGPU() == "" {
				return nil, errors.New("видеокарта NVIDIA не найдена")
			}
			need(runtimeCUDA)
			continue
		}
		m, ok := findWhisperModel(t)
		if !ok {
			return nil, fmt.Errorf("неизвестная модель %q", t)
		}
		need(runtimeCPU)
		need(m.whisperPackage)
	}
	need(vadModel)
	return plan, nil
}

func newLocalEngine(app *App) *localEngine {
	return &localEngine{app: app, srv: &whisperServer{start: startWhisper}}
}

func (e *localEngine) progress() *DownloadView {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dl == nil {
		return nil
	}
	v := *e.dl
	return &v
}

// Download fetches what target needs — a model (with the CPU build, which
// is small and always the fallback) or the CUDA build — in the background.
func (e *localEngine) Download(target string) error {
	plan, err := downloadPlan(target)
	if err != nil {
		return err
	}
	return e.start(download{target: target, plan: plan, adopt: func(c *Config) {
		if target == "cuda" {
			c.Local.GPU = true
		} else {
			c.Local.Model = target
		}
	}})
}

// AutoSetup gets offline recognition going by itself for someone without an
// API key: the model that suits this computer — Large v3 Turbo with the CUDA
// build on an NVIDIA card, Small on the CPU — and once it is in, dictation
// and system audio use it. Nothing to click.
func (e *localEngine) AutoSetup() error {
	gpu := nvidiaGPU() != ""
	model, targets := "small-q5_1", []string{"small-q5_1"}
	if gpu {
		model, targets = "large-v3-turbo-q5_0", []string{"large-v3-turbo-q5_0", "cuda"}
	}
	plan, err := downloadPlan(targets...)
	if err != nil {
		return err
	}
	hotkey := strings.Join(e.app.store.Get().Hotkey.Parts(), "+")
	return e.start(download{
		target: "setup", plan: plan,
		adopt: func(c *Config) {
			c.Local.Model, c.Local.GPU = model, gpu
			c.Local.Mic, c.Local.System, c.Local.SetupDone = true, true, true
		},
		done: "Готово: диктовка работает без интернета — нажмите " + hotkey,
		ended: func(err error) {
			switch {
			case errors.Is(err, context.Canceled): // declined: don't start it again on every launch
				_ = e.app.store.Update(func(c *Config) { c.Local.SetupDone = true })
			case err == nil && e.app.overlay != nil:
				e.app.overlay.Show(overlayFrame{Kind: ovInfo, Text: "Распознавание без интернета готово"}, 4*time.Second)
			}
			// On a network error it is tried again at the next launch.
		},
	})
}

func (e *localEngine) start(d download) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dl != nil {
		return errors.New("уже идёт загрузка — дождитесь её или отмените")
	}
	var total int64
	for _, p := range d.plan {
		total += p.Size
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.dl, e.cancel = &DownloadView{Target: d.target, Label: "Подготовка…", Total: total}, cancel
	go e.run(ctx, d)
	return nil
}

func (e *localEngine) CancelDownload() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
}

func (e *localEngine) setStage(label string) {
	e.mu.Lock()
	if e.dl != nil {
		e.dl.Label = label
	}
	e.mu.Unlock()
	e.app.emit("local-progress", e.progress())
}

func (e *localEngine) run(ctx context.Context, d download) {
	err := e.fetchAll(ctx, d.plan)
	if err == nil {
		err = e.app.store.Update(d.adopt)
	}
	res := LocalResult{OK: true, Msg: d.done}
	if l := e.app.store.Get().Local; err == nil && l.GPU && l.missing() == "" {
		e.setStage("Подготовка видеокарты — один раз, до минуты…")
		if werr := e.warmUp(ctx, l); werr != nil && ctx.Err() == nil {
			// An old driver, too little video memory…: the CPU still works.
			_ = e.app.store.Update(func(c *Config) { c.Local.GPU = false })
			res.Msg = "Видеокарта не запустилась, распознаёт процессор: " + werr.Error()
		} else {
			err = werr
		}
	}
	e.mu.Lock()
	e.dl, e.cancel = nil, nil
	e.mu.Unlock()
	switch {
	case errors.Is(err, context.Canceled):
		res.Msg = "Загрузка отменена"
	case err != nil:
		res = LocalResult{Msg: "Не удалось: " + err.Error()}
	case res.Msg == "":
		res.Msg = "Готово — Whisper работает на этом компьютере"
	}
	if d.ended != nil {
		d.ended(err)
	}
	e.app.emit("local-changed", res)
}

func (e *localEngine) fetchAll(ctx context.Context, plan []whisperPackage) error {
	if len(plan) == 0 {
		return nil
	}
	api, _ := e.app.apiConfig(e.app.store.Get()) // the proxy, if one is set, helps here too
	proxy, err := api.proxyFunc()
	if err != nil {
		return err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = proxy
	t.ResponseHeaderTimeout = 30 * time.Second
	client := &http.Client{Transport: t}
	for _, p := range plan {
		e.setStage("Скачиваю: " + p.Label)
		err := install(ctx, client, p, func(n int64) {
			e.mu.Lock()
			if e.dl == nil {
				e.mu.Unlock()
				return
			}
			e.dl.Done += n
			send := time.Since(e.sent) > 120*time.Millisecond
			if send {
				e.sent = time.Now()
			}
			e.mu.Unlock()
			if send {
				e.app.emit("local-progress", e.progress())
			}
		})
		if err != nil {
			return fmt.Errorf("%s: %w", p.Label, err)
		}
	}
	return nil
}

// warmUp runs the model once. New cards (RTX 50xx) are newer than the CUDA
// build, so the driver compiles its kernels on the first run — up to a
// minute, once; better here than on the user's first dictation.
func (e *localEngine) warmUp(ctx context.Context, l LocalConfig) error {
	proc, release, err := e.srv.acquire(l)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, err = proc.transcribe(ctx, whisperRequest{Audio: wavFile(warmUpAudio()), Lang: "en", NoVAD: true})
	return err
}

// warmUpAudio is two seconds of quiet noise: enough to run every kernel.
func warmUpAudio() []byte {
	pcm := make([]byte, sampleRate*2*2)
	x := uint32(1)
	for i := 0; i < len(pcm); i += 2 {
		x = x*1664525 + 1013904223
		v := int16(int32(x>>16)%600 - 300)
		pcm[i], pcm[i+1] = byte(v), byte(uint16(v)>>8)
	}
	return pcm
}

// Remove deletes a model or the CUDA build.
func (e *localEngine) Remove(target string) error {
	if e.progress() != nil {
		return errors.New("дождитесь окончания загрузки")
	}
	if e.app.dict.busyLocal() {
		return errors.New("сейчас идёт распознавание — попробуйте через пару секунд")
	}
	e.srv.Stop() // its files are about to go
	var err error
	if target == "cuda" {
		err = os.RemoveAll(runtimeCUDA.path())
	} else if m, ok := findWhisperModel(target); ok {
		err = os.Remove(m.path())
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		return fmt.Errorf("неизвестная модель %q", target)
	}
	if err != nil {
		return fmt.Errorf("не удалось удалить: %v", err)
	}
	return e.app.store.Update(func(c *Config) {
		if target == "cuda" {
			c.Local.GPU = false
		}
		if c.Local.Model == target {
			c.Local.Model = ""
			for _, m := range whisperModels {
				if m.installed() {
					c.Local.Model = m.ID
					break
				}
			}
		}
		if c.Local.missing() != "" {
			c.Local.Mic, c.Local.System = false, false // back to OpenAI rather than failing
		}
	})
}

func (e *localEngine) Stop() {
	e.CancelDownload()
	e.srv.Stop()
}
