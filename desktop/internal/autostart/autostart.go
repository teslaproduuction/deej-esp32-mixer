// Package autostart toggles "run on Windows login" by writing the
// exe path into HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
// HKCU (current user) means we don't need admin rights to flip it.
package autostart

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	appName = "Mixer"
)

// IsEnabled returns true if our Run entry currently exists.
func IsEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Enable writes the Run entry pointing at the current executable with
// the --hidden flag so the next Windows login starts us straight into
// the tray (no window popping up in the user's face).
func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	value := fmt.Sprintf(`"%s" --hidden`, exe)
	return k.SetStringValue(appName, value)
}

func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(appName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// IsHiddenStart returns true if the process was started with --hidden,
// which is what our own Run entry passes. Used by main.go to decide
// StartHidden.
func IsHiddenStart() bool {
	for _, a := range os.Args[1:] {
		if strings.EqualFold(a, "--hidden") {
			return true
		}
	}
	return false
}
