// Package nag decides when to remind about Granola.
//
// The decision is a pure function of time, of whether the microphone is live,
// and of which applications hold it, so it carries no Windows dependency and is
// tested directly.
package nag

import (
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultGranola is the executable whose grip on the microphone means the
	// conversation is already being recorded.
	DefaultGranola = "Granola.exe"

	// DefaultDebounce is how long the microphone must stay live before the
	// first reminder. It is short on purpose: the microphone is muted in
	// hardware except when a conversation is about to happen, so an unmuted
	// one is already a deliberate act rather than something to wait out.
	DefaultDebounce = 10 * time.Second
)

// DefaultRepeats are the gaps after the first reminder. The list also caps how
// many reminders one conversation can get: two, and then silence.
//
// Two rather than more because unmuting the microphone is deliberate here - if
// the first reminder went unanswered, the odds are it was meant to, and the
// second is only there for having been distracted mid-thought.
var DefaultRepeats = []time.Duration{3 * time.Minute}

// DefaultSuppress lists applications whose grip on the microphone means the
// reminder is unwanted even though the microphone is live.
var DefaultSuppress = []string{
	// Gaming with friends is talking with the microphone live, and no
	// transcript is wanted.
	"Discord.exe",
}

// Config is the whole behaviour of the reminder.
type Config struct {
	// Granola is the executable whose grip means the conversation is already
	// being recorded.
	Granola string
	// Suppress lists executables that call the reminder off while they hold
	// the microphone.
	Suppress []string
	// Debounce is how long the microphone must stay live before the first
	// reminder.
	Debounce time.Duration
	// Repeats are the gaps after the first reminder, and their count caps the
	// reminders per conversation.
	Repeats []time.Duration
}

// DefaultConfig returns the configuration the tool ships with.
func DefaultConfig() Config {
	return Config{
		Granola:  DefaultGranola,
		Suppress: slices.Clone(DefaultSuppress),
		Debounce: DefaultDebounce,
		Repeats:  slices.Clone(DefaultRepeats),
	}
}

// State tracks one live stretch of microphone at a time.
//
// The tray toggles the pause from its own goroutine while the poller calls
// Update from another, so every method takes the lock.
type State struct {
	mu  sync.Mutex
	cfg Config

	// live is set while the microphone is unmuted and unrecorded; since is
	// when that started, and sent counts the reminders it has already had.
	live  bool
	since time.Time
	sent  int
	last  time.Time

	// paused suppresses reminders without losing the current stretch, for the
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

// SetPaused turns reminders off and on. Pausing keeps the current stretch, so
// unpausing mid-conversation does not start its debounce over.
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

// Update reports whether to remind now.
//
// micOn is whether the microphone is unmuted, read from its level; holders are
// the executables capturing it, as the consent store spells them. Reminders
// stop as soon as Granola takes the microphone, and the counter resets when the
// microphone goes quiet, so the next conversation starts from a clean slate.
func (s *State) Update(now time.Time, micOn bool, holders []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	recording := contains(holders, s.cfg.Granola)
	suppressed := slices.ContainsFunc(s.cfg.Suppress, func(exe string) bool {
		return contains(holders, exe)
	})

	// Muted, already being recorded, or deliberately not our business: nothing
	// to remind about, and the next conversation gets its reminders back.
	if !micOn || recording || suppressed {
		s.live = false
		s.sent = 0
		return false
	}

	if !s.live {
		s.live = true
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

// Live reports whether the microphone is currently live and unrecorded.
func (s *State) Live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// Sent reports how many reminders the current conversation has had.
func (s *State) Sent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func contains(holders []string, exe string) bool {
	return slices.ContainsFunc(holders, func(h string) bool {
		return strings.EqualFold(h, exe)
	})
}
