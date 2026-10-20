package main

import (
	"embed"
	"log"
	"os"
	"slices"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

// instanceID guards against a second copy; test builds override it via
// -ldflags "-X main.instanceID=..." to run next to an installed copy.
var instanceID = "7d3e9a51-2c84-4f1b-b6e0-speechtotext"

func main() {
	hook, err := startKeyHook()
	if err != nil {
		log.Fatalf("keyboard hook: %v", err)
	}
	ov, err := startOverlay()
	if err != nil {
		log.Fatalf("overlay: %v", err)
	}
	args := os.Args[1:]
	app := NewApp(NewConfigStore(), hook, ov, slices.Contains(args, "--autostart"))
	// Started by a command (Stream Deck, a shortcut): run it and stay in the tray.
	if cmd, ok := parseCommand(args); ok {
		app.pending, app.hidden = cmd, true
	}
	if slices.Contains(args, "--hidden") {
		app.hidden = true
	}

	err = wails.Run(&options.App{
		Title:            "SpeechToText",
		Width:            winW,
		Height:           barH,
		Frameless:        true,
		DisableResize:    true,
		StartHidden:      true, // shown from domReady once positioned
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               instanceID,
			OnSecondInstanceLaunch: func(d options.SecondInstanceData) { app.onSecondInstance(d.Args) },
		},
		Windows: &windows.Options{
			// A fully transparent window: the rounded card, its border and
			// shadow are drawn by CSS, so corners can be any radius and stay
			// anti-aliased (DWM only offers a fixed small rounding).
			WebviewIsTransparent:              true,
			WindowIsTranslucent:               true,
			BackdropType:                      windows.None,
			DisableFramelessWindowDecorations: true,
			Theme:                             windows.Dark,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
