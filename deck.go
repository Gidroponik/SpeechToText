package main

import (
	"embed"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// The Stream Deck plugin ships inside the exe; installing it means handing
// the .streamDeckPlugin package to the Stream Deck app, which asks the user.

//go:embed deck
var deckFS embed.FS

const (
	deckPackage = "deck/com.speechtotext.deck.streamDeckPlugin"
	deckUUID    = "com.speechtotext.deck"
)

type DeckView struct {
	Software  bool `json:"software"`  // the Stream Deck app is installed
	Installed bool `json:"installed"` // our plugin is installed
	Bundled   bool `json:"bundled"`   // this build carries the plugin
}

func deckStatus() DeckView {
	var v DeckView
	_, err := os.Stat(filepath.Join(os.Getenv("ProgramFiles"), "Elgato", "StreamDeck", "StreamDeck.exe"))
	v.Software = err == nil
	dir, _ := os.UserConfigDir()
	_, err = os.Stat(filepath.Join(dir, "Elgato", "StreamDeck", "Plugins", deckUUID+".sdPlugin", "manifest.json"))
	v.Installed = err == nil
	_, err = deckFS.Open(deckPackage)
	v.Bundled = err == nil
	return v
}

// InstallStreamDeck opens the bundled plugin package with the Stream Deck app.
func (a *App) InstallStreamDeck() error {
	data, err := deckFS.ReadFile(deckPackage)
	if err != nil {
		return errors.New("в этой сборке нет плагина: соберите его через streamdeck/build.mjs")
	}
	path := filepath.Join(os.TempDir(), "SpeechToText.streamDeckPlugin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("open")
	file, _ := windows.UTF16PtrFromString(path)
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}
