package tray

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/alert.ico
var iconAlert []byte

// beaconUID is the alert icon's id within the process. Windows files each
// pair of executable and icon id as its own entry in the taskbar settings, so
// this one can sit on the taskbar while the menu icon - id 100, hard-coded by
// systray - stays in the overflow.
const beaconUID = 1

const beaconTip = "granag: микрофон включён, Granola не пишет"

const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmApply   = 0x8000 + 1 // WM_APP + 1: bring the icon in line with want

	nimAdd    = 0
	nimDelete = 2
	nifIcon   = 0x2
	nifTip    = 0x4

	smCxSmIcon = 49
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	pCreateWindowEx           = user32.NewProc("CreateWindowExW")
	pDefWindowProc            = user32.NewProc("DefWindowProcW")
	pDestroyWindow            = user32.NewProc("DestroyWindow")
	pDispatchMessage          = user32.NewProc("DispatchMessageW")
	pGetMessage               = user32.NewProc("GetMessageW")
	pGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	pPostMessage              = user32.NewProc("PostMessageW")
	pPostQuitMessage          = user32.NewProc("PostQuitMessage")
	pRegisterClassEx          = user32.NewProc("RegisterClassExW")
	pRegisterWindowMessage    = user32.NewProc("RegisterWindowMessageW")
	pShellNotifyIcon          = shell32.NewProc("Shell_NotifyIconW")
	pGetModuleHandle          = kernel32.NewProc("GetModuleHandleW")
)

// Beacon is the second tray icon: a red microphone that is there only while
// the microphone is live and nobody is recording it, and gone otherwise.
//
// The menu icon cannot do this job. Windows 11 parks it in the overflow behind
// the arrow, where a colour change goes unseen, and systray has no way to take
// an icon away and bring it back. So the beacon is its own icon on its own
// hidden window, with its own message loop, and it asks to be shown on the
// taskbar itself.
type Beacon struct {
	logf func(string, ...any)
	done chan struct{}

	mu   sync.Mutex
	want bool
	hwnd uintptr

	// Owned by the window thread.
	shown      bool
	icon       uintptr
	taskbarMsg uintptr
	lastErr    string
}

// active is the beacon the window procedure serves. There is one per process,
// and a Go callback has no other way to reach it.
var active *Beacon

// NewBeacon creates the hidden window that owns the icon, without showing
// anything yet.
func NewBeacon(logf func(string, ...any)) (*Beacon, error) {
	b := &Beacon{logf: logf, done: make(chan struct{})}
	ready := make(chan error, 1)
	go b.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return b, nil
}

// Set says whether the icon should be on the taskbar. Safe to call from any
// goroutine and as often as liked: it is called on every poll, which is also
// what retries an add that failed because Explorer was not up yet at logon.
func (b *Beacon) Set(on bool) {
	b.mu.Lock()
	b.want = on
	hwnd := b.hwnd
	b.mu.Unlock()
	pPostMessage.Call(hwnd, wmApply, 0, 0)
}

// Close removes the icon and the window. An icon left behind by a process
// that exits without it stays on the taskbar until the mouse passes over it.
func (b *Beacon) Close() {
	b.mu.Lock()
	hwnd := b.hwnd
	b.mu.Unlock()
	pPostMessage.Call(hwnd, wmClose, 0, 0)
	select {
	case <-b.done:
	case <-time.After(time.Second):
	}
}

// loop creates the window and serves its messages. A window belongs to the
// thread that created it, so the goroutine keeps its thread to the end.
func (b *Beacon) loop(ready chan<- error) {
	runtime.LockOSThread()
	defer close(b.done)

	hwnd, err := b.create()
	if err != nil {
		ready <- err
		return
	}
	b.mu.Lock()
	b.hwnd = hwnd
	b.mu.Unlock()
	ready <- nil

	var m struct {
		hwnd    uintptr
		message uint32
		wParam  uintptr
		lParam  uintptr
		time    uint32
		pt      struct{ x, y int32 }
		private uint32
	}
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (b *Beacon) create() (uintptr, error) {
	size, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
	icon, err := iconFromICO(iconAlert, int(size))
	if err != nil {
		return 0, err
	}
	b.icon = icon

	name, _ := windows.UTF16PtrFromString("TaskbarCreated")
	b.taskbarMsg, _, _ = pRegisterWindowMessage.Call(uintptr(unsafe.Pointer(name)))

	inst, _, _ := pGetModuleHandle.Call(0)
	class, _ := windows.UTF16PtrFromString("granagBeacon")
	wc := struct {
		size, style                        uint32
		wndProc                            uintptr
		clsExtra, wndExtra                 int32
		instance, icon, cursor, background uintptr
		menuName, className                *uint16
		iconSm                             uintptr
	}{
		wndProc:   windows.NewCallback(beaconProc),
		instance:  inst,
		className: class,
	}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return 0, fmt.Errorf("RegisterClassEx: %w", err)
	}

	// Set before the window exists: creating it already calls the procedure.
	active = b

	// A hidden top-level window rather than a message-only one, because only
	// top-level windows hear TaskbarCreated, the broadcast that says Explorer
	// restarted and every icon has to be added again.
	hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(class)), 0, 0,
		0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		return 0, fmt.Errorf("CreateWindowEx: %w", err)
	}
	return hwnd, nil
}

func beaconProc(hwnd, message, wParam, lParam uintptr) uintptr {
	b := active
	switch {
	case message == wmApply:
		b.apply(hwnd)
		return 0
	case b.taskbarMsg != 0 && message == b.taskbarMsg:
		b.shown = false
		b.apply(hwnd)
		return 0
	case message == wmClose:
		if b.shown {
			b.notify(hwnd, nimDelete)
			b.shown = false
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case message == wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

// apply adds or removes the icon to match want.
func (b *Beacon) apply(hwnd uintptr) {
	b.mu.Lock()
	want := b.want
	b.mu.Unlock()
	if want == b.shown {
		return
	}

	if !want {
		b.notify(hwnd, nimDelete)
		b.shown = false
		return
	}
	if err := b.notify(hwnd, nimAdd); err != nil {
		if msg := err.Error(); msg != b.lastErr {
			b.logf("alert icon: could not add it, retrying on every poll: %v", err)
			b.lastErr = msg
		}
		return
	}
	b.shown, b.lastErr = true, ""
	go b.promote()
}

func (b *Beacon) notify(hwnd, op uintptr) error {
	// NOTIFYICONDATAW, laid out as on amd64.
	var nid struct {
		size                   uint32
		wnd                    uintptr
		id, flags, callbackMsg uint32
		icon                   uintptr
		tip                    [128]uint16
		state, stateMask       uint32
		info                   [256]uint16
		timeoutOrVersion       uint32
		infoTitle              [64]uint16
		infoFlags              uint32
		guidItem               windows.GUID
		balloonIcon            uintptr
	}
	nid.size = uint32(unsafe.Sizeof(nid))
	nid.wnd = hwnd
	nid.id = beaconUID
	if op == nimAdd {
		nid.flags = nifIcon | nifTip
		nid.icon = b.icon
		tip, _ := windows.UTF16FromString(beaconTip)
		copy(nid.tip[:len(nid.tip)-1], tip)
	}
	if r, _, err := pShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&nid))); r == 0 {
		return err
	}
	return nil
}

// promote moves the icon out of the overflow and onto the taskbar.
//
// Windows 11 sends every new tray icon to the overflow, and no API asks for
// anything else. The per-icon switch in Settings lives in the registry, one
// subkey per executable and icon id under NotifyIconSettings, and the switch
// writes IsPromoted = 1 there. Explorer creates the subkey on the first add, a
// moment after the call returns, so this looks a few times before giving up.
//
// It runs after every add, so turning the icon off in Settings does not stick:
// an alert tucked away behind the arrow is no alert.
func (b *Beacon) promote() {
	self, err := os.Executable()
	if err != nil {
		b.logf("alert icon: %v", err)
		return
	}
	exe := filepath.Base(self)
	for range 10 {
		found, changed, err := promoteEntry(exe)
		if err != nil {
			b.logf("alert icon: could not promote it to the taskbar: %v", err)
			return
		}
		if found {
			if changed {
				b.logf("alert icon promoted to the taskbar")
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	b.logf("alert icon: Explorer has no settings entry for it, left in the overflow")
}

// promoteEntry sets IsPromoted on every NotifyIconSettings entry for the
// beacon. It matches on the file name rather than the full path because
// Explorer writes paths under known folders as a folder GUID and the rest, and
// a build run from elsewhere being promoted too is harmless.
func promoteEntry(exe string) (found, changed bool, err error) {
	root, err := registry.OpenKey(registry.CURRENT_USER,
		`Control Panel\NotifyIconSettings`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return false, false, err
	}
	defer root.Close()

	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return false, false, err
	}
	for _, name := range names {
		k, err := registry.OpenKey(root, name, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			continue
		}
		uid, _, uidErr := k.GetIntegerValue("UID")
		path, _, pathErr := k.GetStringValue("ExecutablePath")
		if uidErr == nil && pathErr == nil && uid == beaconUID &&
			strings.EqualFold(filepath.Base(path), exe) {
			found = true
			if v, _, err := k.GetIntegerValue("IsPromoted"); err != nil || v != 1 {
				if err := k.SetDWordValue("IsPromoted", 1); err == nil {
					changed = true
				}
			}
		}
		k.Close()
	}
	return found, changed, nil
}

// iconFromICO makes an icon handle out of the bytes of an .ico file, from the
// image closest to size pixels wide and no smaller, or the largest there is.
func iconFromICO(ico []byte, size int) (uintptr, error) {
	if len(ico) < 6 {
		return 0, fmt.Errorf("icon: %d bytes is too short", len(ico))
	}
	count := int(binary.LittleEndian.Uint16(ico[4:]))
	if len(ico) < 6+16*count {
		return 0, fmt.Errorf("icon: directory of %d entries does not fit", count)
	}

	best, bestW := -1, 0
	for i := range count {
		w := int(ico[6+16*i])
		if w == 0 {
			w = 256
		}
		better := best < 0 ||
			(w >= size && (bestW < size || w < bestW)) ||
			(w < size && bestW < size && w > bestW)
		if better {
			best, bestW = i, w
		}
	}
	if best < 0 {
		return 0, fmt.Errorf("icon: no images")
	}

	e := ico[6+16*best:]
	n := binary.LittleEndian.Uint32(e[8:])
	off := binary.LittleEndian.Uint32(e[12:])
	if uint64(off)+uint64(n) > uint64(len(ico)) || n == 0 {
		return 0, fmt.Errorf("icon: image %d runs past the end", best)
	}
	h, _, err := pCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&ico[off])),
		uintptr(n), 1, 0x00030000, uintptr(size), uintptr(size), 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateIconFromResourceEx: %w", err)
	}
	return h, nil
}
