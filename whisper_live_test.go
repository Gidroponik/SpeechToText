//go:build live

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Offline recognition end to end (go test -tags live -run LiveWhisper -v):
// downloads what is missing into %LOCALAPPDATA%\SpeechToText\whisper through
// the app's own download path, then feeds STT_WAV (24 kHz 16-bit mono) to a
// localSession in 100 ms chunks through the segmenter, as a recording would.
// The app's settings are not touched: the engine gets a throwaway store.

func TestLiveWhisper(t *testing.T) {
	wav := os.Getenv("STT_WAV")
	if wav == "" {
		t.Skip("STT_WAV is not set")
	}
	data, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	pcm := data[44:]

	app := &App{store: &ConfigStore{path: filepath.Join(t.TempDir(), "config.json"), cfg: defaultConfig()}}
	app.dict = newDictation(app)
	e := newLocalEngine(app)
	defer e.Stop()

	targets := []string{"small-q5_1", "large-v3-turbo-q5_0"}
	if nvidiaGPU() != "" {
		targets = append(targets, "cuda")
	}
	for _, target := range targets {
		began := time.Now()
		if err := e.Download(target); err != nil {
			t.Fatal(err)
		}
		for e.progress() != nil {
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("%s ready in %.1fs (GPU %q)", target, time.Since(began).Seconds(), nvidiaGPU())
	}

	if os.Getenv("STT_REALTIME") != "" {
		// Paced like a real recording, to watch the drafts arrive.
		l := LocalConfig{Model: "large-v3-turbo-q5_0", GPU: nvidiaGPU() != "", Load: "normal"}
		began := time.Now()
		drafts := 0
		s := startLocalSession(e.srv, l, "ru", "", func(p string) {
			drafts++
			t.Logf("%5.1fs  %s", time.Since(began).Seconds(), p)
		})
		seg := newSegmenter(true)
		for i := 0; i < len(pcm); i += chunkBytes {
			c := pcm[i:min(len(pcm), i+chunkBytes)]
			s.Append(c)
			if seg.push(pcmRMS(c), len(c)) {
				s.Commit()
			}
			time.Sleep(time.Duration(len(c)/2) * time.Second / sampleRate)
		}
		stop := time.Now()
		text, err := s.Finish(context.Background(), seg.speech)
		s.Close()
		t.Logf("%s: %d previews, final %.2fs after stop: %s (%v)", l.label(), drafts, time.Since(stop).Seconds(), text, err)
		if err != nil || drafts < 5 {
			t.Fatal("expected drafts while speaking and a final text")
		}
		return
	}

	runs := []LocalConfig{{Model: "small-q5_1"}, {Model: "large-v3-turbo-q5_0"}}
	if nvidiaGPU() != "" {
		runs = append(runs, LocalConfig{Model: "large-v3-turbo-q5_0", GPU: true}, LocalConfig{Model: "small-q5_1", GPU: true})
	}
	for _, l := range runs {
		l.Load = "normal"
		if msg := l.missing(); msg != "" {
			t.Fatalf("%s: %s", l.label(), msg)
		}
		started := time.Now()
		s := startLocalSession(e.srv, l, "ru", "", nil)
		seg := newSegmenter(true)
		for i := 0; i < len(pcm); i += chunkBytes {
			c := pcm[i:min(len(pcm), i+chunkBytes)]
			s.Append(c)
			if seg.push(pcmRMS(c), len(c)) {
				s.Commit()
			}
		}
		stop := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		text, err := s.Finish(ctx, seg.speech)
		cancel()
		s.Close()
		if err != nil {
			t.Fatalf("%s: %v", l.label(), err)
		}
		t.Logf("%s: %.2fs after stop (%.1fs incl. start) → %s", l.label(), time.Since(stop).Seconds(), time.Since(started).Seconds(), text)
		if len([]rune(text)) < 100 {
			t.Errorf("%s: too little text", l.label())
		}
	}
}
