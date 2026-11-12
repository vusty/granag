package tray

import (
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The tray menu is a plain Win32 popup menu, and Windows draws those light
// unless the process asks otherwise. The way to ask is undocumented:
// uxtheme.dll exports it by ordinal only, stable since Windows 10 1903, and it
// is what other Win32 programs with dark menus call.
const (
	ordSetPreferredAppMode = 135
	ordFlushMenuThemes     = 136

	appModeForceDark  = 2
	appModeForceLight = 3

	// Before 1903 ordinal 135 is a different function.
	minBuild = 18362
)

// The menu follows the Windows mode, the one the taskbar and Start use, not
// the app mode: it opens from the taskbar and should look like it.
const personalize = `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`

type menuTheme struct{ setMode, flush uintptr }

// followMenuTheme themes the menu now and again whenever the Windows mode
// changes. On a Windows too old for it the menu simply stays light.
func followMenuTheme() {
	if windows.RtlGetVersion().BuildNumber < minBuild {
		return
	}
	dll, err := windows.LoadLibraryEx("uxtheme.dll", 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return
	}
	var m menuTheme
	if m.setMode, err = windows.GetProcAddressByOrdinal(dll, ordSetPreferredAppMode); err != nil {
		return
	}
	if m.flush, err = windows.GetProcAddressByOrdinal(dll, ordFlushMenuThemes); err != nil {
		return
	}
	m.apply()

	go func() {
		k, err := registry.OpenKey(registry.CURRENT_USER, personalize, registry.NOTIFY|registry.QUERY_VALUE)
		if err != nil {
			return
		}
		defer k.Close()
		// Blocks until a value under the key changes, then once more.
		for windows.RegNotifyChangeKeyValue(windows.Handle(k), false, windows.REG_NOTIFY_CHANGE_LAST_SET, 0, false) == nil {
			m.apply()
		}
	}()
}

func (m menuTheme) apply() {
	mode := uintptr(appModeForceLight)
	if systemDark() {
		mode = appModeForceDark
	}
	syscall.SyscallN(m.setMode, mode)
	syscall.SyscallN(m.flush)
}

func systemDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, personalize, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
	return err == nil && v == 0
}
