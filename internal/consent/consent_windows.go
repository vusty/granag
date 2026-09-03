// Package consent reads Windows' own record of which applications are using
// the microphone.
//
// CapabilityAccessManager keeps one subkey per application under the
// microphone consent store, with a FILETIME for when capture started and when
// it stopped. While an application holds the microphone, LastUsedTimeStop is
// zero — this is the same bookkeeping that drives the microphone icon in the
// tray. It answers "is Granola recording, or merely running" without any
// Granola API.
package consent

import (
	"fmt"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	storeRoot    = `SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`
	nonPackaged  = "NonPackaged"
	valueStarted = "LastUsedTimeStart"
	valueStopped = "LastUsedTimeStop"
)

// Holder is one application's most recent microphone session.
type Holder struct {
	// Exe is the executable's file name for a desktop application
	// ("Granola.exe"), or the package family name for a Store application.
	Exe string
	// Path is the full executable path, or the package family name.
	Path    string
	Started time.Time
	// Stopped is the zero time while the application still holds the device.
	Stopped time.Time
}

// Holding reports whether the application has the microphone open right now.
func (h Holder) Holding() bool { return h.Stopped.IsZero() }

// Holders returns the most recent microphone session of every application
// Windows has a record of, packaged and desktop alike.
func Holders() ([]Holder, error) {
	var out []Holder

	desktop, err := read(storeRoot+`\`+nonPackaged, true)
	if err != nil {
		return nil, err
	}
	out = append(out, desktop...)

	// Store applications sit directly under the consent store, keyed by
	// package family name; NonPackaged is the one subkey that is not an app.
	packaged, err := read(storeRoot, false)
	if err != nil {
		return nil, err
	}
	out = append(out, packaged...)

	return out, nil
}

// Active returns only the applications holding the microphone right now.
func Active() ([]Holder, error) {
	all, err := Holders()
	if err != nil {
		return nil, err
	}
	var out []Holder
	for _, h := range all {
		if h.Holding() {
			out = append(out, h)
		}
	}
	return out, nil
}

// IsHolding reports whether an application whose executable name matches exe,
// case-insensitively, has the microphone open right now.
func IsHolding(exe string) (bool, error) {
	active, err := Active()
	if err != nil {
		return false, err
	}
	for _, h := range active {
		if strings.EqualFold(h.Exe, exe) {
			return true, nil
		}
	}
	return false, nil
}

func read(key string, decodePath bool) ([]Holder, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	defer k.Close()

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", key, err)
	}

	var out []Holder
	for _, name := range names {
		if !decodePath && name == nonPackaged {
			continue
		}
		h, ok, err := holder(k, name, decodePath)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, h)
		}
	}
	return out, nil
}

func holder(parent registry.Key, name string, decodePath bool) (Holder, bool, error) {
	k, err := registry.OpenKey(parent, name, registry.QUERY_VALUE)
	if err != nil {
		// An application can be removed between enumerating and opening.
		return Holder{}, false, nil
	}
	defer k.Close()

	started, _, err := k.GetIntegerValue(valueStarted)
	if err != nil {
		// Consent recorded, never used: no session to report.
		return Holder{}, false, nil
	}
	stopped, _, err := k.GetIntegerValue(valueStopped)
	if err != nil {
		return Holder{}, false, fmt.Errorf("%s\\%s: %w", name, valueStopped, err)
	}

	h := Holder{
		Path:    name,
		Exe:     name,
		Started: fromFiletime(started),
		Stopped: fromFiletime(stopped),
	}
	if decodePath {
		// Windows stores the path with backslashes replaced by '#'.
		h.Path = strings.ReplaceAll(name, "#", `\`)
		h.Exe = path.Base(strings.ReplaceAll(h.Path, `\`, "/"))
	}
	return h, true, nil
}

// fromFiletime converts a FILETIME to a time, mapping the zero FILETIME that
// marks an open session to the zero time rather than to the year 1601.
func fromFiletime(v uint64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	const (
		ticksPerSecond = 10_000_000
		// Seconds between 1601-01-01 and 1970-01-01.
		epochDelta = 11_644_473_600
	)
	sec := int64(v/ticksPerSecond) - epochDelta
	nsec := int64(v%ticksPerSecond) * 100
	return time.Unix(sec, nsec)
}
