package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeRealtime imitates the transcription WebSocket: it acknowledges each
// commit with a committed event and sends a delta and a final transcript
// for it, taken from the transcripts list in order.
type fakeRealtime struct {
	t           *testing.T
	transcripts []string
	rejectFirst bool // answer the first commit with an error

	mu       sync.Mutex
	auth     string
	query    string
	session  map[string]any
	appended int
	commits  int
	clears   int
}

func (f *fakeRealtime) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = r.Header.Get("Authorization")
	f.query = r.URL.RawQuery
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk-test-key-0123456789abcdef" {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
		return
	}
	up := websocket.Upgrader{}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		f.t.Error(err)
		return
	}
	defer conn.Close()
	item := 0
	for {
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			return
		}
		f.mu.Lock()
		switch ev["type"] {
		case "session.update":
			f.session = ev
		case "input_audio_buffer.append":
			b, _ := base64.StdEncoding.DecodeString(ev["audio"].(string))
			f.appended += len(b)
		case "input_audio_buffer.clear":
			f.clears++
		case "input_audio_buffer.commit":
			f.commits++
			if f.rejectFirst && f.commits == 1 {
				_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{
					"type": "invalid_request_error", "code": "input_audio_buffer_commit_empty",
					"message": "buffer too small", "event_id": ev["event_id"]}})
				break
			}
			item++
			id := fmt.Sprintf("item_%03d", item)
			text := f.transcripts[item-1]
			_ = conn.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": id})
			_ = conn.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": id, "delta": text[:len(text)/2]})
			_ = conn.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": id, "transcript": text})
		}
		f.mu.Unlock()
	}
}

func newFake(t *testing.T, transcripts ...string) (*fakeRealtime, apiConfig) {
	f := &fakeRealtime{t: t, transcripts: transcripts}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, apiConfig{Key: "sk-test-key-0123456789abcdef", BaseURL: srv.URL + "/v1"}
}

func pcmOf(ms int) []byte { return make([]byte, sampleRate*2*ms/1000) }

func TestLiveSessionSingleTurn(t *testing.T) {
	f, api := newFake(t, "Привет, как дела?")
	var mu sync.Mutex
	var previews []string
	s := startLiveSession(api, transcribeOptions{
		Delay:     "high",
		Languages: []string{"ru", "EN", "bad code!"},
		Keywords:  []string{"Telegram", "<tag>", "telegram", "line\nbreak", ""},
	}, func(p string) {
		mu.Lock()
		previews = append(previews, p)
		mu.Unlock()
	})
	defer s.Close()
	for range 10 {
		s.Append(pcmOf(100))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := s.Finish(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Привет, как дела?" {
		t.Fatalf("text = %q", text)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.query != "intent=transcription" {
		t.Errorf("query = %q", f.query)
	}
	if f.appended != len(pcmOf(1000)) || f.commits != 1 {
		t.Errorf("appended %d bytes, %d commits", f.appended, f.commits)
	}
	in := f.session["session"].(map[string]any)["audio"].(map[string]any)["input"].(map[string]any)
	if v, ok := in["turn_detection"]; !ok || v != nil {
		t.Errorf("turn_detection = %v (present %v), want explicit null", v, ok)
	}
	format := in["format"].(map[string]any)
	if format["type"] != "audio/pcm" || format["rate"] != float64(24000) {
		t.Errorf("format = %v", format)
	}
	tr := in["transcription"].(map[string]any)
	if tr["model"] != "gpt-live-transcribe" || tr["delay"] != "high" {
		t.Errorf("transcription = %v", tr)
	}
	if got := fmt.Sprint(tr["languages"]); got != "[ru en]" {
		t.Errorf("languages = %s", got)
	}
	if got := fmt.Sprint(tr["keywords"]); got != "[Telegram tag linebreak]" {
		t.Errorf("keywords = %s", got)
	}
	if _, ok := tr["language"]; ok {
		t.Error("singular language must not be sent")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(previews) == 0 {
		t.Error("no live preview")
	}
}

func TestLiveSessionSegmentsJoinInOrder(t *testing.T) {
	f, api := newFake(t, "Первая часть.", "Вторая часть.")
	s := startLiveSession(api, transcribeOptions{}, nil)
	defer s.Close()
	s.Append(pcmOf(500))
	s.Commit()
	s.Append(pcmOf(500))
	s.Append(pcmOf(40)) // too short to commit on its own: dropped with clear
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// commitTail=true commits the remaining 540 ms as the second turn.
	text, err := s.Finish(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Первая часть. Вторая часть." {
		t.Fatalf("text = %q", text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commits != 2 || f.clears != 0 {
		t.Fatalf("commits %d clears %d", f.commits, f.clears)
	}
}

func TestLiveSessionDropsSilentTail(t *testing.T) {
	f, api := newFake(t, "Только начало.")
	s := startLiveSession(api, transcribeOptions{}, nil)
	defer s.Close()
	s.Append(pcmOf(400))
	s.Commit()
	s.Append(pcmOf(300))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := s.Finish(ctx, false)
	if err != nil || text != "Только начало." {
		t.Fatalf("text %q err %v", text, err)
	}
	// The first turn may be finished before the clear goes out; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		commits, clears := f.commits, f.clears
		f.mu.Unlock()
		if clears == 1 || time.Now().After(deadline) {
			if commits != 1 || clears != 1 {
				t.Fatalf("commits %d clears %d", commits, clears)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveSessionRejectedCommit(t *testing.T) {
	f, api := newFake(t, "Всё равно получилось.")
	f.rejectFirst = true
	s := startLiveSession(api, transcribeOptions{}, nil)
	defer s.Close()
	s.Append(pcmOf(300))
	s.Commit()
	s.Append(pcmOf(300))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := s.Finish(ctx, true)
	if err != nil || text != "Всё равно получилось." {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestLiveSessionBadKey(t *testing.T) {
	_, api := newFake(t)
	api.Key = "sk-wrong-key-000000000000"
	s := startLiveSession(api, transcribeOptions{}, nil)
	defer s.Close()
	s.Append(pcmOf(300))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Finish(ctx, true)
	if err == nil || !strings.Contains(err.Error(), "Неверный API-ключ") {
		t.Fatalf("err = %v", err)
	}
}

func TestTranscribeAll(t *testing.T) {
	f, api := newFake(t, "Длинная запись целиком.")
	pcm := pcmOf(12000) // 12 s: several append events, one commit
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := transcribeAll(ctx, api, transcribeOptions{}, pcm)
	if err != nil || text != "Длинная запись целиком." {
		t.Fatalf("text %q err %v", text, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appended != len(pcm) || f.commits != 1 {
		t.Fatalf("appended %d of %d, commits %d", f.appended, len(pcm), f.commits)
	}
}

func TestRealtimeURL(t *testing.T) {
	cases := map[string]string{
		"":                           "wss://api.openai.com/v1/realtime?intent=transcription",
		"https://api.openai.com/v1/": "wss://api.openai.com/v1/realtime?intent=transcription",
		"http://localhost:8080/v1":   "ws://localhost:8080/v1/realtime?intent=transcription",
	}
	for base, want := range cases {
		if got := (apiConfig{BaseURL: base}).realtimeURL(); got != want {
			t.Errorf("%q: got %q", base, got)
		}
	}
}

func TestSessionUpdateIsValidJSON(t *testing.T) {
	b, err := json.Marshal(sessionUpdate(transcribeOptions{Delay: "low"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"turn_detection":null`) {
		t.Fatalf("%s", b)
	}
}

func TestCloseErrorMapping(t *testing.T) {
	err := closeError(&websocket.CloseError{Code: 3000, Text: "invalid_request_error.invalid_api_key"})
	var ae *apiError
	if !errors.As(err, &ae) || ae.Error() != "Неверный API-ключ OpenAI" {
		t.Fatalf("3000 close: %v", err)
	}
	if err := closeError(&websocket.CloseError{Code: 1006}); errors.As(err, &ae) {
		t.Fatalf("abnormal close must stay a network error (retryable): %v", err)
	}
}
