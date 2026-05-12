// Package audio wraps go-wca (Windows Core Audio bindings) with a small
// human-friendly API: list sessions, set volume by master/exe-name/system.
package audio

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

// Special target keywords understood by SetVolume.
const (
	TargetMaster     = "master"
	TargetSystem     = "system"
	TargetMic        = "mic"
	TargetForeground = "game"
)

type Session struct {
	PID      uint32  `json:"pid"`
	Name     string  `json:"name"`   // basename of the process exe, e.g. "chrome.exe"
	Volume   float32 `json:"volume"` // 0.0..1.0
	IsSystem bool    `json:"isSystem"`
}

// InitCOM must be called once per goroutine that uses this package.
// Pair with ole.CoUninitialize() via defer at goroutine exit.
func InitCOM() error {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// S_FALSE (already initialised) returns as OleError — accept it.
		if oe, ok := err.(*ole.OleError); ok && oe.Code() == 1 {
			return nil
		}
		return err
	}
	return nil
}

func Uninit() { ole.CoUninitialize() }

// ListSessions enumerates every active audio session on the default
// playback endpoint and returns one Session per record.
func ListSessions() ([]Session, error) {
	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &mmde,
	); err != nil {
		return nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): %w", err)
	}
	defer mmde.Release()

	var device *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EMultimedia, &device); err != nil {
		return nil, fmt.Errorf("GetDefaultAudioEndpoint: %w", err)
	}
	defer device.Release()

	var asm *wca.IAudioSessionManager2
	if err := device.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &asm); err != nil {
		return nil, fmt.Errorf("Activate(IAudioSessionManager2): %w", err)
	}
	defer asm.Release()

	var enum *wca.IAudioSessionEnumerator
	if err := asm.GetSessionEnumerator(&enum); err != nil {
		return nil, err
	}
	defer enum.Release()

	var count int
	if err := enum.GetCount(&count); err != nil {
		return nil, err
	}

	sessions := make([]Session, 0, count)
	for i := 0; i < count; i++ {
		s, ok := readSession(enum, i)
		if !ok {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func readSession(enum *wca.IAudioSessionEnumerator, i int) (Session, bool) {
	var ctrl *wca.IAudioSessionControl
	if err := enum.GetSession(i, &ctrl); err != nil {
		return Session{}, false
	}
	defer ctrl.Release()

	dispatch, err := ctrl.QueryInterface(wca.IID_IAudioSessionControl2)
	if err != nil {
		return Session{}, false
	}
	ctrl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(dispatch))
	defer ctrl2.Release()

	var pid uint32
	_ = ctrl2.GetProcessId(&pid)

	isSystem := ctrl2.IsSystemSoundsSession() == nil // returns S_OK if yes

	dispatch2, err := ctrl2.QueryInterface(wca.IID_ISimpleAudioVolume)
	if err != nil {
		return Session{}, false
	}
	vol := (*wca.ISimpleAudioVolume)(unsafe.Pointer(dispatch2))
	defer vol.Release()

	var level float32
	_ = vol.GetMasterVolume(&level)

	return Session{
		PID:      pid,
		Name:     processName(pid),
		Volume:   level,
		IsSystem: isSystem,
	}, true
}

// processName returns the basename of the executable owning the PID, or
// "" if access is denied (which is common for system pids).
func processName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var buf [windows.MAX_LONG_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return strings.ToLower(filepath.Base(syscall.UTF16ToString(buf[:size])))
}

// SetMasterVolume sets the default endpoint master level (0..1).
func SetMasterVolume(level float32) error {
	level = clamp01(level)

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &mmde,
	); err != nil {
		return err
	}
	defer mmde.Release()

	var device *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EMultimedia, &device); err != nil {
		return err
	}
	defer device.Release()

	var aev *wca.IAudioEndpointVolume
	if err := device.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return err
	}
	defer aev.Release()

	// Auto-unmute when the user moves the slider above zero. Without
	// this, raising the slider would change the level but Windows would
	// still play nothing because the endpoint stayed muted.
	if level > 0 {
		_ = aev.SetMute(false, nil)
	}

	return aev.SetMasterVolumeLevelScalar(level, nil)
}

// SetVolumeByExe finds every session whose process basename matches
// exeName (case-insensitive, e.g. "chrome.exe") and sets its volume.
// Returns the number of sessions matched.
func SetVolumeByExe(exeName string, level float32) (int, error) {
	exeName = strings.ToLower(exeName)
	level = clamp01(level)

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &mmde,
	); err != nil {
		return 0, err
	}
	defer mmde.Release()

	var device *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EMultimedia, &device); err != nil {
		return 0, err
	}
	defer device.Release()

	var asm *wca.IAudioSessionManager2
	if err := device.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &asm); err != nil {
		return 0, err
	}
	defer asm.Release()

	var enum *wca.IAudioSessionEnumerator
	if err := asm.GetSessionEnumerator(&enum); err != nil {
		return 0, err
	}
	defer enum.Release()

	var count int
	if err := enum.GetCount(&count); err != nil {
		return 0, err
	}

	matched := 0
	for i := 0; i < count; i++ {
		var ctrl *wca.IAudioSessionControl
		if enum.GetSession(i, &ctrl) != nil {
			continue
		}

		ctrl2disp, err := ctrl.QueryInterface(wca.IID_IAudioSessionControl2)
		if err != nil {
			ctrl.Release()
			continue
		}
		ctrl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(ctrl2disp))

		var pid uint32
		_ = ctrl2.GetProcessId(&pid)
		isSystem := ctrl2.IsSystemSoundsSession() == nil

		matches := false
		if exeName == TargetSystem {
			matches = isSystem
		} else if pid != 0 {
			matches = processName(pid) == exeName
		}

		if matches {
			if voldisp, err := ctrl2.QueryInterface(wca.IID_ISimpleAudioVolume); err == nil {
				vol := (*wca.ISimpleAudioVolume)(unsafe.Pointer(voldisp))
				// Snap out of mute when the user raises the slider above
				// zero — feels much more natural than "you raised it but
				// nothing plays because it's still muted".
				if level > 0 {
					_ = vol.SetMute(false, nil)
				}
				if err := vol.SetMasterVolume(level, nil); err == nil {
					matched++
				}
				vol.Release()
			}
		}
		ctrl2.Release()
		ctrl.Release()
	}
	return matched, nil
}

func clamp01(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ReadPeakLevels enumerates the default endpoint once and returns the
// current peak meter value (0..1) for each slider, picking the loudest
// target attached to that slider. Cheap enough to poll at 25–40 Hz.
//
// Targets resolve like this (mirrors applySlider in app.go):
//   - "master"  → endpoint peak
//   - "system"  → system-sounds session peak
//   - "game"    → currentForeground (caller passes the resolved exe)
//   - "*.exe"   → max peak across sessions with that basename
func ReadPeakLevels(mapping map[int][]string, currentForeground string, n int) ([]float32, error) {
	out := make([]float32, n)

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &mmde,
	); err != nil {
		return out, err
	}
	defer mmde.Release()

	var device *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EMultimedia, &device); err != nil {
		return out, err
	}
	defer device.Release()

	// Master peak from the endpoint itself.
	var masterPeak float32
	var amiMaster *wca.IAudioMeterInformation
	if err := device.Activate(wca.IID_IAudioMeterInformation, wca.CLSCTX_ALL, nil, &amiMaster); err == nil {
		_ = amiMaster.GetPeakValue(&masterPeak)
		amiMaster.Release()
	}

	// Per-session peaks, keyed by exe basename. Multiple sessions with
	// the same exe (e.g. several Chrome tabs) collapse to max().
	peakByExe := map[string]float32{}
	var systemPeak float32

	var asm *wca.IAudioSessionManager2
	if err := device.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &asm); err == nil {
		defer asm.Release()
		var enum *wca.IAudioSessionEnumerator
		if err := asm.GetSessionEnumerator(&enum); err == nil {
			defer enum.Release()
			var count int
			_ = enum.GetCount(&count)
			for i := 0; i < count; i++ {
				var ctrl *wca.IAudioSessionControl
				if enum.GetSession(i, &ctrl) != nil {
					continue
				}
				ctrl2disp, err := ctrl.QueryInterface(wca.IID_IAudioSessionControl2)
				if err != nil {
					ctrl.Release()
					continue
				}
				ctrl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(ctrl2disp))

				var pid uint32
				_ = ctrl2.GetProcessId(&pid)
				isSystem := ctrl2.IsSystemSoundsSession() == nil

				amidisp, err := ctrl2.QueryInterface(wca.IID_IAudioMeterInformation)
				if err == nil {
					ami := (*wca.IAudioMeterInformation)(unsafe.Pointer(amidisp))
					var peak float32
					if ami.GetPeakValue(&peak) == nil {
						if isSystem {
							if peak > systemPeak {
								systemPeak = peak
							}
						} else if pid != 0 {
							name := processName(pid)
							if name != "" && peak > peakByExe[name] {
								peakByExe[name] = peak
							}
						}
					}
					ami.Release()
				}
				ctrl2.Release()
				ctrl.Release()
			}
		}
	}

	// Resolve each slider's target list to a level.
	for i := 0; i < n; i++ {
		targets := mapping[i]
		var best float32
		for _, t := range targets {
			var v float32
			switch t {
			case TargetMaster:
				v = masterPeak
			case TargetSystem:
				v = systemPeak
			case TargetForeground:
				if currentForeground != "" {
					v = peakByExe[currentForeground]
				}
			default:
				v = peakByExe[strings.ToLower(t)]
			}
			if v > best {
				best = v
			}
		}
		out[i] = clamp01(best)
	}
	return out, nil
}
