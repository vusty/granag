package mic

import (
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

var iidIAudioEndpointVolume = ole.NewGUID("{5CDF2C82-841E-4546-9722-0CF74078229A}")

type endpointVolumeVtbl struct {
	ole.IUnknownVtbl
	RegisterControlChangeNotify   uintptr
	UnregisterControlChangeNotify uintptr
	GetChannelCount               uintptr
	SetMasterVolumeLevel          uintptr
	SetMasterVolumeLevelScalar    uintptr
	GetMasterVolumeLevel          uintptr
	GetMasterVolumeLevelScalar    uintptr
	SetChannelVolumeLevel         uintptr
	SetChannelVolumeLevelScalar   uintptr
	GetChannelVolumeLevel         uintptr
	GetChannelVolumeLevelScalar   uintptr
	SetMute                       uintptr
	GetMute                       uintptr
	GetVolumeStepInfo             uintptr
	VolumeStepUp                  uintptr
	VolumeStepDown                uintptr
	QueryHardwareSupport          uintptr
	GetVolumeRange                uintptr
}

// full is where the input volume belongs. Anything below it quietly makes the
// microphone inaudible to the other side while everything still looks
// connected, and Windows resets it to zero on its own across restarts.
const full = 0.999

// Volume returns the endpoint's input volume, 0.0 to 1.0. This is the Windows
// slider, not the gain knob on the microphone itself.
func (d *Device) Volume() (float32, error) {
	vol, err := d.activate(iidIAudioEndpointVolume)
	if err != nil {
		return 0, err
	}
	defer vol.Release()
	return masterScalar(vol)
}

// RaiseToMax puts the endpoint's input volume at maximum and reports whether it
// had to change anything.
//
// It gets there by stepping rather than by setting a value: the setters take a
// float by value, and on amd64 a float argument travels in an XMM register,
// which Go's syscall bridge cannot fill. Stepping needs no arguments at all.
func (d *Device) RaiseToMax() (bool, error) {
	vol, err := d.activate(iidIAudioEndpointVolume)
	if err != nil {
		return false, err
	}
	defer vol.Release()

	current, err := masterScalar(vol)
	if err != nil {
		return false, err
	}
	if current >= full {
		return false, nil
	}

	var step, steps uint32
	r, _, _ := syscall.SyscallN(
		vtbl[endpointVolumeVtbl](vol).GetVolumeStepInfo,
		uintptr(unsafe.Pointer(vol)),
		uintptr(unsafe.Pointer(&step)),
		uintptr(unsafe.Pointer(&steps)),
	)
	if err := hr(r, "GetVolumeStepInfo"); err != nil {
		return false, err
	}

	// steps+1 is the most it can take from anywhere on the scale; the guard is
	// there so a device reporting nonsense cannot spin this forever.
	for i := uint32(0); i <= steps+1; i++ {
		level, err := masterScalar(vol)
		if err != nil {
			return true, err
		}
		if level >= full {
			return true, nil
		}
		r, _, _ := syscall.SyscallN(
			vtbl[endpointVolumeVtbl](vol).VolumeStepUp,
			uintptr(unsafe.Pointer(vol)),
			0,
		)
		if err := hr(r, "VolumeStepUp"); err != nil {
			return true, err
		}
	}
	return true, nil
}

func masterScalar(vol *comObject) (float32, error) {
	var level float32
	r, _, _ := syscall.SyscallN(
		vtbl[endpointVolumeVtbl](vol).GetMasterVolumeLevelScalar,
		uintptr(unsafe.Pointer(vol)),
		uintptr(unsafe.Pointer(&level)),
	)
	if err := hr(r, "GetMasterVolumeLevelScalar"); err != nil {
		return 0, err
	}
	return level, nil
}
