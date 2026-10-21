package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWavFile(t *testing.T) {
	pcm := make([]byte, 4800)
	w := wavFile(pcm)
	if len(w) != 44+len(pcm) || string(w[0:4]) != "RIFF" || string(w[8:16]) != "WAVEfmt " || string(w[36:40]) != "data" {
		t.Fatalf("bad header % x", w[:44])
	}
	if rate := binary.LittleEndian.Uint32(w[24:]); rate != sampleRate {
		t.Fatalf("rate %d", rate)
	}
	if n := binary.LittleEndian.Uint32(w[40:]); n != uint32(len(pcm)) {
		t.Fatalf("data size %d", n)
	}
}

func TestTidyWhisper(t *testing.T) {
	cases := map[string]string{
		" Привет, Саша.\n Как дела?\n":                                    "Привет, Саша. Как дела?",
		"Всё готово. Субтитры сделал DimaTorzok":                          "Всё готово.",
		"Редактор субтитров А.Синецкая Корректор А.Егорова":               "",
		"Продолжение следует...":                                          "",
		"[музыка] Давай начнём (Музыка)":                                  "Давай начнём",
		"Thanks. Subtitles by the Amara.org community":                    "Thanks.",
		"Продолжение следует в понедельник, после совещания.":             "Продолжение следует в понедельник, после совещания.",
		"Купи молоко (обычное, не обезжиренное) и хлеб — до семи вечера.": "Купи молоко (обычное, не обезжиренное) и хлеб — до семи вечера.",
	}
	for in, want := range cases {
		if got := tidyWhisper(in); got != want {
			t.Errorf("tidyWhisper(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWhisperLangAndPrompt(t *testing.T) {
	cfg := defaultConfig()
	if l := (actionSpec{Source: "mic"}).whisperLang(cfg); l != "auto" {
		t.Errorf("ru+en → %q, want auto", l)
	}
	cfg.Languages = []string{"ru"}
	if l := (actionSpec{Source: "mic"}).whisperLang(cfg); l != "ru" {
		t.Errorf("ru → %q", l)
	}
	if l := (actionSpec{Source: "system"}).whisperLang(cfg); l != "auto" {
		t.Errorf("system audio → %q, want auto", l)
	}
	if l := (actionSpec{Source: "mic", From: "de", To: "ru"}).whisperLang(cfg); l != "de" {
		t.Errorf("translation from de → %q", l)
	}
	if p := whisperPrompt([]string{"Kubernetes", " Анна Сергеевна ", "kubernetes", ""}); p != "Kubernetes, Анна Сергеевна." {
		t.Errorf("prompt %q", p)
	}
	if p := whisperPrompt(nil); p != "" {
		t.Errorf("empty prompt %q", p)
	}
}

func TestLocalConfig(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	l := LocalConfig{Model: "small-q5_1"}
	if !strings.Contains(l.missing(), "Модель") {
		t.Errorf("missing() = %q", l.missing())
	}
	if (LocalConfig{Mic: true}).usedFor("system") || !(LocalConfig{Mic: true}).usedFor("mic") || !(LocalConfig{System: true}).usedFor("system") {
		t.Error("usedFor")
	}
	if l.label() != "Whisper Small, процессор" {
		t.Errorf("label %q", l.label())
	}
	if lo, n, hi := whisperThreads("low"), whisperThreads("normal"), whisperThreads("max"); lo < 2 || lo > n || n > hi {
		t.Errorf("threads %d/%d/%d", lo, n, hi)
	}
	if whisperThreads("") != whisperThreads("normal") {
		t.Error("an old config without a load setting should get normal")
	}
	for _, m := range whisperModels {
		if len(m.SHA256) != 64 || m.Size <= 0 || !strings.HasPrefix(m.URL, "https://huggingface.co/") {
			t.Errorf("model %s: bad catalog entry", m.ID)
		}
	}
}

func TestSegmenterForWhisper(t *testing.T) {
	s := newSegmenter(true)
	feed := func(rms float64, ms int) (commits int) {
		for range ms / chunkMs {
			if s.push(rms, chunkBytes) {
				commits++
			}
		}
		return
	}
	if n := feed(.002, 1000) + feed(.08, 7000) + feed(.002, 1000); n != 0 {
		t.Fatalf("9 s take split %d times", n)
	}
	if n := feed(.08, 4000) + feed(.002, 1000); n != 1 {
		t.Fatalf("12 s then a pause: %d commits", n)
	}
	// Pieces must fit whisper's 30-second window.
	if n := feed(.08, 30000); n != 1 {
		t.Fatalf("30 s monologue: %d commits", n)
	}
}

func makeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rt.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return path
}

func TestUnpackRuntime(t *testing.T) {
	zipPath := makeZip(t, map[string]string{
		"Release/whisper-server.exe":      "exe",
		"Release/whisper.dll":             "dll",
		`Release\ggml-cpu-haswell.dll`:    "cpu",
		"Release/test-vad.exe":            "test",
		"Release/SDL2.dll":                "sdl",
		"../../ggml.dll":                  "escape attempt",
		"Release/sub/../../ggml-cuda.dll": "nested",
	})
	dir := filepath.Join(t.TempDir(), "rt")
	if err := unpackRuntime(zipPath, dir); err != nil {
		t.Fatal(err)
	}
	var got []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := []string{"ggml-cpu-haswell.dll", "ggml-cuda.dll", "ggml.dll", "whisper-server.exe", "whisper.dll"}
	if !slices.Equal(got, want) {
		t.Fatalf("unpacked %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "ggml.dll")); err == nil {
		t.Fatal("an entry escaped the target folder")
	}
	if err := unpackRuntime(makeZip(t, map[string]string{"Release/whisper.dll": "x"}), filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("an archive without whisper-server.exe should fail")
	}
}

func TestFetchChecksHash(t *testing.T) {
	body := []byte(strings.Repeat("whisper ", 5000))
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/short":
			w.Write(body[:100])
		default:
			w.Write(body)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	good := whisperPackage{ID: "m", URL: srv.URL + "/ok", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}

	var seen int64
	dest := filepath.Join(dir, "ok.bin")
	if err := fetch(context.Background(), srv.Client(), good, dest, func(n int64) { seen += n }); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dest); string(data) != string(body) || seen != int64(len(body)) {
		t.Fatalf("got %d bytes, progress %d", len(data), seen)
	}

	bad := good
	bad.SHA256 = strings.Repeat("0", 64)
	dest = filepath.Join(dir, "bad.bin")
	if err := fetch(context.Background(), srv.Client(), bad, dest, func(int64) {}); err == nil || !strings.Contains(err.Error(), "контрольная сумма") {
		t.Fatalf("tampered file: %v", err)
	}
	short := good
	short.URL = srv.URL + "/short"
	if err := fetch(context.Background(), srv.Client(), short, filepath.Join(dir, "short.bin"), func(int64) {}); err == nil {
		t.Fatal("a truncated download must fail")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(left) > 0 || fileExists(filepath.Join(dir, "bad.bin")) {
		t.Fatalf("leftovers after failed downloads: %v", left)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// fakeWhisper is whisper-server's /inference: it answers each final request
// (the test model decodes finals with beam search) with the number of the
// piece and records its form fields; drafts (greedy) get "Черновик".
type fakeWhisper struct {
	mu     sync.Mutex
	n      int
	drafts int
	forms  []map[string]string
	sizes  []int
	fail   bool
	delay  time.Duration
}

func (f *fakeWhisper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"no file"}`, 400)
		return
	}
	defer file.Close()
	audio, _ := io.ReadAll(file)
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	form := map[string]string{}
	for k, v := range r.MultipartForm.Value {
		form[k] = v[0]
	}
	if form["beam_size"] == "" {
		f.drafts++
		w.Write([]byte(`{"text":" Черновик.\n"}`))
		return
	}
	f.forms = append(f.forms, form)
	f.sizes = append(f.sizes, len(audio))
	if f.fail {
		w.Write([]byte(`{"error":"failed to process audio"}`))
		return
	}
	f.n++
	fmt.Fprintf(w, `{"text":" Кусок %d.\n"}`, f.n)
}

func fakeServer(t *testing.T, f *fakeWhisper) *whisperServer {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ready := make(chan struct{})
	close(ready)
	return &whisperServer{start: func(LocalConfig) (*whisperProc, error) {
		return &whisperProc{base: srv.URL, ready: ready, client: srv.Client()}, nil
	}}
}

func TestLocalSessionPiecesInOrder(t *testing.T) {
	f := &fakeWhisper{delay: 20 * time.Millisecond}
	var mu sync.Mutex
	var previews []string
	s := startLocalSession(fakeServer(t, f), LocalConfig{Model: "small-q5_1"}, "ru", "Kubernetes.", func(p string) {
		mu.Lock()
		previews = append(previews, p)
		mu.Unlock()
	})
	defer s.Close()
	second := make([]byte, sampleRate*2)
	for range 3 {
		s.Append(second)
		s.Commit()
	}
	s.Append(second[:minCommitBytes-2]) // too short for a piece of its own…
	s.Commit()
	s.Append(second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := s.Finish(ctx, true) // …so it travels with the tail
	if err != nil {
		t.Fatal(err)
	}
	if text != "Кусок 1. Кусок 2. Кусок 3. Кусок 4." {
		t.Fatalf("text %q", text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forms[0]["language"] != "ru" || f.forms[0]["prompt"] != "Kubernetes." || f.forms[0]["response_format"] != "json" || f.forms[0]["beam_size"] != "5" {
		t.Errorf("form %v", f.forms[0])
	}
	if last := f.sizes[len(f.sizes)-1]; last != 44+minCommitBytes-2+len(second) {
		t.Errorf("tail piece %d bytes", last)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(previews) == 0 || previews[len(previews)-1] != text {
		t.Errorf("previews %q", previews)
	}
}

func TestLocalSessionDrafts(t *testing.T) {
	f := &fakeWhisper{}
	shown := make(chan string, 16)
	s := startLocalSession(fakeServer(t, f), LocalConfig{Model: "small-q5_1"}, "ru", "", func(p string) { shown <- p })
	defer s.Close()
	s.Append(make([]byte, sampleRate*2*2)) // two seconds into a piece, nothing committed yet
	select {
	case p := <-shown:
		if p != "Черновик." {
			t.Fatalf("draft preview %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no draft while speaking")
	}
	// No new audio: no new draft, however long the wait.
	time.Sleep(1200 * time.Millisecond)
	f.mu.Lock()
	n := f.drafts
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d drafts of the same audio", n)
	}
	text, err := s.Finish(context.Background(), true)
	if err != nil || text != "Кусок 1." {
		t.Fatalf("final %q, %v", text, err)
	}
	if last := <-shown; last != "Кусок 1." {
		t.Fatalf("the final text should replace the draft, got %q", last)
	}
}

func TestLocalSessionDropsSilentTail(t *testing.T) {
	f := &fakeWhisper{}
	s := startLocalSession(fakeServer(t, f), LocalConfig{Model: "small-q5_1"}, "auto", "", nil)
	defer s.Close()
	s.Append(make([]byte, sampleRate*2))
	s.Commit()
	s.Append(make([]byte, sampleRate*2)) // trailing silence
	text, err := s.Finish(context.Background(), false)
	if err != nil || text != "Кусок 1." {
		t.Fatalf("%q, %v", text, err)
	}
	// A take that never reached a commit is still recognised as a whole.
	s2 := startLocalSession(fakeServer(t, &fakeWhisper{}), LocalConfig{Model: "small-q5_1"}, "auto", "", nil)
	defer s2.Close()
	s2.Append(make([]byte, sampleRate))
	if text, err := s2.Finish(context.Background(), false); err != nil || text != "Кусок 1." {
		t.Fatalf("short take: %q, %v", text, err)
	}
}

func TestLocalSessionErrors(t *testing.T) {
	s := startLocalSession(fakeServer(t, &fakeWhisper{fail: true}), LocalConfig{Model: "small-q5_1"}, "ru", "", nil)
	defer s.Close()
	s.Append(make([]byte, sampleRate*2))
	if _, err := s.Finish(context.Background(), true); err == nil || !strings.Contains(err.Error(), "failed to process audio") {
		t.Fatalf("server error: %v", err)
	}

	broken := &whisperServer{start: func(LocalConfig) (*whisperProc, error) {
		return nil, fmt.Errorf("не удалось запустить whisper-server")
	}}
	s2 := startLocalSession(broken, LocalConfig{}, "ru", "", nil)
	defer s2.Close()
	s2.Append(make([]byte, sampleRate*2))
	if _, err := s2.Finish(context.Background(), true); err == nil {
		t.Fatal("a server that didn't start must fail the take")
	}

	slow := startLocalSession(fakeServer(t, &fakeWhisper{delay: time.Second}), LocalConfig{Model: "small-q5_1"}, "ru", "", nil)
	defer slow.Close()
	slow.Append(make([]byte, sampleRate*2))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := slow.Finish(ctx, true); err == nil || !strings.Contains(err.Error(), "слишком долго") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestWhisperServerReuse(t *testing.T) {
	starts := 0
	ready := make(chan struct{})
	close(ready)
	s := &whisperServer{start: func(LocalConfig) (*whisperProc, error) {
		starts++
		return &whisperProc{ready: ready}, nil
	}}
	defer s.Stop()
	a := LocalConfig{Model: "small-q5_1"}
	p1, rel1, _ := s.acquire(a)
	p2, rel2, _ := s.acquire(a)
	if p1 != p2 || starts != 1 {
		t.Fatalf("same settings started %d servers", starts)
	}
	// While a take holds it, a settings change waits.
	if p3, rel3, _ := s.acquire(LocalConfig{Model: "base-q5_1"}); p3 != p1 {
		t.Fatal("restarted under a running take")
	} else {
		rel3()
	}
	rel1()
	rel1() // releasing twice is harmless
	rel2()
	if s.loaded() == "" {
		t.Fatal("the model should stay loaded until it has been idle a while")
	}
	if p4, rel4, _ := s.acquire(LocalConfig{Model: "base-q5_1"}); p4 == p1 || starts != 2 {
		t.Fatal("a new model needs a new server")
	} else {
		rel4()
	}
	s.Stop()
	if s.loaded() != "" {
		t.Fatal("Stop should unload")
	}
}

func TestDownloadPlan(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	plan, err := downloadPlan("small-q5_1", "base-q5_1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range plan {
		ids = append(ids, p.ID)
	}
	if want := []string{"cpu", "small-q5_1", "base-q5_1", "vad"}; !slices.Equal(ids, want) {
		t.Fatalf("plan %v, want %v", ids, want)
	}
	if _, err := downloadPlan("medium"); err == nil {
		t.Fatal("unknown model accepted")
	}
}

// installFake puts files of the right names and sizes where installed()
// looks, standing in for a finished download.
func installFake(t *testing.T, pkgs ...whisperPackage) {
	t.Helper()
	for _, p := range pkgs {
		if p.isRuntime() {
			os.MkdirAll(p.path(), 0o755)
			os.WriteFile(filepath.Join(p.path(), "release.txt"), []byte(whisperRelease), 0o644)
			continue
		}
		os.MkdirAll(filepath.Dir(p.path()), 0o755)
		f, err := os.Create(p.path())
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(p.Size)
		f.Close()
	}
}

func TestFirstRunSetupWithoutKey(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	gpu := nvidiaGPU() != ""
	model, _ := findWhisperModel("small-q5_1")
	if gpu {
		model, _ = findWhisperModel("large-v3-turbo-q5_0")
	}
	cfg := defaultConfig()
	if !needsSetup(cfg) {
		t.Fatal("no key and nothing installed: setup is needed")
	}
	if withKey := cfg; func() bool { withKey.APIKey = "sealed"; return needsSetup(withKey) }() {
		t.Fatal("a user with a key gets OpenAI, not a 1 GB download")
	}

	app := &App{store: &ConfigStore{path: filepath.Join(t.TempDir(), "config.json"), cfg: cfg}}
	app.dict = newDictation(app)
	app.whisper = newLocalEngine(app)
	app.whisper.srv = fakeServer(t, &fakeWhisper{}) // the GPU warm-up talks to it
	installFake(t, runtimeCPU, runtimeCUDA, vadModel, model.whisperPackage)
	if err := app.whisper.AutoSetup(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); app.whisper.progress() != nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("setup didn't finish")
		}
	}
	l := app.store.Get().Local
	if l.Model != model.ID || l.GPU != gpu || !l.Mic || !l.System || !l.SetupDone || l.missing() != "" {
		t.Fatalf("after setup: %+v", l)
	}
	if needsSetup(app.store.Get()) {
		t.Fatal("setup must not run again")
	}
}
