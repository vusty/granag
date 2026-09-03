package nag

import (
	"testing"
	"time"
)

const (
	on  = true
	off = false
)

// clock advances a fake now, so the tests read as a timeline rather than as
// arithmetic on timestamps.
type clock struct {
	t     time.Time
	state *State
}

func newClock(cfg Config) *clock {
	return &clock{t: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), state: New(cfg)}
}

// after advances the clock by d and reports whether a reminder fired.
func (c *clock) after(d time.Duration, micOn bool, holders ...string) bool {
	c.t = c.t.Add(d)
	return c.state.Update(c.t, micOn, holders)
}

func TestMutedMicrophoneNeverReminds(t *testing.T) {
	c := newClock(DefaultConfig())
	for i := 0; i < 10; i++ {
		if c.after(time.Minute, off) {
			t.Fatal("reminded with the microphone muted")
		}
	}
}

func TestFirstReminderWaitsOutDebounce(t *testing.T) {
	c := newClock(DefaultConfig())

	if c.after(time.Second, on) {
		t.Fatal("reminded immediately, debounce ignored")
	}
	if c.after(DefaultDebounce-2*time.Second, on) {
		t.Fatal("reminded before the debounce elapsed")
	}
	if !c.after(3*time.Second, on) {
		t.Fatal("no reminder after the debounce elapsed")
	}
}

// A call needs no application to hold the microphone for the reminder to fire:
// the trigger is the microphone being live, which covers a meeting in the room.
func TestNobodyCapturingStillReminds(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, on)
	if !c.after(DefaultDebounce, on) {
		t.Fatal("no reminder with the microphone live and nobody capturing")
	}
}

func TestRepeatsAreCappedAndSpaced(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, on) // the poll that first sees the microphone live
	if !c.after(DefaultDebounce, on) {
		t.Fatal("no first reminder")
	}
	if c.after(DefaultRepeats[0]-time.Second, on) {
		t.Fatal("second reminder came early")
	}
	if !c.after(time.Second, on) {
		t.Fatal("no second reminder")
	}
	if !c.after(DefaultRepeats[1], on) {
		t.Fatal("no third reminder")
	}

	// Three is the cap: an hour of the same conversation earns nothing more.
	for i := 0; i < 6; i++ {
		if c.after(10*time.Minute, on) {
			t.Fatal("reminded past the cap")
		}
	}
	if got := c.state.Sent(); got != 3 {
		t.Fatalf("sent %d reminders, want 3", got)
	}
}

func TestGranolaRecordingSilencesAndResets(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, on)
	if !c.after(DefaultDebounce, on) {
		t.Fatal("no first reminder")
	}
	// Granola starts recording: the reminder stops mid-conversation.
	if c.after(time.Minute, on, "Granola.exe") {
		t.Fatal("reminded while Granola was recording")
	}
	if c.state.Live() {
		t.Fatal("still tracking a conversation Granola is recording")
	}
	// It stops again, and the stretch is treated as new.
	if c.after(time.Second, on) {
		t.Fatal("reminded without waiting out the new debounce")
	}
	if !c.after(DefaultDebounce, on) {
		t.Fatal("no reminder after Granola stopped recording")
	}
}

func TestSuppressedHoldersCallItOff(t *testing.T) {
	c := newClock(DefaultConfig())

	// Gaming: the microphone is live for hours and no transcript is wanted.
	for i := 0; i < 5; i++ {
		if c.after(10*time.Minute, on, "Discord.exe") {
			t.Fatal("reminded while Discord held the microphone")
		}
	}
	// The game ends, the microphone stays live: reminders come back.
	c.after(0, on)
	if !c.after(DefaultDebounce, on) {
		t.Fatal("no reminder once Discord let go")
	}
}

// Everything else holding the microphone is beside the point now that the
// trigger is the microphone itself - notably NVIDIA Broadcast, which takes the
// physical device whenever anything takes its virtual one.
func TestOtherHoldersDoNotCallItOff(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, on, "NVIDIA Broadcast.exe", "chrome.exe")
	if !c.after(DefaultDebounce, on, "NVIDIA Broadcast.exe", "chrome.exe") {
		t.Fatal("a call with Broadcast in the way got no reminder")
	}
}

func TestMutingResetsTheCounter(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, on)
	if !c.after(DefaultDebounce, on) {
		t.Fatal("no first reminder")
	}
	if c.after(time.Second, off) {
		t.Fatal("reminded after the microphone was muted")
	}
	if got := c.state.Sent(); got != 0 {
		t.Fatalf("counter left at %d after muting, want 0", got)
	}
	c.after(0, on)
	if !c.after(DefaultDebounce, on) {
		t.Fatal("the next conversation got no reminder")
	}
}

func TestPauseKeepsTheConversation(t *testing.T) {
	c := newClock(DefaultConfig())
	c.state.SetPaused(true)

	c.after(0, on)
	if c.after(DefaultDebounce*2, on) {
		t.Fatal("reminded while paused")
	}
	if !c.state.Live() {
		t.Fatal("pausing lost the conversation")
	}
	// Unpausing mid-conversation does not restart the debounce: the microphone
	// has been live the whole time.
	c.state.SetPaused(false)
	if !c.after(time.Second, on) {
		t.Fatal("no reminder after unpausing mid-conversation")
	}
}
