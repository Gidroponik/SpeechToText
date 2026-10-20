package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	transcribeModel  = "gpt-live-transcribe"
	transcribePerMin = 0.017 // USD per minute of audio

	minCommitBytes = sampleRate * 2 / 10 // the API rejects turns under 100 ms
	maxAppendBytes = 256 << 10           // keep each append event small
	transcribeHint = "Dictation of a message, note or e-mail recorded from a computer microphone."
)

type transcribeOptions struct {
	Delay     string
	Languages []string
	Keywords  []string
}

// liveSession streams microphone audio to gpt-live-transcribe while the user
// speaks, so the text is ready moments after they stop. Audio appended before
// the WebSocket is up is queued and sent on connect.
type liveSession struct {
	api       apiConfig
	opts      transcribeOptions
	onPartial func(string)

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{} // nudges the writer

	mu          sync.Mutex
	changed     chan struct{} // closed and replaced on every state change
	queue       []outgoing
	sinceCommit int // audio bytes since the last commit
	commits     int // commits sent and not rejected
	items       []string
	seen        []string // every item id in order of first appearance (for previews)
	final       map[string]string
	failed      map[string]string
	partial     map[string]string
	err         error
}

type outgoing struct {
	audio  []byte
	commit bool
	clear  bool
}

func startLiveSession(api apiConfig, opts transcribeOptions, onPartial func(string)) *liveSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &liveSession{
		api: api, opts: opts, onPartial: onPartial,
		ctx: ctx, cancel: cancel,
		wake:    make(chan struct{}, 1),
		changed: make(chan struct{}),
		final:   map[string]string{}, failed: map[string]string{}, partial: map[string]string{},
	}
	go s.run()
	return s
}

// broadcast wakes everyone waiting in Finish; s.mu must be held.
func (s *liveSession) broadcast() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *liveSession) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *liveSession) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.broadcast()
	s.mu.Unlock()
	s.cancel()
}

// Append queues PCM audio (24 kHz, 16-bit mono).
func (s *liveSession) Append(pcm []byte) {
	s.mu.Lock()
	if n := len(s.queue); n > 0 && s.queue[n-1].audio != nil && len(s.queue[n-1].audio)+len(pcm) <= maxAppendBytes {
		s.queue[n-1].audio = append(s.queue[n-1].audio, pcm...)
	} else {
		s.queue = append(s.queue, outgoing{audio: append([]byte(nil), pcm...)})
	}
	s.sinceCommit += len(pcm)
	s.mu.Unlock()
	s.nudge()
}

// Commit ends the current turn so its final transcript can be produced.
func (s *liveSession) Commit() {
	s.mu.Lock()
	ok := s.sinceCommit >= minCommitBytes
	if ok {
		s.queue = append(s.queue, outgoing{commit: true})
		s.commits++
		s.sinceCommit = 0
	}
	s.mu.Unlock()
	if ok {
		s.nudge()
	}
}

// Finish commits the remaining audio (or drops it when commitTail is false,
// e.g. trailing silence) and waits for every turn's final transcript.
func (s *liveSession) Finish(ctx context.Context, commitTail bool) (string, error) {
	s.mu.Lock()
	tail := s.sinceCommit
	s.mu.Unlock()
	if commitTail || tail >= minCommitBytes && s.committed() == 0 {
		s.Commit()
	}
	s.mu.Lock()
	if s.sinceCommit > 0 {
		s.queue = append(s.queue, outgoing{clear: true})
		s.sinceCommit = 0
	}
	s.mu.Unlock()
	s.nudge()

	for {
		s.mu.Lock()
		text, done, err := s.resultLocked()
		ch := s.changed
		s.mu.Unlock()
		if done || err != nil {
			return text, err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errors.New("OpenAI слишком долго распознаёт речь — попробуйте ещё раз")
			}
			return "", ctx.Err()
		}
	}
}

func (s *liveSession) committed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits
}

// resultLocked reports the joined transcript once every commit is resolved.
func (s *liveSession) resultLocked() (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	if s.commits == 0 {
		return "", true, nil
	}
	if len(s.items) < s.commits {
		return "", false, nil
	}
	var parts []string
	var firstFail string
	for _, id := range s.items {
		if t, ok := s.final[id]; ok {
			if t = strings.TrimSpace(t); t != "" {
				parts = append(parts, t)
			}
			continue
		}
		if f, ok := s.failed[id]; ok {
			if firstFail == "" {
				firstFail = f
			}
			continue
		}
		return "", false, nil
	}
	if len(parts) == 0 && firstFail != "" {
		return "", true, errors.New("OpenAI не смог распознать запись: " + firstFail)
	}
	return strings.Join(parts, " "), true, nil
}

// previewLocked is the best current guess of the whole text.
func (s *liveSession) previewLocked() string {
	var parts []string
	for _, id := range s.seen {
		t, ok := s.final[id]
		if !ok {
			t = s.partial[id]
		}
		if t = strings.TrimSpace(t); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// Close aborts the session.
func (s *liveSession) Close() {
	s.fail(context.Canceled)
}

// ---------- connection ----------

func (s *liveSession) run() {
	conn, err := dialRealtime(s.ctx, s.api)
	if err != nil {
		s.fail(err)
		return
	}
	defer conn.Close()
	go func() {
		<-s.ctx.Done()
		conn.Close() // unblocks the reader
	}()

	if err := writeJSON(conn, sessionUpdate(s.opts)); err != nil {
		s.fail(netError(err))
		return
	}
	go s.read(conn)

	commitN := 0
	for {
		s.mu.Lock()
		batch := s.queue
		s.queue = nil
		s.mu.Unlock()
		for _, m := range batch {
			var ev map[string]any
			switch {
			case m.audio != nil:
				ev = map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(m.audio)}
			case m.commit:
				commitN++
				ev = map[string]any{"type": "input_audio_buffer.commit", "event_id": fmt.Sprintf("commit_%d", commitN)}
			case m.clear:
				ev = map[string]any{"type": "input_audio_buffer.clear"}
			}
			if err := writeJSON(conn, ev); err != nil {
				s.fail(netError(err))
				return
			}
		}
		select {
		case <-s.wake:
		case <-s.ctx.Done():
			return
		}
	}
}

func dialRealtime(ctx context.Context, api apiConfig) (*websocket.Conn, error) {
	proxy, err := api.proxyFunc()
	if err != nil {
		return nil, err
	}
	d := websocket.Dialer{Proxy: proxy, HandshakeTimeout: 15 * time.Second}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+api.Key)
	conn, resp, err := d.DialContext(ctx, api.realtimeURL(), h)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, readAPIError(resp.StatusCode, resp.Body)
		}
		return nil, netError(err)
	}
	return conn, nil
}

func writeJSON(conn *websocket.Conn, v any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return conn.WriteJSON(v)
}

type serverEvent struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	Error      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
		EventID string `json:"event_id"`
	} `json:"error"`
}

func (s *liveSession) read(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if s.ctx.Err() == nil {
				s.fail(closeError(err))
			}
			return
		}
		var ev serverEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		s.handle(ev)
	}
}

// closeError maps OpenAI's application close codes (3000+, reason like
// "invalid_request_error.invalid_api_key") to API errors: the handshake
// itself succeeds even with a bad key, the rejection comes as a close.
func closeError(err error) error {
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code >= 3000 {
		kind, code, _ := strings.Cut(ce.Text, ".")
		if code == "" {
			code = kind
		}
		return &apiError{Code: code, Message: "OpenAI закрыл соединение: " + ce.Text}
	}
	return netError(err)
}

func (s *liveSession) handle(ev serverEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	note := func(id string) {
		if id == "" {
			return
		}
		for _, x := range s.seen {
			if x == id {
				return
			}
		}
		s.seen = append(s.seen, id)
	}
	preview := false
	switch ev.Type {
	case "input_audio_buffer.committed":
		s.items = append(s.items, ev.ItemID)
		note(ev.ItemID)
	case "conversation.item.input_audio_transcription.delta":
		note(ev.ItemID)
		s.partial[ev.ItemID] += ev.Delta
		preview = true
	case "conversation.item.input_audio_transcription.completed":
		note(ev.ItemID)
		s.final[ev.ItemID] = ev.Transcript
		preview = true
	case "conversation.item.input_audio_transcription.failed":
		msg := "ошибка распознавания"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		s.failed[ev.ItemID] = msg
	case "error":
		if ev.Error == nil {
			return
		}
		if strings.HasPrefix(ev.Error.EventID, "commit_") {
			s.commits-- // a rejected commit (e.g. too little audio) yields no item
		} else if s.err == nil {
			s.err = &apiError{Code: ev.Error.Code, Message: "OpenAI: " + ev.Error.Message}
		}
	default:
		return
	}
	s.broadcast()
	if preview && s.onPartial != nil {
		go s.onPartial(s.previewLocked())
	}
}

// ---------- session configuration ----------

var langCode = regexp.MustCompile(`^[a-z]{2,3}(-[a-z]{2})?$`)

func sessionUpdate(o transcribeOptions) map[string]any {
	tr := map[string]any{"model": transcribeModel, "prompt": transcribeHint}
	if o.Delay != "" {
		tr["delay"] = o.Delay
	}
	var langs []string
	for _, l := range o.Languages {
		if l = strings.ToLower(strings.TrimSpace(l)); langCode.MatchString(l) {
			langs = append(langs, l)
		}
	}
	if len(langs) > 0 {
		tr["languages"] = langs
	}
	if kw := cleanKeywords(o.Keywords); len(kw) > 0 {
		tr["keywords"] = kw
	}
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": sampleRate},
					"transcription":  tr,
					"turn_detection": nil, // the model has no server VAD; we commit turns ourselves
				},
			},
		},
	}
}

// cleanKeywords drops characters the API rejects (<, >, line breaks) and duplicates.
func cleanKeywords(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range in {
		k = strings.Map(func(r rune) rune {
			if r == '<' || r == '>' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, k)
		k = strings.TrimSpace(k)
		if k == "" || len([]rune(k)) > 80 || seen[strings.ToLower(k)] {
			continue
		}
		seen[strings.ToLower(k)] = true
		out = append(out, k)
		if len(out) == 200 {
			break
		}
	}
	return out
}

// transcribeAll sends a finished recording in one go; used as a fallback
// when the live connection dropped mid-recording.
func transcribeAll(ctx context.Context, api apiConfig, opts transcribeOptions, pcm []byte) (string, error) {
	s := startLiveSession(api, opts, nil)
	defer s.Close()
	for i := 0; i < len(pcm); i += maxAppendBytes {
		s.Append(pcm[i:min(len(pcm), i+maxAppendBytes)])
	}
	return s.Finish(ctx, true)
}
