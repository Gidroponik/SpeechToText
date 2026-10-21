package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	whisperIdle    = 5 * time.Minute // unload the model after this long unused
	whisperStartup = time.Minute     // loading a model, antivirus scans of fresh DLLs included
)

// whisperServer owns the whisper-server process: started for the current
// runtime, model and thread count, restarted when they change, and stopped
// after whisperIdle so the model doesn't sit in RAM or video memory.
type whisperServer struct {
	start func(LocalConfig) (*whisperProc, error) // startWhisper; tests swap it

	mu    sync.Mutex
	proc  *whisperProc
	users int
	idle  *time.Timer
}

// procKey identifies what a running server was started with.
func procKey(l LocalConfig) string {
	return fmt.Sprintf("%s|%s|%d|%t", l.runtime().ID, l.Model, whisperThreads(l.Load), vadModel.installed())
}

// acquire returns a server for l, starting one if needed; release must be
// called once the caller is done with it. The server may still be loading
// the model: transcribe waits for it.
func (s *whisperServer) acquire(l LocalConfig) (*whisperProc, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A changed setting restarts the server, unless a take is still using it.
	if s.proc != nil && (!s.proc.alive() || s.proc.key != procKey(l) && s.users == 0) {
		s.proc.kill()
		s.proc = nil
	}
	if s.proc == nil {
		p, err := s.start(l)
		if err != nil {
			return nil, nil, err
		}
		p.key = procKey(l)
		s.proc = p
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	s.users++
	var once sync.Once
	return s.proc, func() { once.Do(s.release) }, nil
}

func (s *whisperServer) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users--; s.users == 0 && s.proc != nil {
		p := s.proc
		s.idle = time.AfterFunc(whisperIdle, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.users == 0 && s.proc == p {
				p.kill()
				s.proc = nil
			}
		})
	}
}

// loaded is the running server's key, "" when none is running.
func (s *whisperServer) loaded() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil || !s.proc.alive() {
		return ""
	}
	return s.proc.key
}

func (s *whisperServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle != nil {
		s.idle.Stop()
	}
	if s.proc != nil {
		s.proc.kill()
		s.proc = nil
	}
}

// whisperProc is one running whisper-server.
type whisperProc struct {
	key    string
	base   string        // http://127.0.0.1:port
	ready  chan struct{} // closed once the model is loaded or the start failed
	err    error         // why it failed; set before ready is closed
	cmd    *exec.Cmd
	exited chan struct{}
	out    *tailWriter
	client *http.Client
}

func (p *whisperProc) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *whisperProc) kill() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func startWhisper(l LocalConfig) (*whisperProc, error) {
	rt := l.runtime()
	m, ok := findWhisperModel(l.Model)
	if !ok {
		return nil, fmt.Errorf("неизвестная модель %q", l.Model)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{"-m", m.path(), "--host", "127.0.0.1", "--port", strconv.Itoa(port), "-t", strconv.Itoa(whisperThreads(l.Load))}
	if vadModel.installed() {
		args = append(args, "--vad", "-vm", vadModel.path())
	}
	cmd := exec.Command(filepath.Join(rt.path(), "whisper-server.exe"), args...)
	cmd.Dir = rt.path()
	cmd.Env = os.Environ()
	if os.Getenv("CUDA_CACHE_MAXSIZE") == "" {
		// Room for the kernels the driver compiles for new cards, so it does that once.
		cmd.Env = append(cmd.Env, "CUDA_CACHE_MAXSIZE=4294967296")
	}
	out := &tailWriter{}
	cmd.Stdout, cmd.Stderr = out, out
	// No console window, and below-normal priority: a game or a call keeps the CPU first.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.BELOW_NORMAL_PRIORITY_CLASS}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не удалось запустить whisper-server: %v", err)
	}
	killWithApp(cmd.Process.Pid)
	p := &whisperProc{
		base: "http://127.0.0.1:" + strconv.Itoa(port), ready: make(chan struct{}),
		cmd: cmd, exited: make(chan struct{}), out: out,
		client: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	go p.waitReady()
	return p, nil
}

func (p *whisperProc) waitReady() {
	defer close(p.ready)
	check := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	deadline := time.Now().Add(whisperStartup)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			p.err = errors.New("whisper-server не запустился: " + p.out.lastLine())
			return
		case <-time.After(100 * time.Millisecond):
		}
		if resp, err := check.Get(p.base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
	p.err = errors.New("whisper-server не загрузил модель за минуту")
	p.kill()
}

type whisperRequest struct {
	Audio  []byte // a WAV file
	Lang   string // a language code or "auto"
	Prompt string // spelling hints: names and terms
	Beam   int    // beam search width, 0 = greedy
	NoVAD  bool
}

// transcribe sends one piece of audio, waiting for the model to load first.
func (p *whisperProc) transcribe(ctx context.Context, r whisperRequest) (string, error) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if p.err != nil {
		return "", p.err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write(r.Audio)
	fields := [][2]string{{"language", r.Lang}, {"response_format", "json"}, {"temperature", "0"}, {"no_timestamps", "true"}}
	if r.Prompt != "" {
		fields = append(fields, [2]string{"prompt", r.Prompt})
	}
	if r.Beam > 0 {
		fields = append(fields, [2]string{"beam_size", strconv.Itoa(r.Beam)})
	}
	if r.NoVAD {
		fields = append(fields, [2]string{"vad", "false"})
	}
	for _, f := range fields {
		_ = mw.WriteField(f[0], f[1])
	}
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", p.base+"/inference", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !p.alive() {
			return "", errors.New("whisper-server остановился: " + p.out.lastLine())
		}
		return "", fmt.Errorf("whisper-server не ответил: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var res struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &res); err != nil || res.Error != "" || resp.StatusCode != http.StatusOK {
		msg := res.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", errors.New("whisper-server: " + msg)
	}
	return res.Text, nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// tailWriter keeps the end of whisper-server's output for error messages.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailWriter) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > 8192 {
		t.buf = t.buf[len(t.buf)-8192:]
	}
	return len(b), nil
}

func (t *tailWriter) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "без сообщения об ошибке"
}

var (
	jobOnce sync.Once
	job     windows.Handle
)

// killWithApp puts the process into a job object that Windows closes when
// the app exits for any reason, a crash included, taking the server along.
func killWithApp(pid int) {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(h)
			return
		}
		job = h
	})
	if job == 0 {
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(ph)
	_ = windows.AssignProcessToJobObject(job, ph)
}

var procEnumDisplayDevicesW = user32.NewProc("EnumDisplayDevicesW")

type displayDevice struct {
	Cb           uint32
	DeviceName   [32]uint16
	DeviceString [128]uint16
	StateFlags   uint32
	DeviceID     [128]uint16
	DeviceKey    [128]uint16
}

// nvidiaGPU names the NVIDIA card the CUDA build can use, "" when there is
// none (no card, or a driver without CUDA).
var nvidiaGPU = sync.OnceValue(func() string {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(sys, "nvcuda.dll")); err != nil {
		return ""
	}
	for i := uintptr(0); i < 32; i++ {
		var d displayDevice
		d.Cb = uint32(unsafe.Sizeof(d))
		if r, _, _ := procEnumDisplayDevicesW.Call(0, i, uintptr(unsafe.Pointer(&d)), 0); r == 0 {
			break
		}
		if name := windows.UTF16ToString(d.DeviceString[:]); strings.Contains(strings.ToUpper(name), "NVIDIA") {
			return name
		}
	}
	return "NVIDIA"
})
