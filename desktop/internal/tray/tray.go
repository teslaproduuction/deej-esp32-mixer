// Package tray drives the Windows notification-area icon. The Wails
// window is hidden (not destroyed) on close via options.HideWindowOnClose,
// and the tray menu is the only way to bring it back or quit the process.
package tray

import (
	_ "embed"

	"github.com/energye/systray"
)

//go:embed icon.ico
var iconData []byte

// Callbacks are the actions the tray menu invokes back into the App.
// They are wired in app.go on startup.
type Callbacks struct {
	OnOpen   func()
	OnReload func()
	OnQuit   func()
}

// Run blocks the calling goroutine until OnQuit fires. Always launch
// this in its own goroutine; energye's fork handles the Win32 message
// pump internally without needing the main thread.
func Run(cb Callbacks) {
	systray.Run(func() { onReady(cb) }, func() {})
}

func onReady(cb Callbacks) {
	systray.SetIcon(iconData)
	systray.SetTitle("Mixer")
	systray.SetTooltip("Mixer — аппаратный регулятор громкости")

	// Left/double click on the icon also opens the window — more
	// natural than forcing the user into the right-click menu.
	systray.SetOnClick(func(menu systray.IMenu) {
		if cb.OnOpen != nil {
			cb.OnOpen()
		}
	})
	systray.SetOnDClick(func(menu systray.IMenu) {
		if cb.OnOpen != nil {
			cb.OnOpen()
		}
	})
	systray.SetOnRClick(func(menu systray.IMenu) {
		_ = menu.ShowMenu()
	})

	mOpen := systray.AddMenuItem("Открыть", "Показать окно")
	mOpen.Click(func() {
		if cb.OnOpen != nil {
			cb.OnOpen()
		}
	})

	mReload := systray.AddMenuItem("Перезагрузить конфиг", "Перечитать config.yaml")
	mReload.Click(func() {
		if cb.OnReload != nil {
			cb.OnReload()
		}
	})

	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Выход", "Завершить mixer")
	mQuit.Click(func() {
		if cb.OnQuit != nil {
			cb.OnQuit()
		}
	})
}
