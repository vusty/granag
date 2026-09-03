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

	// micOnThreshold is the level above which the microphone counts as
	// unmuted, measured on a QuadCast S with a stream held open. Muted it
	// floors at 0.0002, reliably; unmuted and silent it has read anywhere from
	// 0.0063 to 0.054, depending on the gain knob and on how quiet the room
	// is. So the margin is lopsided on purpose - a comfortable tenfold above
	// the muted floor, and only threefold below the quietest live reading
	// seen - because a missed reminder costs a transcript while a false one
	// costs a notification.
	micOnThreshold = 0.002

	// speechThreshold is only for the watch command's display. Speech runs
	// 0.03 upwards, but so does an unmuted microphone in a quiet room, so
	// this tells presence apart rather than speech.
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
	if err := initCOM(); err != nil {
		fail(err)
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

// initCOM initialises COM on the current thread.
//
// S_FALSE means the thread already had COM in the same mode, which is success
// and still takes a reference, so the caller uninitialises either way. go-ole
// reports every non-zero HRESULT as an error though, S_FALSE included, so the
// difference has to be sorted out here - and it comes up for real: the tray
// owns the main thread while the poller runs on its own, and -no-tray puts both
// on the same one.
func initCOM() error {
	err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	var oleErr *ole.OleError
	if errors.As(err, &oleErr) && oleErr.Code() == 1 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("CoInitializeEx: %w", err)
	}
	return nil
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

// runOpts is what the poll loop needs beyond the state machine.
type runOpts struct {
	device      string
	every       time.Duration
	threshold   float64
	buffer      time.Duration
	dry         bool
	keepVolume  bool
	volumeEvery time.Duration
	status      func(string)
}

// run is the reminder itself: hold the microphone open so its level can be
// read, and remind whenever it is live and Granola is not recording.
func run(args []string) error {
	cfg := nag.DefaultConfig()
	o := runOpts{device: defaultDevice, buffer: mic.DefaultBuffer, volumeEvery: 30 * time.Second}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	noTray := fs.Bool("no-tray", false, "stay in the terminal, without the tray icon")
	logPath := fs.String("log", "", "append the log to this file instead of stdout")
	suppress := fs.String("suppress", strings.Join(cfg.Suppress, ","),
		"comma-separated executables that call the reminder off while they hold the microphone")
	repeats := fs.String("repeats", durations(cfg.Repeats),
		"comma-separated gaps after the first reminder; their count caps reminders per conversation, and empty means remind once")
	fs.StringVar(&o.device, "device", o.device, "match this capture device by name")
	fs.DurationVar(&o.every, "poll", 2*time.Second, "how often to read the level and the consent store")
	fs.Float64Var(&o.threshold, "on-threshold", micOnThreshold,
		"level above which the microphone counts as unmuted")
	fs.BoolVar(&o.dry, "dry-run", false, "log reminders instead of showing them")
	fs.BoolVar(&o.keepVolume, "keep-volume", true,
		"hold every capture device's input volume at maximum")
	fs.DurationVar(&cfg.Debounce, "debounce", cfg.Debounce,
		"how long the microphone must stay live before the first reminder")
	fs.StringVar(&cfg.Granola, "granola", cfg.Granola,
		"executable whose grip on the microphone means the conversation is being recorded")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Suppress = splitList(*suppress)
	var err error
	if cfg.Repeats, err = parseDurations(*repeats); err != nil {
		return fmt.Errorf("-repeats: %w", err)
	}

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

	logf("reminder after %s live microphone, repeats %v, suppressed by %s",
		cfg.Debounce, cfg.Repeats, strings.Join(cfg.Suppress, ", "))

	if *noTray {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		go func() {
			<-stop
			cancel()
		}()
		return poll(ctx, state, o)
	}

	o.status = tray.SetStatus
	errc := make(chan error, 1)
	go func() { errc <- poll(ctx, state, o) }()

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

// poll holds a capture stream open and drives the state machine until ctx is
// done.
//
// The stream is the whole reason the level is readable: with nothing capturing,
// the meter reads the same flat value whether the microphone is muted or being
// talked into. Holding it lights the Windows microphone indicator for as long
// as the tool runs, which is the price of noticing a conversation that nobody
// is dialling into.
func poll(ctx context.Context, state *nag.State, o runOpts) error {
	// COM is per-thread, and this runs on its own goroutine while the tray
	// owns the main one.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initCOM(); err != nil {
		return err
	}
	defer ole.CoUninitialize()

	dev, err := mic.Find(o.device)
	if err != nil {
		return err
	}
	defer dev.Release()

	stream, err := dev.Hold(o.buffer)
	if err != nil {
		return fmt.Errorf("holding %s open: %w", dev.Name, err)
	}
	defer stream.Close()
	logf("holding %s open; unmuted above %.4f", dev.Name, o.threshold)

	ticker := time.NewTicker(o.every)
	defer ticker.Stop()

	volumeTick := time.NewTicker(o.volumeEvery)
	defer volumeTick.Stop()
	if o.keepVolume {
		keepVolume()
	}

	var wasLive bool
	for {
		select {
		case <-ctx.Done():
			logf("stopped")
			return nil
		case <-volumeTick.C:
			if o.keepVolume {
				keepVolume()
			}
			continue
		case <-ticker.C:
		}

		if err := stream.Drain(); err != nil {
			return err
		}
		peak, err := dev.Peak()
		if err != nil {
			return err
		}
		micOn := float64(peak) > o.threshold

		active, err := consent.Active()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(active))
		for _, h := range active {
			names = append(names, h.Exe)
		}
		sort.Strings(names)

		remind := state.Update(time.Now(), micOn, names)

		if live := state.Live(); live != wasLive {
			if live {
				logf("microphone live (peak %.4f), held by %s", peak, orNone(names))
			} else {
				logf("microphone quiet")
			}
			wasLive = live
		}
		if o.status != nil {
			switch {
			case state.Paused():
				o.status("пауза")
			case state.Live():
				o.status("микрофон включён, запись не идёт")
			default:
				o.status("слежу за микрофоном")
			}
		}
		if !remind {
			continue
		}

		body := "Микрофон включён, а Granola не пишет."
		if o.dry {
			logf("would remind: %s", body)
			continue
		}
		logf("reminding (%d of this conversation)", state.Sent())
		if err := (notify.Toast{Title: "Granola не записывает", Body: body}).Show(); err != nil {
			logf("toast failed: %v", err)
		}
	}
}

// keepVolume puts every active capture device back to full input volume.
//
// Windows drops the input volume to zero across some restarts, and a
// zero-volume microphone is silent to the other side while everything still
// looks connected - the failure you learn about a minute into a call. The gain
// that matters lives on the microphone itself, so there is nothing this slider
// should ever be doing except sitting at maximum.
func keepVolume() {
	devs, err := mic.Devices()
	if err != nil {
		logf("volume check failed: %v", err)
		return
	}
	for _, d := range devs {
		raised, err := d.RaiseToMax()
		switch {
		case err != nil:
			logf("%s: could not raise the input volume: %v", d.Name, err)
		case raised:
			logf("%s: input volume was below maximum, raised it", d.Name)
		}
		d.Release()
	}
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "nobody"
	}
	return strings.Join(names, ", ")
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
		// Anything after "on" is passed to run at logon, which is how the log
		// file gets set: an autostarted process has no console, so without one
		// there is no way to see afterwards what it thought was happening.
		value := `"` + self + `" run`
		for _, arg := range args[1:] {
			if strings.ContainsAny(arg, ` "`) {
				value += ` "` + arg + `"`
				continue
			}
			value += " " + arg
		}
		if err := k.SetStringValue("granag", value); err != nil {
			return err
		}
		fmt.Println("autostart on:", value)
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

// durations renders a gap list the way -repeats accepts it back.
func durations(ds []time.Duration) string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.String())
	}
	return strings.Join(out, ",")
}

func parseDurations(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range splitList(s) {
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("%s is not a gap", part)
		}
		out = append(out, d)
	}
	return out, nil
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
