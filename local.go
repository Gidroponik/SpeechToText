package main

import (
	"context"
	"encoding/binary"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// transcriber is a recognition session for one take: OpenAI's live
// connection (realtime.go) or whisper on this computer (localSession).
type transcriber interface {
	Append(pcm []byte)
	Commit()
	Finish(ctx context.Context, commitTail bool) (string, error)
	Close()
}

// localSession is the offline counterpart of liveSession. Committed pieces
// of the recording go to whisper-server while the user keeps talking, so
// only the last piece is left to recognise when they stop. Whisper has no
// streaming, so in between the piece being spoken is recognised again every
// so often as a draft: that is the text the indicator shows while recording.
type localSession struct {
	lang      string
	prompt    string
	beam      int
	onPartial func(string)

	ctx       context.Context
	cancel    context.CancelFunc
	proc      *whisperProc
	release   func()
	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu        sync.Mutex
	buf       []byte   // audio since the last commit
	queue     [][]byte // committed pieces waiting for the server
	commits   int
	sealed    bool     // Finish was called: no more pieces will come
	texts     []string // final text of each recognised piece
	drafts    []string // draft of each committed piece still waiting for its final text
	draft     string   // draft of the piece being spoken
	drafted   int      // len(buf) at the last draft
	nextDraft time.Time
	err       error
}

const (
	draftMinBytes = sampleRate * 2      // a draft needs a second of audio…
	draftStep     = sampleRate * 2 / 2  // …and half a second more than the last one
	draftMaxBytes = sampleRate * 2 * 28 // whisper's window, less a margin
	draftEvery    = 800 * time.Millisecond
)

func startLocalSession(srv *whisperServer, l LocalConfig, lang, prompt string, onPartial func(string)) *localSession {
	ctx, cancel := context.WithCancel(context.Background())
	m, _ := findWhisperModel(l.Model)
	s := &localSession{
		lang: lang, prompt: prompt, beam: m.Beam, onPartial: onPartial, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	// Starting the server now loads the model while the user is still speaking.
	s.proc, s.release, s.err = srv.acquire(l)
	go s.run()
	return s
}

func (s *localSession) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Append adds PCM audio (24 kHz, 16-bit mono).
func (s *localSession) Append(pcm []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, pcm...)
	s.mu.Unlock()
}

// Commit sends the audio since the previous commit off to be recognised.
func (s *localSession) Commit() {
	s.mu.Lock()
	ok := s.commitLocked()
	s.mu.Unlock()
	if ok {
		s.nudge()
	}
}

func (s *localSession) commitLocked() bool {
	if len(s.buf) < minCommitBytes || s.sealed {
		return false
	}
	s.queue = append(s.queue, s.buf)
	s.drafts = append(s.drafts, s.draft)
	s.buf, s.draft, s.drafted = nil, "", 0
	s.commits++
	return true
}

// previewLocked is the best current guess of the whole text.
func (s *localSession) previewLocked() string {
	var parts []string
	for _, p := range append(append(slices.Clone(s.texts), s.drafts...), s.draft) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// Finish commits the remaining audio (or drops it when commitTail is false,
// e.g. trailing silence) and waits for every piece's text.
func (s *localSession) Finish(ctx context.Context, commitTail bool) (string, error) {
	s.mu.Lock()
	if commitTail || s.commits == 0 {
		s.commitLocked()
	}
	s.buf, s.sealed = nil, true
	s.mu.Unlock()
	s.nudge()
	select {
	case <-s.done:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("Whisper слишком долго распознаёт речь — выберите модель полегче или видеокарту")
		}
		return "", ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	return strings.Join(s.texts, " "), nil
}

// Close aborts the session and lets the server go idle.
func (s *localSession) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.release != nil {
			s.release()
		}
	})
}

// run feeds the server one request at a time: committed pieces first, a
// draft of the piece being spoken when there is nothing else to do.
func (s *localSession) run() {
	defer close(s.done)
	if s.proc == nil {
		return // acquire failed; Finish reports s.err
	}
	for {
		s.mu.Lock()
		var piece, sketch []byte
		gen := s.commits
		if len(s.queue) > 0 {
			piece, s.queue = s.queue[0], s.queue[1:]
		} else if !s.sealed && len(s.buf) >= draftMinBytes && len(s.buf)-s.drafted >= draftStep && !time.Now().Before(s.nextDraft) {
			sketch = slices.Clone(s.buf[max(0, len(s.buf)-draftMaxBytes):])
			s.drafted = len(s.buf)
		}
		sealed := s.sealed
		s.mu.Unlock()
		switch {
		case piece != nil:
			if !s.final(piece) {
				return
			}
		case sketch != nil:
			s.sketch(sketch, gen)
		case sealed:
			return
		default:
			select {
			case <-s.wake:
			case <-time.After(200 * time.Millisecond):
			case <-s.ctx.Done():
				return
			}
		}
	}
}

// final recognises a committed piece; false when the session can't go on.
func (s *localSession) final(piece []byte) bool {
	text, err := s.proc.transcribe(s.ctx, whisperRequest{Audio: wavFile(piece), Lang: s.lang, Prompt: s.prompt, Beam: s.beam})
	s.mu.Lock()
	if err != nil {
		if s.err == nil && s.ctx.Err() == nil {
			s.err = err
		}
		s.mu.Unlock()
		return false
	}
	if text = tidyWhisper(text); text != "" {
		s.texts = append(s.texts, text)
	}
	s.drafts = s.drafts[1:]
	preview := s.previewLocked()
	s.mu.Unlock()
	s.show(preview)
	return true
}

// sketch recognises the piece being spoken as a draft for the indicator.
// Drafts decode greedily, and after each one the next waits three times as
// long as it took, so a slow CPU isn't kept busy by them.
func (s *localSession) sketch(audio []byte, gen int) {
	began := time.Now()
	text, err := s.proc.transcribe(s.ctx, whisperRequest{Audio: wavFile(audio), Lang: s.lang, Prompt: s.prompt})
	s.mu.Lock()
	s.nextDraft = time.Now().Add(max(draftEvery, 3*time.Since(began)))
	fresh := err == nil && gen == s.commits && !s.sealed // still the piece being spoken
	if fresh {
		s.draft = tidyWhisper(text)
	}
	preview := s.previewLocked()
	s.mu.Unlock()
	if fresh {
		s.show(preview)
	}
}

func (s *localSession) show(preview string) {
	if preview != "" && s.onPartial != nil {
		s.onPartial(preview)
	}
}

// wavFile wraps 24 kHz 16-bit mono PCM in a WAV header; whisper-server
// resamples it to the 16 kHz the model takes.
func wavFile(pcm []byte) []byte {
	b := make([]byte, 44, 44+len(pcm))
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], sampleRate)
	binary.LittleEndian.PutUint32(b[28:], sampleRate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)))
	return append(b, pcm...)
}

// Whisper learned from subtitled videos and sometimes "hears" their credits
// or a sound tag in noise: those never belong in a dictation.
var (
	whisperCredits = regexp.MustCompile(`(?i)(?:редактор субтитров|субтитры|корректор)[^!?\n]*?(?:dimatorzok|amara\.org|синецкая|егорова)[.!?]*|subtitles by the amara\.org community[.!?]*`)
	whisperTags    = regexp.MustCompile(`(?i)\[[^\]\n]{1,40}\]|\((?:музыка|music|аплодисменты|applause|смех|laughter|тишина|silence)\)`)
)

func tidyWhisper(s string) string {
	s = whisperCredits.ReplaceAllString(s, " ")
	s = whisperTags.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	switch strings.ToLower(strings.Trim(s, ".!?… ")) {
	case "", "продолжение следует":
		return ""
	}
	return s
}

// whisperLang is the spoken language for whisper: one known language, or
// "auto" to let it detect the language of each piece.
func (a actionSpec) whisperLang(cfg Config) string {
	if langs := a.languages(cfg); len(langs) == 1 {
		return langs[0]
	}
	return "auto"
}

// whisperPrompt turns the dictionary into a spelling hint.
func whisperPrompt(keywords []string) string {
	kw := cleanKeywords(keywords)
	if len(kw) == 0 {
		return ""
	}
	if len(kw) > 40 {
		kw = kw[:40] // whisper reads at most 224 tokens of prompt
	}
	return strings.Join(kw, ", ") + "."
}
