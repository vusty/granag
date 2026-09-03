package nag

import (
	"testing"
	"time"
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

// after advances the clock and reports whether a reminder fired.
func (c *clock) after(d time.Duration, holders ...string) bool {
	c.t = c.t.Add(d)
	return c.state.Update(c.t, holders)
}

func TestSilenceNeverReminds(t *testing.T) {
	c := newClock(DefaultConfig())
	for i := 0; i < 10; i++ {
		if c.after(time.Minute) {
			t.Fatal("reminded with nobody holding the microphone")
		}
	}
}

func TestFirstReminderWaitsOutDebounce(t *testing.T) {
	c := newClock(DefaultConfig())

	if c.after(time.Second, "chrome.exe") {
		t.Fatal("reminded immediately, debounce ignored")
	}
	if c.after(DefaultDebounce-2*time.Second, "chrome.exe") {
		t.Fatal("reminded before the debounce elapsed")
	}
	if !c.after(3*time.Second, "chrome.exe") {
		t.Fatal("no reminder after the debounce elapsed")
	}
}

func TestRepeatsAreCappedAndSpaced(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, "chrome.exe") // the poll that first sees the call
	if !c.after(DefaultDebounce, "chrome.exe") {
		t.Fatal("no first reminder")
	}
	if c.after(DefaultRepeats[0]-time.Second, "chrome.exe") {
		t.Fatal("second reminder came early")
	}
	if !c.after(time.Second, "chrome.exe") {
		t.Fatal("no second reminder")
	}
	if !c.after(DefaultRepeats[1], "chrome.exe") {
		t.Fatal("no third reminder")
	}

	// Three is the cap: an hour of the same conversation earns nothing more.
	for i := 0; i < 6; i++ {
		if c.after(10*time.Minute, "chrome.exe") {
			t.Fatal("reminded past the cap")
		}
	}
	if got := c.state.Sent(); got != 3 {
		t.Fatalf("sent %d reminders, want 3", got)
	}
}

func TestGranolaRecordingSilencesAndResets(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, "chrome.exe")
	if !c.after(DefaultDebounce, "chrome.exe") {
		t.Fatal("no first reminder")
	}
	// Granola starts recording: the reminder stops mid-conversation.
	if c.after(time.Minute, "chrome.exe", "Granola.exe") {
		t.Fatal("reminded while Granola was recording")
	}
	if c.state.Talking() {
		t.Fatal("still tracking a conversation Granola is recording")
	}
	// It stops again, and the conversation is treated as new.
	if c.after(time.Second, "chrome.exe") {
		t.Fatal("reminded without waiting out the new debounce")
	}
	if !c.after(DefaultDebounce, "chrome.exe") {
		t.Fatal("no reminder after Granola stopped recording")
	}
}

func TestIgnoredHoldersAreNotAConversation(t *testing.T) {
	c := newClock(DefaultConfig())

	// Broadcast opens the physical microphone alongside whoever took its
	// virtual one, and the sound settings page captures to draw a level bar.
	for i := 0; i < 5; i++ {
		if c.after(time.Minute, "NVIDIA Broadcast.exe", "windows.immersivecontrolpanel_cw5n1h2txyewy") {
			t.Fatal("ignored holders counted as a conversation")
		}
	}
	// Gaming is not a meeting.
	for i := 0; i < 5; i++ {
		if c.after(time.Minute, "Discord.exe") {
			t.Fatal("Discord counted as a conversation")
		}
	}
	// A real client alongside Broadcast still counts.
	c.after(0, "NVIDIA Broadcast.exe", "chrome.exe")
	if !c.after(DefaultDebounce, "NVIDIA Broadcast.exe", "chrome.exe") {
		t.Fatal("a real client next to Broadcast did not count")
	}
}

func TestEndingAConversationResetsTheCounter(t *testing.T) {
	c := newClock(DefaultConfig())

	c.after(0, "chrome.exe")
	if !c.after(DefaultDebounce, "chrome.exe") {
		t.Fatal("no first reminder")
	}
	if c.after(time.Second) {
		t.Fatal("reminded after the call ended")
	}
	if got := c.state.Sent(); got != 0 {
		t.Fatalf("counter left at %d after the call ended, want 0", got)
	}
	c.after(0, "Zoom.exe")
	if !c.after(DefaultDebounce, "Zoom.exe") {
		t.Fatal("the next conversation got no reminder")
	}
}

func TestPauseKeepsTheConversation(t *testing.T) {
	c := newClock(DefaultConfig())
	c.state.SetPaused(true)

	c.after(0, "chrome.exe")
	if c.after(DefaultDebounce*2, "chrome.exe") {
		t.Fatal("reminded while paused")
	}
	if !c.state.Talking() {
		t.Fatal("pausing lost the conversation")
	}
	// Unpausing mid-call does not restart the debounce: the conversation has
	// been under way the whole time.
	c.state.SetPaused(false)
	if !c.after(time.Second, "chrome.exe") {
		t.Fatal("no reminder after unpausing mid-conversation")
	}
}
