package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type Settings struct {
	StartMinimized bool `json:"startMinimized"` // on autostart go straight to the tray
	AlwaysOnTop    bool `json:"alwaysOnTop"`
	Sounds         bool `json:"sounds"`      // short beeps when recording starts and stops
	KeepHistory    bool `json:"keepHistory"` // remember recent dictations (encrypted on disk)
}

// ListenConfig is "system audio → text": record what the speakers play.
type ListenConfig struct {
	Hotkey Hotkey `json:"hotkey"`
	Device string `json:"device"` // output endpoint id; "" follows the system default
	Style  string `json:"style"`  // raw | clean | polish | formal
	To     string `json:"to"`     // translate into this language; "" keeps it
	Output string `json:"output"` // show (result window) | clipboard | insert
}

// TranslateBind is a hotkey that turns speech in one language into text in another.
type TranslateBind struct {
	ID     string `json:"id"`
	From   string `json:"from"` // spoken language, "" = any
	To     string `json:"to"`
	Hotkey Hotkey `json:"hotkey"`
}

type Config struct {
	Version int `json:"version"` // config format, see migrate

	// Secrets are stored DPAPI-encrypted (see secret_windows.go), never in plain text.
	APIKey  string `json:"apiKey"`
	Proxy   string `json:"proxy"` // may carry credentials, so it is encrypted too
	BaseURL string `json:"baseUrl"`

	Hotkey Hotkey `json:"hotkey"`
	Mode   string `json:"mode"`  // "toggle" | "hold"
	MicID  string `json:"micId"` // endpoint id; "" follows the system default

	// Transcription (gpt-live-transcribe).
	Delay     string   `json:"delay"` // minimal | low | medium | high | xhigh
	Languages []string `json:"languages"`
	Keywords  []string `json:"keywords"`

	// Clean-up of the raw transcript.
	Style        string `json:"style"` // raw | clean | polish | formal
	Model        string `json:"model"`
	CustomPrompt string `json:"customPrompt"`
	NoFinalDot   bool   `json:"noFinalDot"`

	// Delivery into the focused app.
	Insert           string `json:"insert"` // paste | type | clipboard
	RestoreClipboard bool   `json:"restoreClipboard"`
	PressEnter       bool   `json:"pressEnter"`

	Listen       ListenConfig    `json:"listen"`
	Translations []TranslateBind `json:"translations"`
	Local        LocalConfig     `json:"local"` // whisper on this computer (whisper.go)

	Settings Settings `json:"settings"`
}

const defaultBaseURL = "https://api.openai.com/v1"

func defaultConfig() Config {
	return Config{
		BaseURL:          defaultBaseURL,
		Hotkey:           Hotkey{Ctrl: true, Shift: true, Key: vkSpace},
		Mode:             "toggle",
		Delay:            "high",
		Languages:        []string{"ru", "en"},
		Style:            "polish",
		Model:            defaultModel,
		Insert:           "paste",
		RestoreClipboard: true,
		Version:          configVersion,
		Listen:           ListenConfig{Style: "clean", Output: "show"},
		Local:            LocalConfig{Load: "normal"},
		Settings:         Settings{StartMinimized: true, Sounds: true, KeepHistory: true},
	}
}

const configVersion = 2

// migrate upgrades a config saved by an older version.
func (c *Config) migrate() {
	if c.Version < 2 {
		// 1.1.0 copied system-audio text to the clipboard; it now opens in a window.
		if c.Listen.Output == "clipboard" || c.Listen.Output == "" {
			c.Listen.Output = "show"
		}
	}
	c.Version = configVersion
}

// ConfigStore persists Config as JSON under %APPDATA%\SpeechToText.
type ConfigStore struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "SpeechToText")
}

func NewConfigStore() *ConfigStore {
	s := &ConfigStore{path: filepath.Join(configDir(), "config.json"), cfg: defaultConfig()}
	if data, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(data, &s.cfg)
	}
	s.cfg.migrate()
	return s
}

// Get returns a copy of the current config.
func (s *ConfigStore) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.Languages = slices.Clone(s.cfg.Languages)
	c.Keywords = slices.Clone(s.cfg.Keywords)
	c.Translations = slices.Clone(s.cfg.Translations)
	return c
}

// Update mutates the config under lock and saves it.
func (s *ConfigStore) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
