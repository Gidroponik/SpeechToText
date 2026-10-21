package main

import (
	"fmt"
	"strings"
)

// An action is one thing a hotkey, the panel or a Stream Deck button can
// start: dictation, system audio → text, or a translation.

type Language struct {
	Code   string `json:"code"`
	Native string `json:"native"`
}

// languages offered for translation; the transcription model accepts more.
var languages = []Language{
	{"ru", "Русский"}, {"en", "English"}, {"uk", "Українська"}, {"be", "Беларуская"}, {"kk", "Қазақша"},
	{"de", "Deutsch"}, {"fr", "Français"}, {"es", "Español (España)"}, {"it", "Italiano"}, {"pt", "Português"},
	{"pl", "Polski"}, {"tr", "Türkçe"}, {"zh", "中文"}, {"ja", "日本語"}, {"ko", "한국어"}, {"ar", "العربية"},
}

// langVariants pins the regional variant a translation must use.
var langVariants = map[string]string{
	"es": "вариант Испании (кастильский), не латиноамериканский: обращение к нескольким людям — vosotros/vuestro, " +
		"лексика Испании (ordenador, móvil, coche, conducir, vale, zumo, gafas, coger), европейская орфография и пунктуация",
}

func langKnown(code string) bool {
	for _, l := range languages {
		if l.Code == code {
			return true
		}
	}
	return false
}

func langNative(code string) string {
	for _, l := range languages {
		if l.Code == code {
			return l.Native
		}
	}
	return code
}

type actionSpec struct {
	ID     string // dictate | listen | tr:<id> | translate (ad hoc, e.g. from Stream Deck)
	Source string // mic | system
	From   string // spoken language hint, "" = from settings / any
	To     string // translate into, "" = no translation
	Style  string // clean-up style, "" = the one from the text panel
	Output string // insert | clipboard | show
	Local  bool   // recognise with whisper whatever the settings say (the offline panel's test)
}

func validOutput(o string) bool { return o == "insert" || o == "clipboard" || o == "show" }

func (a actionSpec) Label() string {
	to := ""
	if a.To != "" {
		to = " → " + strings.ToUpper(a.To)
	}
	switch {
	case a.Source == "system":
		return "Звук системы" + to
	case a.To != "":
		from := ""
		if a.From != "" {
			from = " " + strings.ToUpper(a.From)
		}
		return "Перевод" + from + to
	}
	return "Диктовка"
}

// resolveAction finds an action by id. Ad hoc translations ("translate")
// take their languages and source from the request.
func resolveAction(cfg Config, id string, adhoc actionSpec) (actionSpec, error) {
	switch {
	case id == "" || id == "dictate":
		return actionSpec{ID: "dictate", Source: "mic", Output: "insert"}, nil
	case id == "listen":
		a := actionSpec{ID: "listen", Source: "system", To: cfg.Listen.To, Style: cfg.Listen.Style, Output: cfg.Listen.Output}
		if adhoc.To != "" {
			a.To = adhoc.To
		}
		if validOutput(adhoc.Output) {
			a.Output = adhoc.Output
		}
		return a, nil
	case id == "translate":
		a := adhoc
		a.ID = id
		if a.Source != "system" {
			a.Source = "mic"
		}
		if !validOutput(a.Output) {
			a.Output = "insert"
		}
		if !langKnown(a.To) {
			return a, fmt.Errorf("неизвестный язык перевода %q", a.To)
		}
		if a.From != "" && !langKnown(a.From) {
			return a, fmt.Errorf("неизвестный язык речи %q", a.From)
		}
		return a, nil
	case strings.HasPrefix(id, "tr:"):
		for _, t := range cfg.Translations {
			if "tr:"+t.ID == id {
				return actionSpec{ID: id, Source: "mic", From: t.From, To: t.To, Output: "insert"}, nil
			}
		}
	}
	return actionSpec{}, fmt.Errorf("нет такого действия: %s", id)
}

// hotkeyBindings lists every assigned hotkey for the keyboard hook.
func hotkeyBindings(cfg Config) []bindSpec {
	specs := []bindSpec{{"dictate", cfg.Hotkey}, {"listen", cfg.Listen.Hotkey}}
	for _, t := range cfg.Translations {
		specs = append(specs, bindSpec{"tr:" + t.ID, t.Hotkey})
	}
	return specs
}

// hotkeyOwner names the action already using hk, if any (except id itself).
func hotkeyOwner(cfg Config, hk Hotkey, id string) string {
	for _, b := range hotkeyBindings(cfg) {
		if b.id != id && b.hk.Key != 0 && b.hk == hk {
			a, _ := resolveAction(cfg, b.id, actionSpec{})
			return a.Label()
		}
	}
	return ""
}

// setActionHotkey stores hk for the action id.
func setActionHotkey(c *Config, id string, hk Hotkey) bool {
	switch id {
	case "dictate":
		c.Hotkey = hk
		return true
	case "listen":
		c.Listen.Hotkey = hk
		return true
	}
	for i := range c.Translations {
		if "tr:"+c.Translations[i].ID == id {
			c.Translations[i].Hotkey = hk
			return true
		}
	}
	return false
}

// ActionInfo describes an action for the panel and Stream Deck.
type ActionInfo struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Hotkey []string `json:"hotkey"`
}

func actionList(cfg Config) []ActionInfo {
	var out []ActionInfo
	for _, b := range hotkeyBindings(cfg) {
		a, err := resolveAction(cfg, b.id, actionSpec{})
		if err != nil {
			continue
		}
		parts := b.hk.Parts()
		if parts == nil {
			parts = []string{}
		}
		out = append(out, ActionInfo{ID: b.id, Label: a.Label(), Hotkey: parts})
	}
	return out
}
