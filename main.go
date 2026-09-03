//go:build windows

// Command granag watches the microphone and reminds you to start
// recording in Granola when a conversation is under way and Granola is not
// capturing.
//
//	granag run     the reminder itself
//	granag list    every capture device and every microphone session
//	granag watch   a one-line-per-second timeline of level and holders
//	granag toast   fire one notification, to prove toasts reach the screen
//
// list and watch are the probes the detector was designed against, kept
// because they are the only way to tell a broken reading from a quiet room.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"

	ole "github.com/go-ole/go-ole"

	"github.com/vusty/granag/internal/consent"
	"github.com/vusty/granag/internal/mic"
	"github.com/vusty/granag/internal/nag"
	"github.com/vusty/granag/internal/notify"
	"github.com/vusty/granag/internal/tray"
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
	case "run":
		if err := run(os.Args[2:]); err != nil {
			fail(err)
		}
	case "autostart":
		if err := autostartCmd(os.Args[2:]); err != nil {
			fail(err)
		}
	case "toast":
		if err := (notify.Toast{
			Title: "granag",
			Body:  "Тестовое уведомление — значит тосты работают.",
		}).Show(); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: granag <run|list|watch|toast|autostart> [flags]")
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

// run is the reminder itself: read the consent store on a timer, feed the
// state machine, show a toast when it asks for one.
//
// Polling rather than waiting on RegNotifyChangeKeyValue is deliberate. The
// read is a dozen registry subkeys, microseconds of work, and the state machine
// needs its own timers for the debounce and the repeats regardless - so an
// event-driven registry watch would remove no timer and add a syscall loop.
func run(args []string) error {
	cfg := nag.DefaultConfig()

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	every := fs.Duration("poll", 2*time.Second, "how often to read the consent store")
	dry := fs.Bool("dry-run", false, "log reminders instead of showing them")
	noTray := fs.Bool("no-tray", false, "stay in the terminal, without the tray icon")
	logPath := fs.String("log", "", "append the log to this file instead of stdout")
	ignore := fs.String("ignore", strings.Join(cfg.Ignore, ","),
		"comma-separated executables that do not count as a conversation")
	fs.DurationVar(&cfg.Debounce, "debounce", cfg.Debounce,
		"how long a conversation must hold before the first reminder")
	fs.StringVar(&cfg.Granola, "granola", cfg.Granola,
		"executable whose grip on the microphone means the conversation is being recorded")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Ignore = splitList(*ignore)

	// Autostarted there is no console to write to, so a log file is the only
	// way to find out afterwards what the tool thought was happening.
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		logOut = f
	}

	state := nag.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logf("watching for conversations; reminder after %s, repeats %v, ignoring %s",
		cfg.Debounce, cfg.Repeats, strings.Join(cfg.Ignore, ", "))

	if *noTray {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		go func() {
			<-stop
			cancel()
		}()
		return poll(ctx, state, *every, *dry, nil)
	}

	errc := make(chan error, 1)
	go func() { errc <- poll(ctx, state, *every, *dry, tray.SetStatus) }()

	tray.Run(tray.Options{
		OnPause: func(paused bool) {
			state.SetPaused(paused)
			if paused {
				logf("reminders paused")
			} else {
				logf("reminders resumed")
			}
		},
		OnOpenGranola: openGranola,
		OnQuit:        cancel,
	})

	cancel()
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

// poll drives the state machine until ctx is done. status, when set, receives
// short lines for the tray tooltip.
func poll(ctx context.Context, state *nag.State, every time.Duration, dry bool, status func(string)) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	var previous string
	for {
		select {
		case <-ctx.Done():
			logf("stopped")
			return nil
		case <-ticker.C:
		}

		active, err := consent.Active()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(active))
		for _, h := range active {
			names = append(names, h.Exe)
		}
		sort.Strings(names)

		// Log only transitions, so a day of running leaves a readable trail.
		if current := strings.Join(names, ", "); current != previous {
			if current == "" {
				logf("microphone free")
			} else {
				logf("microphone held by %s", current)
			}
			previous = current
		}

		remind := state.Update(time.Now(), names)
		if status != nil && !state.Paused() {
			switch {
			case state.Talking():
				status("разговор идёт, запись не включена")
			default:
				status("слежу за разговорами")
			}
		}
		if !remind {
			continue
		}

		body := "Идёт разговор, а запись не включена."
		if len(names) > 0 {
			body = fmt.Sprintf("Микрофон держит %s, а запись не включена.", strings.Join(names, ", "))
		}
		if dry {
			logf("would remind: %s", body)
			continue
		}
		logf("reminding (%d of this conversation)", state.Sent())
		if err := (notify.Toast{Title: "Granola не записывает", Body: body}).Show(); err != nil {
			logf("toast failed: %v", err)
		}
	}
}

// granolaExe finds Granola through the consent store, which records the full
// path of everything that has ever asked for the microphone. Granola has, so
// there is no need to guess at install locations.
func granolaExe() (string, error) {
	holders, err := consent.Holders()
	if err != nil {
		return "", err
	}
	for _, h := range holders {
		if strings.EqualFold(h.Exe, nag.DefaultGranola) {
			return h.Path, nil
		}
	}
	return "", fmt.Errorf("no %s in the microphone consent store", nag.DefaultGranola)
}

func openGranola() error {
	path, err := granolaExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

const autostartKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartCmd turns logon startup on and off through the Run key. A registry
// value needs no COM, unlike a Start Menu shortcut, and Windows honours it the
// same way.
func autostartCmd(args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	switch action {
	case "on":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		if err := k.SetStringValue("granag", `"`+self+`" run`); err != nil {
			return err
		}
		fmt.Println("autostart on:", self)
	case "off":
		if err := k.DeleteValue("granag"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		fmt.Println("autostart off")
	case "status":
		v, _, err := k.GetStringValue("granag")
		if errors.Is(err, registry.ErrNotExist) {
			fmt.Println("autostart off")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println("autostart on:", v)
	default:
		return fmt.Errorf("autostart: want on, off or status, got %q", action)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// logOut is where logf writes; -log points it at a file.
var logOut io.Writer = os.Stdout

func logf(format string, args ...any) {
	fmt.Fprintf(logOut, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
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
