//go:build stdebug

package main

import (
	"os"
	"time"
)

// Debug builds (-tags stdebug) open the panel named in STT_PANEL on start and,
// with STT_OVERLAY=1, cycle the dictation indicator through its states, so
// the UI can be checked without touching the keyboard.
func init() {
	debugHook = func(a *App) {
		if p := os.Getenv("STT_PANEL"); p != "" {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				a.emit("open-panel", p)
			}()
		}
		if os.Getenv("STT_OVERLAY") == "1" {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				o := a.overlay
				o.Show(overlayFrame{Kind: ovListening, Text: "Говорите…", Dim: true, Hint: "0:00"}, 0)
				for i := range 40 {
					time.Sleep(100 * time.Millisecond)
					o.Update(func(f *overlayFrame) {
						f.Level = float64(i%10) / 10
						f.Hint = fmtDuration(time.Duration(i) * 100 * time.Millisecond)
						if i > 15 {
							f.Text, f.Dim = "привет как дела я хотел сказать что завтра встречаемся в три", false
						}
					})
				}
				o.Show(overlayFrame{Kind: ovWorking, Text: "Улучшаю текст…"}, 0)
				time.Sleep(2 * time.Second)
				o.Show(overlayFrame{Kind: ovDone, Text: "Вставлено"}, 1500*time.Millisecond)
			}()
		}
	}
}
