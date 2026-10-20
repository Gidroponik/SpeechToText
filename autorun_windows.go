package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// The app runs without administrator rights, so the per-user Run key is
// all autostart needs (no UAC prompt, no scheduled task).
const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "SpeechToText"
)

func autorunEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValueName)
	return err == nil
}

func setAutorun(enable bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enable {
		if err := k.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValueName, `"`+exe+`" --autostart`)
}
