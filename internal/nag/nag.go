// Package nag decides when to remind about Granola.
//
// The decision is a pure function of time and of which applications hold the
// microphone, so it carries no Windows dependency and is tested directly.
package nag

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Defaults for Config. A conversation has to hold for Debounce before the
// first reminder, so a voice message or a microphone test says nothing.
const (
	DefaultGranola  = "Granola.exe"
	DefaultDebounce = 30 * time.Second
)

// DefaultRepeats are the gaps after the first reminder. The list also caps how
// many reminders one conversation can get: three, and then silence. A reminder
// that keeps arriving is a reminder that gets dismissed without reading.
var DefaultRepeats = []time.Duration{3 * time.Minute, 10 * time.Minute}

// DefaultIgnore lists applications whose grip on the microphone does not mean
// a conversation worth transcribing.
var DefaultIgnore = []string{
	// Opens the physical microphone on demand, when something takes its
	// virtual one, so it always appears alongside the real client rather than
	// instead of it. Counting it would make every call look like two.
	"NVIDIA Broadcast.exe",
	// The sound settings page, which captures to draw its level bar.
	"windows.immersivecontrolpanel_cw5n1h2txyewy",
	// Gaming with friends is not a meeting.
	"Discord.exe",
}

// Config is the whole behaviour of the reminder.
type Config struct {
	// Granola is the executable whose grip means the conversation is already
	// being recorded.
	Granola string
	// Ignore lists executables that do not count as a conversation.
	Ignore []string
	// Debounce is how long a conversation must hold before the first reminder.
	Debounce time.Duration
	// Repeats are the gaps after the first reminder, and their count caps the
	// reminders per conversation.
	Repeats []time.Duration
}

// DefaultConfig returns the configuration the tool ships with.
func DefaultConfig() Config {
	return Config{
		Granola:  DefaultGranola,
		Ignore:   slices.Clone(DefaultIgnore),
		Debounce: DefaultDebounce,
		Repeats:  slices.Clone(DefaultRepeats),
	}
}

// State tracks one conversation at a time.
//
// The tray toggles the pause from its own goroutine while the poller calls
// Update from another, so every method takes the lock.
type State struct {
	mu  sync.Mutex
	cfg Config

	// talking is set while a conversation is under way; since is when it
	// started, and sent counts the reminders it has already had.
	talking bool
	since   time.Time
	sent    int
	last    time.Time

	// paused suppresses reminders without losing the conversation, for the
	// tray toggle.
	paused bool
}

// New returns a State with cfg, filling in defaults for zero fields.
func New(cfg Config) *State {
	if cfg.Granola == "" {
		cfg.Granola = DefaultGranola
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = DefaultDebounce
	}
	return &State{cfg: cfg}
}

// SetPaused turns reminders off and on. Pausing keeps the current
// conversation, so unpausing mid-call does not start its debounce over.
func (s *State) SetPaused(p bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = p
}

// Paused reports whether reminders are suppressed.
func (s *State) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// Update feeds the current microphone holders in and reports whether to remind
// now. holders are executable names as the consent store spells them.
//
// Reminders stop as soon as Granola takes the microphone, and the counter
// resets when the conversation ends, so the next one starts from a clean slate.
func (s *State) Update(now time.Time, holders []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	recording := s.matches(holders, s.cfg.Granola)
	conversation := false
	for _, h := range holders {
		if s.ignored(h) || strings.EqualFold(h, s.cfg.Granola) {
			continue
		}
		conversation = true
		break
	}

	// Granola is recording, or there is nobody to record: nothing to remind
	// about, and the next conversation gets its reminders back.
	if !conversation || recording {
		s.talking = false
		s.sent = 0
		return false
	}

	if !s.talking {
		s.talking = true
		s.since = now
		s.sent = 0
	}
	if s.paused {
		return false
	}

	var wait time.Duration
	switch {
	case s.sent == 0:
		wait = s.cfg.Debounce
	case s.sent <= len(s.cfg.Repeats):
		wait = s.cfg.Repeats[s.sent-1]
	default:
		// This conversation has had every reminder it gets.
		return false
	}

	from := s.since
	if s.sent > 0 {
		from = s.last
	}
	if now.Sub(from) < wait {
		return false
	}

	s.sent++
	s.last = now
	return true
}

// Talking reports whether a conversation is currently under way.
func (s *State) Talking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.talking
}

// Sent reports how many reminders the current conversation has had.
func (s *State) Sent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func (s *State) ignored(exe string) bool {
	return slices.ContainsFunc(s.cfg.Ignore, func(i string) bool {
		return strings.EqualFold(i, exe)
	})
}

func (s *State) matches(holders []string, exe string) bool {
	return slices.ContainsFunc(holders, func(h string) bool {
		return strings.EqualFold(h, exe)
	})
}
