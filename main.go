//go:build windows

// Command granag watches the microphone and reminds you to start
// recording in Granola when a conversation is under way and Granola is not
// capturing.
//
// This build carries the two read-only probes the detector is built on, so the
// behaviour can be checked before any notification logic exists:
//
//	granag list          every capture device and every microphone session
//	granag watch         a one-line-per-second timeline of level and holders
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"time"

	ole "github.com/go-ole/go-ole"

	"github.com/vusty/granag/internal/consent"
	"github.com/vusty/granag/internal/mic"
)

const (
	// defaultDevice matches the physical microphone rather than the NVIDIA
	// Broadcast virtual one: Broadcast's noise removal drives silence to an
	// exact zero, which makes "muted" and "quiet" indistinguishable.
	defaultDevice = "QuadCast"

	// speechThreshold separates speech from a muted or idle microphone. A
	// muted QuadCast S floors at about 0.0002 and speech runs 0.03 to 0.14,
	// so anything in between works; this sits two orders of magnitude above
	// the floor and well below the quietest speech.
	speechThreshold = 0.01

	samplePeriod = 100 * time.Millisecond
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	// COM lives on one thread for the process lifetime of these commands.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		fail(fmt.Errorf("CoInitializeEx: %w", err))
	}
	defer ole.CoUninitialize()

	switch os.Args[1] {
	case "list":
		if err := list(); err != nil {
			fail(err)
		}
	case "watch":
		if err := watch(os.Args[2:]); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: granag <list|watch> [flags]")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "granag:", err)
	os.Exit(1)
}

// list prints what the two probes see right now: the capture endpoints with a
// live level, and every microphone session Windows remembers.
func list() error {
	devs, err := mic.Devices()
	if err != nil {
		return err
	}
	defer func() {
		for _, d := range devs {
			d.Release()
		}
	}()

	fmt.Println("Active capture devices")
	for _, d := range devs {
		peak, err := d.Peak()
		if err != nil {
			fmt.Printf("  %-34s  %v\n", d.Name, err)
			continue
		}
		fmt.Printf("  %-34s  peak %.4f\n", d.Name, peak)
	}

	holders, err := consent.Holders()
	if err != nil {
		return err
	}
	sort.Slice(holders, func(i, j int) bool {
		if holders[i].Holding() != holders[j].Holding() {
			return holders[i].Holding()
		}
		return holders[i].Started.After(holders[j].Started)
	})

	fmt.Println("\nMicrophone sessions, most recent first")
	for _, h := range holders {
		when := h.Started.Format("01-02 15:04:05")
		if h.Holding() {
			fmt.Printf("  %-26s  since %s  HOLDING NOW\n", h.Exe, when)
			continue
		}
		fmt.Printf("  %-26s  %s  for %s\n", h.Exe, when,
			h.Stopped.Sub(h.Started).Round(time.Second))
	}
	return nil
}

// watch prints one line a second: the peak level over that second, whether it
// counts as speech, and which applications hold the microphone.
func watch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	device := fs.String("device", defaultDevice, "match this capture device by name")
	threshold := fs.Float64("threshold", speechThreshold, "peak level counted as speech")
	seconds := fs.Int("seconds", 0, "stop after this many seconds (0 runs until interrupted)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dev, err := mic.Find(*device)
	if err != nil {
		return err
	}
	defer dev.Release()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	fmt.Printf("watching %s, speech above %.4f\n\n", dev.Name, *threshold)
	fmt.Printf("%4s  %-8s  %-6s  %s\n", "sec", "peak", "speech", "holding the mic")

	ticker := time.NewTicker(samplePeriod)
	defer ticker.Stop()

	var (
		peak    float32
		samples int
		elapsed int
	)
	for {
		select {
		case <-stop:
			fmt.Println("\ninterrupted")
			return nil
		case <-ticker.C:
		}

		p, err := dev.Peak()
		if err != nil {
			return err
		}
		if p > peak {
			peak = p
		}
		if samples++; samples < int(time.Second/samplePeriod) {
			continue
		}

		elapsed++
		speech := "no"
		if float64(peak) > *threshold {
			speech = "YES"
		}
		fmt.Printf("%4d  %-8.4f  %-6s  %s\n", elapsed, peak, speech, holders())

		peak, samples = 0, 0
		if *seconds > 0 && elapsed >= *seconds {
			return nil
		}
	}
}

func holders() string {
	active, err := consent.Active()
	if err != nil {
		return "error: " + err.Error()
	}
	if len(active) == 0 {
		return "—"
	}
	names := make([]string, 0, len(active))
	for _, h := range active {
		names = append(names, h.Exe)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
