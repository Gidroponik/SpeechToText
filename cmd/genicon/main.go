// Command genicon regenerates the icon files used by the build and the UI:
//
//	go run ./cmd/genicon
package main

import (
	"bytes"
	"image/png"
	"log"
	"os"

	"SpeechToText/internal/icon"
)

func main() {
	writePNG("build/appicon.png", 1024)
	writePNG("frontend/dist/img/logo-mark.png", 128)
	write("build/windows/icon.ico", icon.ICO(false, 16, 20, 24, 32, 40, 48, 64, 256))
	writePNG("streamdeck/com.speechtotext.deck.sdPlugin/imgs/plugin/marketplace.png", 256)
	writePNG("streamdeck/com.speechtotext.deck.sdPlugin/imgs/plugin/marketplace@2x.png", 512)
}

func writePNG(path string, size int) {
	var b bytes.Buffer
	if err := png.Encode(&b, icon.Draw(size, false)); err != nil {
		log.Fatal(err)
	}
	write(path, b.Bytes())
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("%s (%d bytes)", path, len(data))
}
