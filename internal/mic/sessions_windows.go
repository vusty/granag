package mic

import (
	"fmt"
	"path/filepath"
	"slices"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// Cross-checked against go-wca v0.3.0, like the stream IIDs.
var (
	iidIAudioSessionManager2 = ole.NewGUID("{77AA99A0-1BD6-484F-8BC7-2C654C9A9B6F}")
	iidIAudioSessionControl2 = ole.NewGUID("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")
)

const audioSessionStateActive = 1

type sessionManager2Vtbl struct {
	ole.IUnknownVtbl
	GetAudioSessionControl        uintptr
	GetSimpleAudioVolume          uintptr
	GetSessionEnumerator          uintptr
	RegisterSessionNotification   uintptr
	UnregisterSessionNotification uintptr
	RegisterDuckNotification      uintptr
	UnregisterDuckNotification    uintptr
}

type sessionEnumeratorVtbl struct {
	ole.IUnknownVtbl
	GetCount   uintptr
	GetSession uintptr
}

type sessionControl2Vtbl struct {
	ole.IUnknownVtbl
	GetState                           uintptr
	GetDisplayName                     uintptr
	SetDisplayName                     uintptr
	GetIconPath                        uintptr
	SetIconPath                        uintptr
	GetGroupingParam                   uintptr
	SetGroupingParam                   uintptr
	RegisterAudioSessionNotification   uintptr
	UnregisterAudioSessionNotification uintptr
	GetSessionIdentifier               uintptr
	GetSessionInstanceIdentifier       uintptr
	GetProcessId                       uintptr
	IsSystemSoundsSession              uintptr
	SetDuckingPreference               uintptr
}

// Capture is one process capturing from a microphone right now.
type Capture struct {
	Exe    string // executable file name, "Granola.exe"
	PID    uint32
	Device string // the endpoint it captures from
}

// Capturing lists every process with a running capture stream on any active
// microphone, this process included.
//
// This is the audio engine's own view, the one the volume mixer is built on,
// so it is exact and current. It replaced the CapabilityAccessManager consent
// store: from the 26H2 update on (build 26300) Windows stopped writing that
// registry key, and its last entries froze in October 2026 while Granola kept
// recording, so every recording read as "Granola is not recording".
//
// COM must already be initialised on this thread.
func Capturing() ([]Capture, error) {
	devs, err := Devices()
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, d := range devs {
			d.Release()
		}
	}()

	var out []Capture
	for _, d := range devs {
		pids, err := d.activePIDs()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name, err)
		}
		for _, pid := range pids {
			out = append(out, Capture{Exe: exeName(pid), PID: pid, Device: d.Name})
		}
	}
	return out, nil
}

// CapturingExes is Capturing reduced to the distinct executable names, sorted,
// leaving out this process.
func CapturingExes() ([]string, error) {
	caps, err := Capturing()
	if err != nil {
		return nil, err
	}
	self := uint32(windows.GetCurrentProcessId())
	var names []string
	for _, c := range caps {
		if c.PID != self && !slices.Contains(names, c.Exe) {
			names = append(names, c.Exe)
		}
	}
	slices.Sort(names)
	return names, nil
}

// activePIDs returns the process of every session on this endpoint whose
// stream is running. An application that opened the microphone and stopped
// keeps an inactive session until it exits; that is not capturing.
func (d *Device) activePIDs() ([]uint32, error) {
	var mgr *comObject
	r, _, _ := syscall.SyscallN(
		vtbl[deviceVtbl](d.dev).Activate,
		uintptr(unsafe.Pointer(d.dev)),
		uintptr(unsafe.Pointer(iidIAudioSessionManager2)),
		clsCtxAll, 0,
		uintptr(unsafe.Pointer(&mgr)),
	)
	if err := hr(r, "Activate(IAudioSessionManager2)"); err != nil {
		return nil, err
	}
	defer mgr.Release()

	var enum *comObject
	r, _, _ = syscall.SyscallN(
		vtbl[sessionManager2Vtbl](mgr).GetSessionEnumerator,
		uintptr(unsafe.Pointer(mgr)),
		uintptr(unsafe.Pointer(&enum)),
	)
	if err := hr(r, "GetSessionEnumerator"); err != nil {
		return nil, err
	}
	defer enum.Release()

	var count int32
	r, _, _ = syscall.SyscallN(
		vtbl[sessionEnumeratorVtbl](enum).GetCount,
		uintptr(unsafe.Pointer(enum)),
		uintptr(unsafe.Pointer(&count)),
	)
	if err := hr(r, "GetCount(sessions)"); err != nil {
		return nil, err
	}

	var pids []uint32
	for i := int32(0); i < count; i++ {
		pid, active, err := sessionAt(enum, i)
		if err != nil {
			return nil, err
		}
		// PID 0 is the system sounds session.
		if active && pid != 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func sessionAt(enum *comObject, i int32) (pid uint32, active bool, err error) {
	var ctl *comObject
	r, _, _ := syscall.SyscallN(
		vtbl[sessionEnumeratorVtbl](enum).GetSession,
		uintptr(unsafe.Pointer(enum)),
		uintptr(i),
		uintptr(unsafe.Pointer(&ctl)),
	)
	if err := hr(r, "GetSession"); err != nil {
		return 0, false, err
	}
	defer ctl.Release()

	unk, err := ctl.QueryInterface(iidIAudioSessionControl2)
	if err != nil {
		return 0, false, fmt.Errorf("IAudioSessionControl2: %w", err)
	}
	ctl2 := (*comObject)(unsafe.Pointer(unk))
	defer ctl2.Release()

	var state int32
	r, _, _ = syscall.SyscallN(
		vtbl[sessionControl2Vtbl](ctl2).GetState,
		uintptr(unsafe.Pointer(ctl2)),
		uintptr(unsafe.Pointer(&state)),
	)
	if err := hr(r, "GetState"); err != nil {
		return 0, false, err
	}
	// GetProcessId fails with AUDCLNT_S_NO_SINGLE_PROCESS for a session shared
	// across processes; such a session has no one owner to name, so skip it.
	r, _, _ = syscall.SyscallN(
		vtbl[sessionControl2Vtbl](ctl2).GetProcessId,
		uintptr(unsafe.Pointer(ctl2)),
		uintptr(unsafe.Pointer(&pid)),
	)
	if r != 0 {
		return 0, false, nil
	}
	return pid, state == audioSessionStateActive, nil
}

// exeName is the executable file name of a process, or "pid N" when it cannot
// be opened, which happens for elevated processes.
func exeName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Sprintf("pid %d", pid)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return fmt.Sprintf("pid %d", pid)
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}
