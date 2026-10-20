package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The control API lets the Stream Deck plugin (or any local tool) start and
// stop actions and follow the state. It listens on 127.0.0.1 only, on a
// random port, and every request must carry the token from control.json.
// Requests from browsers are refused: they would need a CORS preflight
// (custom Authorization header) that is never granted, and carry an Origin.

// LiveState is the dictation state shown by the panel and Stream Deck keys.
type LiveState struct {
	Phase  string `json:"phase"`  // idle | recording | processing
	Action string `json:"action"` // id of the running action
	Source string `json:"source"` // mic | system
	To     string `json:"to"`     // translation target of the running action
	Since  int64  `json:"since"`  // unix ms when recording started
	HasKey bool   `json:"hasKey"`
}

// command is one request: from the control API or the command line.
type command struct {
	Op     string `json:"op"`     // toggle | start | stop | cancel
	Action string `json:"action"` // dictate | listen | translate | tr:<id>
	To     string `json:"to"`
	From   string `json:"from"`
	Source string `json:"source"`
	Output string `json:"output"`
}

type controlServer struct {
	app   *App
	token string
	port  int
	srv   *http.Server

	mu   sync.Mutex
	subs map[chan []byte]struct{}
	last []byte
}

func controlFile() string { return filepath.Join(configDir(), "control.json") }

func startControl(app *App) (*controlServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	c := &controlServer{app: app, token: hex.EncodeToString(b), port: ln.Addr().(*net.TCPAddr).Port, subs: map[chan []byte]struct{}{}}
	c.last, _ = json.Marshal(LiveState{Phase: "idle"})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) { c.writeState(w) })
	mux.HandleFunc("GET /v1/events", c.events)
	mux.HandleFunc("GET /v1/actions", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusOK, map[string]any{"actions": actionList(app.store.Get()), "languages": languages})
	})
	mux.HandleFunc("POST /v1/action", c.action)
	c.srv = &http.Server{Handler: c.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = c.srv.Serve(ln) }()

	exe, _ := os.Executable()
	info, _ := json.MarshalIndent(map[string]any{
		"port": c.port, "token": c.token, "pid": os.Getpid(), "exe": exe, "version": version,
	}, "", "  ")
	_ = os.MkdirAll(configDir(), 0o755)
	if err := os.WriteFile(controlFile(), info, 0o600); err != nil {
		return c, fmt.Errorf("control.json: %w", err)
	}
	return c, nil
}

// guard admits only local, token-bearing, non-browser requests.
func (c *controlServer) guard(next http.Handler) http.Handler {
	hosts := []string{fmt.Sprintf("127.0.0.1:%d", c.port), fmt.Sprintf("localhost:%d", c.port)}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || (r.Host != hosts[0] && r.Host != hosts[1]) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (c *controlServer) writeState(w http.ResponseWriter) {
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(last)
}

// publish sends a new state to every subscriber.
func (c *controlServer) publish(s LiveState) {
	b, _ := json.Marshal(s)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = b
	for ch := range c.subs {
		select {
		case ch <- b:
		default: // a slow reader gets the next one
		}
	}
}

// events streams states as Server-Sent Events.
func (c *controlServer) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 8)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	first := c.last
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, "data: %s\n\n", first)
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		fl.Flush()
	}
}

func (c *controlServer) action(w http.ResponseWriter, r *http.Request) {
	var cmd command
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&cmd); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if err := c.app.run(cmd); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c.writeState(w)
}

func (c *controlServer) close() {
	_ = c.srv.Close()
}

// ---------- command line ----------

// parseCommand reads e.g. `--toggle dictate`, `--start translate --to en`,
// `--stop` or `--cancel` from the command line.
func parseCommand(args []string) (command, bool) {
	var cmd command
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() string {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
				return args[i]
			}
			return ""
		}
		switch arg {
		case "--toggle", "--start":
			cmd.Op = arg[2:]
			cmd.Action = next()
		case "--stop", "--cancel":
			cmd.Op = arg[2:]
		case "--to":
			cmd.To = next()
		case "--from":
			cmd.From = next()
		case "--source":
			cmd.Source = next()
		case "--output":
			cmd.Output = next()
		}
	}
	return cmd, cmd.Op != ""
}

// run executes a command against the dictation.
func (a *App) run(cmd command) error {
	switch cmd.Op {
	case "stop":
		a.dict.Stop()
		return nil
	case "cancel":
		a.dict.Cancel()
		return nil
	case "toggle", "start":
	default:
		return fmt.Errorf("неизвестная команда %q", cmd.Op)
	}
	if cmd.Action == "" {
		cmd.Action = "dictate"
	}
	if cmd.Action != "translate" && cmd.To != "" && !langKnown(cmd.To) {
		return errors.New("неизвестный язык перевода")
	}
	spec, err := resolveAction(a.store.Get(), cmd.Action, actionSpec{To: cmd.To, From: cmd.From, Source: cmd.Source, Output: cmd.Output})
	if err != nil {
		return err
	}
	if cmd.Op == "toggle" {
		a.dict.Toggle(spec)
	} else {
		a.dict.Start(phIdle, spec, false)
	}
	return nil
}
