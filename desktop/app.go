package main

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"mixer/internal/autostart"
	maudio "mixer/internal/audio"
	mconfig "mixer/internal/config"
	"mixer/internal/notify"
	mserial "mixer/internal/serial"
	mtray "mixer/internal/tray"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the bound object exposed to the Svelte frontend.
type App struct {
	ctx    context.Context
	mu     sync.RWMutex
	cfg    mconfig.Config
	reader *mserial.Reader
	apply  chan [mserial.NumSliders]int

	// Calibration state. When `calibrating` is true the applier just
	// records min/max into `cal` instead of routing audio.
	calMu       sync.Mutex
	calibrating bool
	cal         [mserial.NumSliders]mconfig.Calibration
}

// changeThreshold suppresses redundant SetVolume calls. ESP32 ADC has
// ±15-30 raw-unit noise even with the firmware's median smoothing, so
// a threshold below ~0.03 (≈30 raw units) ghosts movements and would
// also trigger spurious auto-unmute on muted sessions.
const changeThreshold float32 = 0.03

func NewApp() *App {
	return &App{
		reader: mserial.New(),
		apply:  make(chan [mserial.NumSliders]int, 4),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	cfg, created, err := mconfig.Load()
	if err != nil {
		wruntime.LogErrorf(ctx, "config load: %v", err)
		cfg = mconfig.Default()
	}
	if cfg.Calibration == nil {
		cfg.Calibration = mconfig.DefaultCalibration()
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()

	if created {
		path, _ := mconfig.Path()
		wruntime.LogInfof(ctx, "created default config at %s", path)
	}

	go a.pumpReader()
	go a.applier()
	go mtray.Run(mtray.Callbacks{
		OnOpen:   func() { wruntime.WindowShow(a.ctx) },
		OnReload: func() { _, _ = a.ReloadConfig() },
		OnQuit:   func() { wruntime.Quit(a.ctx) },
	})

	if cfg.ComPort != "" {
		go func() {
			if err := a.reader.Start(cfg.ComPort, cfg.BaudRate); err != nil {
				wruntime.EventsEmit(ctx, "serial-error", "auto-connect: "+err.Error())
				notify.Warn("Mixer", "Не удалось подключиться к "+cfg.ComPort+": "+err.Error())
				return
			}
			_ = a.reader.Send(fmt.Sprintf("MODE:%d", cfg.LedMode))
			notify.Info("Mixer", "Подключено: "+cfg.ComPort)
		}()
	}
}

func (a *App) pumpReader() {
	values := a.reader.ValuesCh()
	errs := a.reader.ErrorsCh()
	for {
		select {
		case <-a.ctx.Done():
			return
		case v := <-values:
			wruntime.EventsEmit(a.ctx, "slider-values", v[:])
			select {
			case a.apply <- v:
			default:
			}
		case e := <-errs:
			wruntime.EventsEmit(a.ctx, "serial-error", e.Error())
		}
	}
}

func (a *App) applier() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := maudio.InitCOM(); err != nil {
		wruntime.EventsEmit(a.ctx, "audio-error", "init COM: "+err.Error())
		return
	}
	defer maudio.Uninit()

	var last [mserial.NumSliders]float32
	for i := range last {
		last[i] = -1
	}
	var lastForeground string

	// Drive meter mode at ~25 fps. The firmware redraws faster than
	// this, but going above ~40 Hz for a serial line with text frames
	// just wastes bandwidth.
	meterTicker := time.NewTicker(40 * time.Millisecond)
	defer meterTicker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case v := <-a.apply:
			if a.recordCalibration(v) {
				continue
			}
			for i, raw := range v {
				norm := a.normalize(i, raw)
				if absDiff(norm, last[i]) < changeThreshold {
					continue
				}
				last[i] = norm
				a.applySlider(i, norm, &lastForeground)
			}
		case <-meterTicker.C:
			a.pushMeter(lastForeground)
		}
	}
}

// pushMeter is a no-op unless we're connected, in meter mode, and have
// some mapping to read peaks for. It enumerates Core Audio sessions
// once and emits a single "M:p1,p2,p3,p4,p5" downlink line.
func (a *App) pushMeter(currentForeground string) {
	a.mu.RLock()
	mode := a.cfg.LedMode
	mapping := a.cfg.SliderMapping
	a.mu.RUnlock()

	if mode != 2 || !a.reader.IsRunning() {
		return
	}
	peaks, err := maudio.ReadPeakLevels(mapping, currentForeground, mserial.NumSliders)
	if err != nil {
		return
	}
	// 1023 chosen to mirror the firmware's ADC range so the
	// existing showLights() math fills the bar as if it were a
	// physical slider at that position.
	var sb [64]byte
	out := sb[:0]
	out = append(out, "M:"...)
	for i, p := range peaks {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, fmt.Sprintf("%d", int(p*1023))...)
	}
	_ = a.reader.Send(string(out))
}

// recordCalibration updates per-slider min/max when in calibration mode
// and emits progress to the GUI. Returns true if we are calibrating
// (so the caller skips audio side effects this frame).
func (a *App) recordCalibration(v [mserial.NumSliders]int) bool {
	a.calMu.Lock()
	if !a.calibrating {
		a.calMu.Unlock()
		return false
	}
	for i, raw := range v {
		if raw < a.cal[i].Min {
			a.cal[i].Min = raw
		}
		if raw > a.cal[i].Max {
			a.cal[i].Max = raw
		}
	}
	snapshot := a.cal
	a.calMu.Unlock()

	wruntime.EventsEmit(a.ctx, "calibration-progress", snapshot[:])
	return true
}

// normalize converts a raw 0..1023 ADC reading into a 0..1 volume,
// using the per-slider calibration plus the global invert / noise
// reduction settings from config.
func (a *App) normalize(idx, raw int) float32 {
	a.mu.RLock()
	inv := a.cfg.InvertSliders
	dz := a.cfg.NoiseReduction
	cal, ok := a.cfg.Calibration[idx]
	a.mu.RUnlock()

	if !ok || cal.Max-cal.Min < 50 {
		cal = mconfig.Calibration{Min: 0, Max: 1023}
	}

	if raw < cal.Min+dz {
		raw = cal.Min
	}
	if raw > cal.Max {
		raw = cal.Max
	}

	v := float32(raw-cal.Min) / float32(cal.Max-cal.Min)
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	if inv {
		v = 1 - v
	}
	return v
}

func (a *App) applySlider(idx int, level float32, lastForeground *string) {
	a.mu.RLock()
	targets := append([]string(nil), a.cfg.SliderMapping[idx]...)
	a.mu.RUnlock()

	for _, t := range targets {
		switch t {
		case maudio.TargetMaster:
			if err := maudio.SetMasterVolume(level); err != nil {
				wruntime.EventsEmit(a.ctx, "audio-error", "master: "+err.Error())
			}
		case maudio.TargetForeground:
			if name := maudio.GetForegroundProcessName(); name != "" {
				*lastForeground = name
			}
			if *lastForeground == "" {
				continue
			}
			if _, err := maudio.SetVolumeByExe(*lastForeground, level); err != nil {
				wruntime.EventsEmit(a.ctx, "audio-error", *lastForeground+": "+err.Error())
			}
		default:
			if _, err := maudio.SetVolumeByExe(t, level); err != nil {
				wruntime.EventsEmit(a.ctx, "audio-error", t+": "+err.Error())
			}
		}
	}
}

func absDiff(a, b float32) float32 {
	if a > b {
		return a - b
	}
	return b - a
}

// ---------- Serial ----------

func (a *App) ListPorts() ([]string, error) { return mserial.ListPorts() }
func (a *App) Connect(port string) error {
	a.mu.RLock()
	baud := a.cfg.BaudRate
	mode := a.cfg.LedMode
	a.mu.RUnlock()
	if err := a.reader.Start(port, baud); err != nil {
		return err
	}
	_ = a.reader.Send(fmt.Sprintf("MODE:%d", mode))
	return nil
}
func (a *App) Disconnect()                  { a.reader.Stop() }
func (a *App) IsConnected() bool            { return a.reader.IsRunning() }
func (a *App) SendCommand(cmd string) error { return a.reader.Send(cmd) }

// ---------- Audio ----------

func (a *App) ListAudioSessions() ([]maudio.Session, error) {
	type result struct {
		sessions []maudio.Session
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := maudio.InitCOM(); err != nil {
			ch <- result{nil, err}
			return
		}
		defer maudio.Uninit()
		ss, err := maudio.ListSessions()
		ch <- result{ss, err}
	}()
	r := <-ch
	return r.sessions, r.err
}

// ---------- Config ----------

func (a *App) GetConfig() mconfig.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *App) SaveConfig(cfg mconfig.Config) error {
	if cfg.BaudRate == 0 {
		cfg.BaudRate = mconfig.DefaultBaud
	}
	if cfg.Calibration == nil {
		cfg.Calibration = mconfig.DefaultCalibration()
	}
	if err := mconfig.Save(cfg); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return nil
}

func (a *App) ReloadConfig() (mconfig.Config, error) {
	cfg, _, err := mconfig.Load()
	if err != nil {
		return mconfig.Config{}, err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return cfg, nil
}

func (a *App) ConfigPath() (string, error) { return mconfig.Path() }

// ---------- LED Mode ----------

// SetLEDMode pushes the new mode to the firmware (if connected) and
// persists it in config so the next launch starts there too.
//   0 — position (default)
//   1 — rainbow
//   2 — meter (PC drives strips with peak meters)
func (a *App) SetLEDMode(mode int) error {
	if mode < 0 || mode > 2 {
		return fmt.Errorf("led mode out of range: %d", mode)
	}
	if a.reader.IsRunning() {
		if err := a.reader.Send(fmt.Sprintf("MODE:%d", mode)); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.cfg.LedMode = mode
	cfgCopy := a.cfg
	a.mu.Unlock()
	return mconfig.Save(cfgCopy)
}

// ---------- Autostart ----------

func (a *App) GetAutostart() (bool, error) { return autostart.IsEnabled() }

func (a *App) SetAutostart(enable bool) error {
	if enable {
		return autostart.Enable()
	}
	return autostart.Disable()
}

// ---------- Calibration ----------

// StartCalibration freezes audio routing and starts recording per-slider
// extremes. The GUI should ask the user to move every slider to both
// hard stops, then call StopCalibration to persist the result.
func (a *App) StartCalibration() {
	a.calMu.Lock()
	a.calibrating = true
	for i := range a.cal {
		// Seed the search with "impossible" values so the first frame
		// always replaces both bounds.
		a.cal[i] = mconfig.Calibration{Min: 9999, Max: -1}
	}
	a.calMu.Unlock()
}

// StopCalibration saves the observed min/max into the config and exits
// calibration mode. Returns the persisted calibration map for the GUI.
func (a *App) StopCalibration() (map[int]mconfig.Calibration, error) {
	a.calMu.Lock()
	a.calibrating = false
	snapshot := a.cal
	a.calMu.Unlock()

	result := make(map[int]mconfig.Calibration, mserial.NumSliders)
	for i, c := range snapshot {
		if c.Max-c.Min < 50 {
			c = mconfig.Calibration{Min: 0, Max: 1023}
		}
		result[i] = c
	}

	a.mu.Lock()
	a.cfg.Calibration = result
	cfgCopy := a.cfg
	a.mu.Unlock()

	if err := mconfig.Save(cfgCopy); err != nil {
		return nil, err
	}
	return result, nil
}

// CancelCalibration leaves calibration mode without saving.
func (a *App) CancelCalibration() {
	a.calMu.Lock()
	a.calibrating = false
	a.calMu.Unlock()
}

// ResetCalibration restores the default 0..1023 range for all sliders
// and saves it to disk.
func (a *App) ResetCalibration() error {
	a.mu.Lock()
	a.cfg.Calibration = mconfig.DefaultCalibration()
	cfgCopy := a.cfg
	a.mu.Unlock()
	return mconfig.Save(cfgCopy)
}
