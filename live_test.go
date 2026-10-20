//go:build live

package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Probes against the real OpenAI API (go test -tags live -run Live -v).
// Without OPENAI_API_KEY they use a dummy key and check that both endpoints
// exist and reject it the way the app expects; with a key they must accept it.

func liveAPI() apiConfig {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		key = "sk-proj-invalid-key-for-probe-0000000000"
	}
	return apiConfig{Key: key, Proxy: os.Getenv("STT_PROXY")}
}

func TestLiveModelEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := checkKey(ctx, liveAPI())
	t.Logf("checkKey: %v", err)
	if os.Getenv("OPENAI_API_KEY") != "" && err != nil {
		t.Fatal(err)
	}
}

func TestLiveRealtimeSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := startLiveSession(liveAPI(), transcribeOptions{Delay: "high", Languages: []string{"ru", "en"}, Keywords: []string{"Telegram"}}, nil)
	defer s.Close()
	s.Append(make([]byte, sampleRate*2/2)) // 0.5 s of silence
	text, err := s.Finish(ctx, true)
	t.Logf("text %q, err: %v", text, err)
	if os.Getenv("OPENAI_API_KEY") == "" {
		var ae *apiError
		if !errors.As(err, &ae) || ae.Error() != "Неверный API-ключ OpenAI" {
			t.Fatalf("want an invalid-key apiError, got %#v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

// TestLiveMicMapping lists capture endpoints and finds their waveIn devices
// (no audio is recorded).
func TestLiveMicMapping(t *testing.T) {
	mics, err := listMics()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mics {
		idx, ok := waveInIndex(m.ID)
		t.Logf("default=%v waveIn=%d/%v %s", m.IsDefault, idx, ok, m.Name)
		if !ok {
			t.Errorf("no waveIn device for %q", m.Name)
		}
	}
	if len(mics) > 0 && defaultMicID() == "" {
		t.Error("no default microphone id")
	}
}

// TestLiveLoopback captures 1.5 s of system audio into memory (nothing is
// saved or sent) to check the WASAPI loopback path and the 24 kHz output.
func TestLiveLoopback(t *testing.T) {
	outs, err := listOutputs()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outs {
		t.Logf("output: default=%v %s", o.IsDefault, o.Name)
	}
	var total, chunks int
	var peak float64
	lb, found, err := startLoopback("", func(pcm []byte) {
		total += len(pcm)
		chunks++
		peak = max(peak, pcmRMS(pcm))
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	lb.Stop()
	t.Logf("found=%v chunks=%d audio=%v peak RMS=%.4f", found, chunks, audioDuration(total), peak)
	if total%2 != 0 {
		t.Fatal("odd PCM length")
	}
}
