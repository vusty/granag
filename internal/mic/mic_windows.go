// Package mic reads the peak level of a Windows capture endpoint without
// opening a capture stream of its own.
//
// IAudioMeterInformation reports a level only while the device's audio engine
// is running, which normally means some application is capturing. On this
// machine NVIDIA Broadcast holds the physical microphone open all the time, so
// the meter answers without us capturing anything — no stream, and no
// "an app is using your microphone" indicator in the tray.
package mic

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

const (
	eCapture          = 1
	deviceStateActive = 0x1
	stgmRead          = 0
	clsCtxAll         = 23
	vtLPWSTR          = 31
	pidDeviceFriendly = 14
)

var (
	clsidMMDeviceEnumerator = ole.NewGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidIMMDeviceEnumerator  = ole.NewGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidIAudioMeter          = ole.NewGUID("{C02216F6-8C67-4B5B-9D00-D008E73E0064}")

	// PKEY_Device_FriendlyName — the endpoint name as the sound settings show
	// it, e.g. "Microphone (HyperX QuadCast S)".
	keyFriendlyName = propertyKey{
		fmtid: *ole.NewGUID("{A45C254E-DF1C-4EFD-8020-67D146A850E0}"),
		pid:   pidDeviceFriendly,
	}
)

type propertyKey struct {
	fmtid ole.GUID
	pid   uint32
}

// propVariant is PROPVARIANT trimmed to what we read: a string pointer. The
// trailing padding keeps the struct at least as large as the real union, so
// the callee never writes past it.
type propVariant struct {
	vt  uint16
	_   [3]uint16
	val uintptr
	_   uintptr
}

type enumeratorVtbl struct {
	ole.IUnknownVtbl
	EnumAudioEndpoints                     uintptr
	GetDefaultAudioEndpoint                uintptr
	GetDevice                              uintptr
	RegisterEndpointNotificationCallback   uintptr
	UnregisterEndpointNotificationCallback uintptr
}

type collectionVtbl struct {
	ole.IUnknownVtbl
	GetCount uintptr
	Item     uintptr
}

type deviceVtbl struct {
	ole.IUnknownVtbl
	Activate          uintptr
	OpenPropertyStore uintptr
	GetId             uintptr
	GetState          uintptr
}

type propertyStoreVtbl struct {
	ole.IUnknownVtbl
	GetCount uintptr
	GetAt    uintptr
	GetValue uintptr
	SetValue uintptr
	Commit   uintptr
}

type meterVtbl struct {
	ole.IUnknownVtbl
	GetPeakValue          uintptr
	GetMeteringChannelCnt uintptr
	GetChannelsPeakValues uintptr
	QueryHardwareSupport  uintptr
}

type comObject struct {
	ole.IUnknown
}

func vtbl[T any](o *comObject) *T {
	return (*T)(unsafe.Pointer(o.RawVTable))
}

func hr(r uintptr, what string) error {
	if int32(r) < 0 {
		return fmt.Errorf("%s: hresult 0x%08x", what, uint32(r))
	}
	return nil
}

// Device is one active capture endpoint with its meter already activated.
type Device struct {
	Name  string
	dev   *comObject
	meter *comObject
}

// Peak returns the highest sample seen since the previous call, 0.0 to 1.0.
//
// A physically muted microphone is not a zero: the HyperX QuadCast S keeps
// streaming and its tap-to-mute never reaches Windows, so muted silence still
// reads a small constant floor. Distinguish speech by threshold, not by
// comparing against zero.
func (d *Device) Peak() (float32, error) {
	var peak float32
	r, _, _ := syscall.SyscallN(
		vtbl[meterVtbl](d.meter).GetPeakValue,
		uintptr(unsafe.Pointer(d.meter)),
		uintptr(unsafe.Pointer(&peak)),
	)
	if err := hr(r, "GetPeakValue"); err != nil {
		return 0, err
	}
	return peak, nil
}

// Release drops both COM references. Safe to call twice.
func (d *Device) Release() {
	if d.meter != nil {
		d.meter.Release()
		d.meter = nil
	}
	if d.dev != nil {
		d.dev.Release()
		d.dev = nil
	}
}

// Devices returns every active capture endpoint, meters included. The caller
// releases each one. COM must already be initialised on this thread.
func Devices() ([]*Device, error) {
	unk, err := ole.CreateInstance(clsidMMDeviceEnumerator, iidIMMDeviceEnumerator)
	if err != nil {
		return nil, fmt.Errorf("MMDeviceEnumerator: %w", err)
	}
	enum := (*comObject)(unsafe.Pointer(unk))
	defer enum.Release()

	var coll *comObject
	r, _, _ := syscall.SyscallN(
		vtbl[enumeratorVtbl](enum).EnumAudioEndpoints,
		uintptr(unsafe.Pointer(enum)),
		eCapture, deviceStateActive,
		uintptr(unsafe.Pointer(&coll)),
	)
	if err := hr(r, "EnumAudioEndpoints"); err != nil {
		return nil, err
	}
	defer coll.Release()

	var count uint32
	r, _, _ = syscall.SyscallN(
		vtbl[collectionVtbl](coll).GetCount,
		uintptr(unsafe.Pointer(coll)),
		uintptr(unsafe.Pointer(&count)),
	)
	if err := hr(r, "GetCount"); err != nil {
		return nil, err
	}

	var out []*Device
	for i := uint32(0); i < count; i++ {
		d, err := itemAt(coll, i)
		if err != nil {
			for _, done := range out {
				done.Release()
			}
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func itemAt(coll *comObject, i uint32) (*Device, error) {
	var dev *comObject
	r, _, _ := syscall.SyscallN(
		vtbl[collectionVtbl](coll).Item,
		uintptr(unsafe.Pointer(coll)),
		uintptr(i),
		uintptr(unsafe.Pointer(&dev)),
	)
	if err := hr(r, "Item"); err != nil {
		return nil, err
	}

	name, err := friendlyName(dev)
	if err != nil {
		dev.Release()
		return nil, err
	}

	var meter *comObject
	r, _, _ = syscall.SyscallN(
		vtbl[deviceVtbl](dev).Activate,
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(iidIAudioMeter)),
		clsCtxAll, 0,
		uintptr(unsafe.Pointer(&meter)),
	)
	if err := hr(r, "Activate(IAudioMeterInformation)"); err != nil {
		dev.Release()
		return nil, err
	}

	return &Device{Name: name, dev: dev, meter: meter}, nil
}

func friendlyName(dev *comObject) (string, error) {
	var store *comObject
	r, _, _ := syscall.SyscallN(
		vtbl[deviceVtbl](dev).OpenPropertyStore,
		uintptr(unsafe.Pointer(dev)),
		stgmRead,
		uintptr(unsafe.Pointer(&store)),
	)
	if err := hr(r, "OpenPropertyStore"); err != nil {
		return "", err
	}
	defer store.Release()

	var pv propVariant
	key := keyFriendlyName
	r, _, _ = syscall.SyscallN(
		vtbl[propertyStoreVtbl](store).GetValue,
		uintptr(unsafe.Pointer(store)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(&pv)),
	)
	if err := hr(r, "GetValue(FriendlyName)"); err != nil {
		return "", err
	}
	if pv.vt != vtLPWSTR || pv.val == 0 {
		return "", fmt.Errorf("FriendlyName: unexpected variant type %d", pv.vt)
	}
	name := lpwstr(pv.val)
	defer windows.CoTaskMemFree(unsafe.Pointer(name))

	return windows.UTF16PtrToString(name), nil
}

// lpwstr reinterprets the PROPVARIANT union slot as the string it holds when
// vt is VT_LPWSTR.
//
// The slot is typed uintptr rather than as a pointer on purpose: the callee
// fills it with whatever the property's type calls for, and a garbage collector
// scanning a slot that may hold an integer is a crash waiting for the wrong
// property. That makes this conversion the one place go vet's unsafeptr check
// complains about, and there it is a false positive - the memory belongs to
// CoTaskMemAlloc, not to Go, and the caller frees it.
func lpwstr(v uintptr) *uint16 {
	return (*uint16)(unsafe.Pointer(v))
}

// Find returns the single active capture endpoint whose name contains match,
// case-insensitively. Every other device is released before returning.
func Find(match string) (*Device, error) {
	devs, err := Devices()
	if err != nil {
		return nil, err
	}

	var found *Device
	var names []string
	for _, d := range devs {
		names = append(names, d.Name)
		if found == nil && strings.Contains(strings.ToLower(d.Name), strings.ToLower(match)) {
			found = d
			continue
		}
		d.Release()
	}
	if found == nil {
		return nil, fmt.Errorf("no active capture device matching %q; have: %s",
			match, strings.Join(names, ", "))
	}
	return found, nil
}
