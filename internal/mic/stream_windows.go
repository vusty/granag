package mic

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

const (
	shareModeShared = 0

	// DefaultBuffer is how much audio the shared-mode stream is allowed to
	// queue. Nothing reads it for its contents, so this only bounds how long
	// Drain may fall behind.
	DefaultBuffer = time.Second
)

// Cross-checked against go-wca's table rather than typed from memory: a wrong
// IID costs an hour, because everything up to it succeeds and the only symptom
// is E_NOINTERFACE from a call whose vtable slot measures out correct.
var (
	iidIAudioClient        = ole.NewGUID("{1CB9AD4C-DBFA-4C32-B178-C2F568A703B2}")
	iidIAudioCaptureClient = ole.NewGUID("{C8ADBD64-E71E-48A0-A4DE-185C395CD317}")
)

type audioClientVtbl struct {
	ole.IUnknownVtbl
	Initialize        uintptr
	GetBufferSize     uintptr
	GetStreamLatency  uintptr
	GetCurrentPadding uintptr
	IsFormatSupported uintptr
	GetMixFormat      uintptr
	GetDevicePeriod   uintptr
	Start             uintptr
	Stop              uintptr
	Reset             uintptr
	SetEventHandle    uintptr
	GetService        uintptr
}

type captureClientVtbl struct {
	ole.IUnknownVtbl
	GetBuffer         uintptr
	ReleaseBuffer     uintptr
	GetNextPacketSize uintptr
}

// Stream is a shared-mode capture stream held open purely to keep the device's
// audio engine running.
//
// The meter reports nothing while no application captures - a muted microphone
// and an unmuted one being talked into read the same flat value. Holding a
// stream of our own makes us that application, which is the only way to see
// the microphone's state at a moment when nobody is on a call. The cost is
// visible and permanent: Windows lights the microphone indicator and lists this
// tool among the applications listening.
type Stream struct {
	client  *comObject
	capture *comObject
}

// Hold opens and starts a shared-mode capture stream on the device. Shared mode
// takes nothing away from anyone: calls, Granola and this stream coexist.
func (d *Device) Hold(buffer time.Duration) (*Stream, error) {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}

	client, err := d.activate(iidIAudioClient)
	if err != nil {
		return nil, err
	}

	// The mix format is whatever the engine already runs at; asking for our
	// own would risk a format the device refuses.
	var format *byte
	r, _, _ := syscall.SyscallN(
		vtbl[audioClientVtbl](client).GetMixFormat,
		uintptr(unsafe.Pointer(client)),
		uintptr(unsafe.Pointer(&format)),
	)
	if err := hr(r, "GetMixFormat"); err != nil {
		client.Release()
		return nil, err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(format))

	// REFERENCE_TIME counts 100-nanosecond units.
	r, _, _ = syscall.SyscallN(
		vtbl[audioClientVtbl](client).Initialize,
		uintptr(unsafe.Pointer(client)),
		shareModeShared,
		0,
		uintptr(int64(buffer/100)),
		0,
		uintptr(unsafe.Pointer(format)),
		0,
	)
	if err := hr(r, "IAudioClient.Initialize"); err != nil {
		client.Release()
		return nil, err
	}

	var capture *comObject
	r, _, _ = syscall.SyscallN(
		vtbl[audioClientVtbl](client).GetService,
		uintptr(unsafe.Pointer(client)),
		uintptr(unsafe.Pointer(iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capture)),
	)
	if err := hr(r, "GetService(IAudioCaptureClient)"); err != nil {
		client.Release()
		return nil, err
	}

	r, _, _ = syscall.SyscallN(
		vtbl[audioClientVtbl](client).Start,
		uintptr(unsafe.Pointer(client)),
	)
	if err := hr(r, "IAudioClient.Start"); err != nil {
		capture.Release()
		client.Release()
		return nil, err
	}

	return &Stream{client: client, capture: capture}, nil
}

// Drain discards whatever the device has captured since the last call.
//
// The samples are of no interest - the level comes from the meter - but a
// capture buffer nobody empties is a buffer the driver has to keep overrunning,
// so emptying it is the polite thing to do and costs a couple of calls.
func (s *Stream) Drain() error {
	for {
		var frames uint32
		r, _, _ := syscall.SyscallN(
			vtbl[captureClientVtbl](s.capture).GetNextPacketSize,
			uintptr(unsafe.Pointer(s.capture)),
			uintptr(unsafe.Pointer(&frames)),
		)
		if err := hr(r, "GetNextPacketSize"); err != nil {
			return err
		}
		if frames == 0 {
			return nil
		}

		var (
			data  uintptr
			read  uint32
			flags uint32
		)
		r, _, _ = syscall.SyscallN(
			vtbl[captureClientVtbl](s.capture).GetBuffer,
			uintptr(unsafe.Pointer(s.capture)),
			uintptr(unsafe.Pointer(&data)),
			uintptr(unsafe.Pointer(&read)),
			uintptr(unsafe.Pointer(&flags)),
			0, 0,
		)
		if err := hr(r, "GetBuffer"); err != nil {
			return err
		}
		r, _, _ = syscall.SyscallN(
			vtbl[captureClientVtbl](s.capture).ReleaseBuffer,
			uintptr(unsafe.Pointer(s.capture)),
			uintptr(read),
		)
		if err := hr(r, "ReleaseBuffer"); err != nil {
			return err
		}
	}
}

// Close stops the stream and releases it, which puts the microphone indicator
// out.
func (s *Stream) Close() {
	if s.client != nil {
		syscall.SyscallN(
			vtbl[audioClientVtbl](s.client).Stop,
			uintptr(unsafe.Pointer(s.client)),
		)
	}
	if s.capture != nil {
		s.capture.Release()
		s.capture = nil
	}
	if s.client != nil {
		s.client.Release()
		s.client = nil
	}
}

// activate returns a fresh interface on the device. The caller releases it.
func (d *Device) activate(iid *ole.GUID) (*comObject, error) {
	var obj *comObject
	r, _, _ := syscall.SyscallN(
		vtbl[deviceVtbl](d.dev).Activate,
		uintptr(unsafe.Pointer(d.dev)),
		uintptr(unsafe.Pointer(iid)),
		clsCtxAll, 0,
		uintptr(unsafe.Pointer(&obj)),
	)
	if err := hr(r, fmt.Sprintf("Activate(%s)", iid.String())); err != nil {
		return nil, err
	}
	return obj, nil
}
