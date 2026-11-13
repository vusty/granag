// Package calls keeps a log of calls: how long each one ran, which
// applications held the microphone, and for how much of it the microphone was
// live.
//
// A call is a stretch during which a call application captures the
// microphone. The microphone is muted in hardware except while talking, and
// the hardware mute never reaches Windows, so a call application keeps its
// capture running through a call however long one sits silent in it. Live
// time is therefore close to the time spent talking.
//
// Like package nag, the tracker is a pure function of time, of whether the
// microphone is live, and of who holds it, so it is tested without Windows.
package calls

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// DefaultApps are the executables whose grip on the microphone means a call.
//
// NVIDIA Broadcast is not one: it holds the physical microphone whenever
// anything listens to its virtual one. Games are not either, though some
// capture for their own voice chat.
var DefaultApps = []string{
	"chrome.exe", // Google Meet
	"msedge.exe",
	"Discord.exe",
	"Telegram.exe",
	// A conversation in the room has no call application at all; Granola
	// recording it is the only sign one is happening.
	"Granola.exe",
}

const (
	// DefaultGap is how long nobody may hold the microphone before the call
	// counts as over. A rejoin, or a move from Discord to Meet, stays one call.
	DefaultGap = time.Minute

	// DefaultMinLength drops captures too short to be calls: a voice
	// message, a device test.
	DefaultMinLength = time.Minute

	// DefaultMaxStep is the longest gap between two polls still counted as
	// time passing in a call. Anything longer is sleep or a lost microphone;
	// the open call ends where the polls stopped.
	DefaultMaxStep = time.Minute
)

// Config is the whole behaviour of the tracker.
type Config struct {
	Apps      []string
	Gap       time.Duration
	MinLength time.Duration
	MaxStep   time.Duration
}

// DefaultConfig returns the configuration the tool ships with.
func DefaultConfig() Config {
	return Config{
		Apps:      slices.Clone(DefaultApps),
		Gap:       DefaultGap,
		MinLength: DefaultMinLength,
		MaxStep:   DefaultMaxStep,
	}
}

// Call is one finished call, as it is written to the log.
type Call struct {
	Start time.Time
	// End is the last moment a call application was seen holding the
	// microphone, so the trailing gap is not counted.
	End time.Time
	// Held is the time a call application held the microphone; the gaps
	// inside the call are not in it.
	Held time.Duration
	// Live is the part of Held during which the microphone was unmuted.
	Live time.Duration
	// Apps is how long each call application held the microphone.
	Apps map[string]time.Duration
}

// record is a Call as one JSON line: seconds rather than durations, so a
// script reading the log needs no Go to make sense of it.
type record struct {
	Start       time.Time      `json:"start"`
	End         time.Time      `json:"end"`
	Seconds     int            `json:"seconds"`
	LiveSeconds int            `json:"mic_live_seconds"`
	Apps        map[string]int `json:"apps"`
}

// MarshalJSON writes durations as whole seconds.
func (c Call) MarshalJSON() ([]byte, error) {
	r := record{
		Start:       c.Start,
		End:         c.End,
		Seconds:     int(c.Held.Round(time.Second) / time.Second),
		LiveSeconds: int(c.Live.Round(time.Second) / time.Second),
		Apps:        make(map[string]int, len(c.Apps)),
	}
	for app, d := range c.Apps {
		r.Apps[app] = int(d.Round(time.Second) / time.Second)
	}
	return json.Marshal(r)
}

// UnmarshalJSON reads what MarshalJSON writes.
func (c *Call) UnmarshalJSON(b []byte) error {
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	c.Start, c.End = r.Start, r.End
	c.Held = time.Duration(r.Seconds) * time.Second
	c.Live = time.Duration(r.LiveSeconds) * time.Second
	c.Apps = make(map[string]time.Duration, len(r.Apps))
	for app, s := range r.Apps {
		c.Apps[app] = time.Duration(s) * time.Second
	}
	return nil
}

// AppNames returns the applications that held the microphone, longest first.
func (c Call) AppNames() []string {
	names := make([]string, 0, len(c.Apps))
	for app := range c.Apps {
		names = append(names, app)
	}
	sort.Slice(names, func(i, j int) bool {
		if c.Apps[names[i]] != c.Apps[names[j]] {
			return c.Apps[names[i]] > c.Apps[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

// Tracker turns polls into calls.
//
// Only the poller calls it, so it takes no lock.
type Tracker struct {
	cfg  Config
	last time.Time
	cur  *Call
}

// New returns a Tracker with cfg, filling in defaults for zero fields.
func New(cfg Config) *Tracker {
	if cfg.Apps == nil {
		cfg.Apps = slices.Clone(DefaultApps)
	}
	if cfg.Gap == 0 {
		cfg.Gap = DefaultGap
	}
	if cfg.MinLength == 0 {
		cfg.MinLength = DefaultMinLength
	}
	if cfg.MaxStep == 0 {
		cfg.MaxStep = DefaultMaxStep
	}
	return &Tracker{cfg: cfg}
}

// Update takes one poll and returns the call it finished, if any.
//
// The time since the previous poll is credited to what this poll sees, which
// at a poll every two seconds is as exact as the log needs to be.
func (t *Tracker) Update(now time.Time, micLive bool, holders []string) *Call {
	step := time.Duration(0)
	if !t.last.IsZero() {
		step = now.Sub(t.last)
	}
	t.last = now

	var done *Call
	if step < 0 || step > t.cfg.MaxStep {
		// Asleep, or the microphone was gone: whatever was open ended when
		// the polls stopped, and the dark stretch counts for nothing.
		step = 0
		done = t.finish()
	}

	held := t.callApps(holders)
	switch {
	case len(held) > 0:
		if t.cur == nil {
			t.cur = &Call{Start: now, Apps: map[string]time.Duration{}}
			step = 0
		}
		t.cur.End = now
		t.cur.Held += step
		if micLive {
			t.cur.Live += step
		}
		for _, app := range held {
			t.cur.Apps[app] += step
		}
	case t.cur != nil && now.Sub(t.cur.End) >= t.cfg.Gap:
		done = t.finish()
	}
	return done
}

// Open returns a copy of the call under way, or nil.
func (t *Tracker) Open() *Call {
	if t.cur == nil {
		return nil
	}
	c := *t.cur
	c.Apps = make(map[string]time.Duration, len(t.cur.Apps))
	for app, d := range t.cur.Apps {
		c.Apps[app] = d
	}
	return &c
}

// Finish ends the call under way, for when the tool stops, and returns it if
// it is long enough to keep.
func (t *Tracker) Finish() *Call {
	return t.finish()
}

func (t *Tracker) finish() *Call {
	c := t.cur
	t.cur = nil
	if c == nil || !t.Keep(c) {
		return nil
	}
	return c
}

// Keep reports whether c is long enough to be a call rather than a blip.
func (t *Tracker) Keep(c *Call) bool {
	return c.End.Sub(c.Start) >= t.cfg.MinLength
}

func (t *Tracker) callApps(holders []string) []string {
	var held []string
	for _, h := range holders {
		for _, app := range t.cfg.Apps {
			if strings.EqualFold(h, app) {
				held = append(held, app)
				break
			}
		}
	}
	return held
}

// Append writes c as one line at the end of the log at path.
func Append(path string, c *Call) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Read returns every call in the log at path, oldest first. A missing log is
// an empty one.
func Read(path string) ([]Call, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}

func parse(r io.Reader) ([]Call, error) {
	var out []Call
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Call
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// SaveOpen writes the call under way to path, so a shutdown that never lets
// the tool stop cleanly loses at most the time since the last save. A nil
// call removes the file.
func SaveOpen(path string, c *Call) error {
	if c == nil {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadOpen reads what SaveOpen left behind, or nil if nothing.
func LoadOpen(path string) (*Call, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Call
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
