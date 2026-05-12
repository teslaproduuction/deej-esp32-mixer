// Package notify shows Windows toast notifications. Used for serial
// connect/disconnect/error events so the user gets a visual cue even
// when the main window is hidden in the tray.
package notify

import (
	toast "git.sr.ht/~jackmordaunt/go-toast/v2"
)

const appID = "Mixer"

// Info shows a regular informational toast.
func Info(title, message string) {
	n := toast.Notification{AppID: appID, Title: title, Body: message}
	_ = n.Push()
}

// Warn shows a toast with a "warning" framing — same machinery, just
// a different title prefix because toast.Notification has no severity.
func Warn(title, message string) {
	n := toast.Notification{AppID: appID, Title: "⚠ " + title, Body: message}
	_ = n.Push()
}
