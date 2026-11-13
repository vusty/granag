//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vusty/granag/internal/calls"
)

// saveOpenEvery is how often the call under way is saved, which is how much
// of it a shutdown can cost: Windows ends a tray application at logoff without
// letting it stop cleanly.
const saveOpenEvery = time.Minute

// defaultCallsPath puts the call log next to the autostart log.
func defaultCallsPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return ""
	}
	return filepath.Join(base, "granag", "calls.jsonl")
}

// callLog feeds the poller's readings to a calls.Tracker and writes what it
// finishes. Only the poller goroutine touches it.
type callLog struct {
	path  string
	tr    *calls.Tracker
	saved time.Time
	open  bool
}

func newCallLog(path string, cfg calls.Config) (*callLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &callLog{path: path, tr: calls.New(cfg)}

	// A call that was under way when the tool last died: keep it as it was
	// at its last save.
	c, err := calls.LoadOpen(l.openPath())
	if err != nil {
		logf("call log: unreadable saved call, dropping it: %v", err)
	}
	if c != nil && l.tr.Keep(c) {
		l.write(c, "recovered ")
	}
	if err := calls.SaveOpen(l.openPath(), nil); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *callLog) openPath() string { return l.path + ".open" }

func (l *callLog) observe(now time.Time, micLive bool, holders []string) {
	if done := l.tr.Update(now, micLive, holders); done != nil {
		l.write(done, "")
	}
	open := l.tr.Open()
	switch {
	case open == nil && l.open:
		l.save(nil, now)
	case open != nil && now.Sub(l.saved) >= saveOpenEvery:
		l.save(open, now)
	}
}

// stop ends the call under way, for a clean exit.
func (l *callLog) stop() {
	if done := l.tr.Finish(); done != nil {
		l.write(done, "")
	}
	l.save(nil, time.Now())
}

func (l *callLog) save(c *calls.Call, now time.Time) {
	if err := calls.SaveOpen(l.openPath(), c); err != nil {
		logf("call log: saving the call under way: %v", err)
		return
	}
	l.saved, l.open = now, c != nil
}

func (l *callLog) write(c *calls.Call, how string) {
	if err := calls.Append(l.path, c); err != nil {
		logf("call log: %v", err)
		return
	}
	logf("%scall %s-%s, %s, microphone live %s, %s", how,
		c.Start.Format("15:04"), c.End.Format("15:04"), hm(c.Held), hm(c.Live),
		strings.Join(c.AppNames(), ", "))
}

// callsCmd prints the call log by day.
func callsCmd(args []string) error {
	fs := flag.NewFlagSet("calls", flag.ExitOnError)
	path := fs.String("file", defaultCallsPath(), "the call log")
	days := fs.Int("days", 7, "how many days back, today included")
	if err := fs.Parse(args); err != nil {
		return err
	}
	all, err := calls.Read(*path)
	if err != nil {
		return err
	}

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1-*days)

	var (
		day              string
		dayHeld, dayLive time.Duration
		totHeld, totLive time.Duration
		n                int
	)
	flush := func() {
		if day != "" {
			fmt.Printf("  %-13s %7s  live %6s\n\n", "day", hm(dayHeld), hm(dayLive))
		}
	}
	for _, c := range all {
		start := c.Start.Local()
		if start.Before(from) {
			continue
		}
		if d := start.Format("2006-01-02 Mon"); d != day {
			flush()
			day, dayHeld, dayLive = d, 0, 0
			fmt.Println(day)
		}
		fmt.Printf("  %s-%s   %7s  live %6s  %s\n", start.Format("15:04"), c.End.Local().Format("15:04"),
			hm(c.Held), hm(c.Live), strings.Join(c.AppNames(), ", "))
		dayHeld += c.Held
		dayLive += c.Live
		totHeld += c.Held
		totLive += c.Live
		n++
	}
	flush()
	if n == 0 {
		fmt.Printf("no calls in the last %d days in %s\n", *days, *path)
		return nil
	}
	fmt.Printf("%d calls, %s, microphone live %s\n", n, hm(totHeld), hm(totLive))
	return nil
}

// hm prints a duration as 1h58m or 22m.
func hm(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}
