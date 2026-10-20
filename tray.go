package main

import (
	"runtime"

	"SpeechToText/internal/icon"

	"github.com/energye/systray"
)

var (
	trayIdle = icon.ICO(false, 16, 20, 24, 32, 40, 48)
	trayRec  = icon.ICO(true, 16, 20, 24, 32, 40, 48)
)

// runTray owns the notification-area icon. It runs its own message loop on a
// dedicated OS thread, independent of the Wails UI thread.
func runTray(a *App) {
	runtime.LockOSThread()
	systray.Run(func() {
		systray.SetIcon(trayIdle)
		systray.SetTitle("SpeechToText")
		systray.SetTooltip("SpeechToText — " + a.store.Get().Hotkey.String())
		systray.SetOnClick(func(systray.IMenu) { a.toggleVisible() })
		systray.SetOnDClick(func(systray.IMenu) { a.show() })

		open := systray.AddMenuItem("Открыть", "Показать панель")
		open.Click(a.show)
		systray.AddSeparator()
		quit := systray.AddMenuItem("Выход", "Закрыть SpeechToText")
		quit.Click(a.Quit)
	}, nil)
}

// setTrayRecording turns the tray icon red while the microphone is live.
func setTrayRecording(on bool) {
	if on {
		systray.SetIcon(trayRec)
	} else {
		systray.SetIcon(trayIdle)
	}
}

func setTrayHotkey(hk Hotkey) {
	systray.SetTooltip("SpeechToText — " + hk.String())
}

func quitTray() {
	systray.Quit()
}
