package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const historyLimit = 50

type HistoryItem struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind,omitempty"` // "Перевод → EN", "Звук системы"; empty for dictation
	Raw     string    `json:"raw"`            // what was recognised
	Text    string    `json:"text"`           // what was inserted
	Seconds float64   `json:"seconds"`
	Cost    float64   `json:"cost"` // USD, transcription + clean-up
	Note    string    `json:"note,omitempty"`
}

type Usage struct {
	Seconds float64 `json:"seconds"`
	Cost    float64 `json:"cost"`
	Count   int     `json:"count"`
}

type historyData struct {
	Items []HistoryItem    `json:"items"` // newest first
	Usage map[string]Usage `json:"usage"` // by month, "2026-10"
}

// History keeps recent dictations and monthly usage. The file holds what was
// dictated, so it is DPAPI-encrypted like the API key.
type History struct {
	mu   sync.Mutex
	path string
	data historyData
}

func NewHistory() *History {
	h := &History{path: filepath.Join(configDir(), "history.dat"), data: historyData{Usage: map[string]Usage{}}}
	if sealed, err := os.ReadFile(h.path); err == nil {
		if plain, err := unprotect(sealed); err == nil {
			_ = json.Unmarshal(plain, &h.data)
		}
	}
	if h.data.Usage == nil {
		h.data.Usage = map[string]Usage{}
	}
	return h
}

func (h *History) save() {
	plain, err := json.Marshal(h.data)
	if err != nil {
		return
	}
	sealed, err := protect(plain)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(h.path), 0o755)
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, sealed, 0o600) == nil {
		_ = os.Rename(tmp, h.path)
	}
}

// Add records a dictation; the text itself is kept only when keep is set,
// usage always is.
func (h *History) Add(it HistoryItem, keep bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	month := it.Time.Format("2006-01")
	u := h.data.Usage[month]
	u.Seconds += it.Seconds
	u.Cost += it.Cost
	u.Count++
	h.data.Usage[month] = u
	if keep {
		h.data.Items = append([]HistoryItem{it}, h.data.Items...)
		if len(h.data.Items) > historyLimit {
			h.data.Items = h.data.Items[:historyLimit]
		}
	}
	h.save()
}

func (h *History) Items() []HistoryItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.data.Items)
}

func (h *History) Month(t time.Time) Usage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.data.Usage[t.Format("2006-01")]
}

// Clear forgets the texts; usage counters stay.
func (h *History) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.data.Items = nil
	h.save()
}
