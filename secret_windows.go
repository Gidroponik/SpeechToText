package main

import (
	"encoding/base64"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Secrets (the API key, the proxy URL, the history file) are encrypted with
// DPAPI under the current Windows user: only this user on this computer can
// decrypt them, and the plain text never reaches the config file.

// entropy is mixed into DPAPI so other programs running as the same user
// cannot decrypt our blobs with a bare CryptUnprotectData call.
var entropy = []byte("SpeechToText/dpapi/v1:9f2c41e0b7")

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func takeBlob(b *windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(b.Data)))
	return append([]byte(nil), unsafe.Slice(b.Data, b.Size)...)
}

func protect(plain []byte) ([]byte, error) {
	var out windows.DataBlob
	name, _ := windows.UTF16PtrFromString("SpeechToText")
	if err := windows.CryptProtectData(blob(plain), name, blob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

func unprotect(sealed []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(sealed), nil, blob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

// sealString encrypts s for the config file; "" stays "".
func sealString(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := protect([]byte(s))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// openString reverses sealString. A blob from another user or computer
// cannot be decrypted and yields an error.
func openString(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	plain, err := unprotect(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
