package calls

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	live  = true
	muted = false
)

const poll = 2 * time.Second

// clock advances a fake now and collects the calls the tracker finishes.
type clock struct {
	t     time.Time
	tr    *Tracker
	calls []*Call
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC), tr: New(DefaultConfig())}
}

// hold polls every two seconds for d with the same reading.
func (c *clock) hold(d time.Duration, micLive bool, holders ...string) {
	for end := c.t.Add(d); c.t.Before(end); {
		c.t = c.t.Add(poll)
		if done := c.tr.Update(c.t, micLive, holders); done != nil {
			c.calls = append(c.calls, done)
		}
	}
}

// jump lets d pass with no polls at all, as in sleep, then polls once.
func (c *clock) jump(d time.Duration, micLive bool, holders ...string) {
	c.t = c.t.Add(d)
	if done := c.tr.Update(c.t, micLive, holders); done != nil {
		c.calls = append(c.calls, done)
	}
}

func near(got, want time.Duration) bool {
	diff := got - want
	return diff > -2*poll && diff < 2*poll
}

func TestSilentCallIsStillACall(t *testing.T) {
	c := newClock()
	c.hold(5*time.Minute, muted, "NVIDIA Broadcast.exe")
	c.hold(90*time.Minute, muted, "Discord.exe", "NVIDIA Broadcast.exe")
	c.hold(5*time.Minute, muted, "NVIDIA Broadcast.exe")

	if len(c.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(c.calls))
	}
	call := c.calls[0]
	if !near(call.Held, 90*time.Minute) {
		t.Errorf("held %s, want 90m", call.Held)
	}
	if call.Live != 0 {
		t.Errorf("live %s with the microphone muted throughout", call.Live)
	}
	if _, ok := call.Apps["NVIDIA Broadcast.exe"]; ok {
		t.Error("NVIDIA Broadcast counted as a call application")
	}
}

func TestLiveTimeIsTheUnmutedPart(t *testing.T) {
	c := newClock()
	c.hold(20*time.Minute, muted, "chrome.exe", "Granola.exe")
	c.hold(10*time.Minute, live, "chrome.exe", "Granola.exe")
	c.hold(30*time.Minute, muted, "chrome.exe", "Granola.exe")
	c.hold(2*time.Minute, muted)

	if len(c.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(c.calls))
	}
	call := c.calls[0]
	if !near(call.Held, time.Hour) || !near(call.Live, 10*time.Minute) {
		t.Errorf("held %s live %s, want 1h and 10m", call.Held, call.Live)
	}
	if !near(call.Apps["Granola.exe"], time.Hour) || !near(call.Apps["chrome.exe"], time.Hour) {
		t.Errorf("apps %v, want both near 1h", call.Apps)
	}
	if got := call.AppNames(); len(got) != 2 {
		t.Errorf("app names %v", got)
	}
}

func TestShortDropStaysOneCall(t *testing.T) {
	c := newClock()
	c.hold(10*time.Minute, muted, "Discord.exe")
	c.hold(30*time.Second, muted)
	c.hold(10*time.Minute, muted, "chrome.exe")
	c.hold(2*time.Minute, muted)

	if len(c.calls) != 1 {
		t.Fatalf("got %d calls, want 1 across a 30s drop", len(c.calls))
	}
	if !near(c.calls[0].Held, 20*time.Minute) {
		t.Errorf("held %s, want 20m without the drop", c.calls[0].Held)
	}
}

func TestEndIsLastHoldNotEndOfGap(t *testing.T) {
	c := newClock()
	c.hold(10*time.Minute, muted, "Discord.exe")
	lastHeld := c.t
	c.hold(3*time.Minute, muted)

	if len(c.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(c.calls))
	}
	if !c.calls[0].End.Equal(lastHeld) {
		t.Errorf("end %s, want %s", c.calls[0].End, lastHeld)
	}
}

func TestBlipIsDropped(t *testing.T) {
	c := newClock()
	c.hold(10*time.Second, live, "Telegram.exe")
	c.hold(3*time.Minute, muted)
	if len(c.calls) != 0 {
		t.Fatalf("a 10s voice message became a call: %+v", c.calls[0])
	}
}

func TestSleepEndsTheCall(t *testing.T) {
	c := newClock()
	c.hold(10*time.Minute, muted, "chrome.exe")
	beforeSleep := c.t
	c.jump(8*time.Hour, muted, "chrome.exe")

	if len(c.calls) != 1 {
		t.Fatalf("got %d calls, want the one before sleep", len(c.calls))
	}
	if !c.calls[0].End.Equal(beforeSleep) || !near(c.calls[0].Held, 10*time.Minute) {
		t.Errorf("call %+v absorbed the sleep", c.calls[0])
	}
	if open := c.tr.Open(); open == nil || !open.Start.Equal(c.t) {
		t.Error("the hold after waking did not start a new call")
	}
}

func TestFinishFlushesOpenCall(t *testing.T) {
	c := newClock()
	c.hold(5*time.Minute, live, "Discord.exe")
	if got := c.tr.Finish(); got == nil || !near(got.Held, 5*time.Minute) {
		t.Fatalf("finish returned %+v", got)
	}
	if c.tr.Open() != nil {
		t.Error("call still open after finish")
	}
}

func TestApplicationNamesIgnoreCase(t *testing.T) {
	c := newClock()
	c.hold(5*time.Minute, muted, "CHROME.EXE")
	if open := c.tr.Open(); open == nil || open.Apps["chrome.exe"] == 0 {
		t.Fatalf("chrome in capitals not counted: %+v", open)
	}
}

func TestLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "calls.jsonl")
	start := time.Date(2026, 11, 13, 17, 40, 0, 0, time.FixedZone("", 5*3600))
	want := &Call{
		Start: start,
		End:   start.Add(58 * time.Minute),
		Held:  58 * time.Minute,
		Live:  12 * time.Minute,
		Apps:  map[string]time.Duration{"Discord.exe": 58 * time.Minute, "Granola.exe": 57 * time.Minute},
	}
	for i := 0; i < 2; i++ {
		if err := Append(path, want); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d calls, want 2", len(got))
	}
	g := got[1]
	if !g.Start.Equal(want.Start) || g.Held != want.Held || g.Live != want.Live || g.Apps["Granola.exe"] != 57*time.Minute {
		t.Errorf("round trip gave %+v", g)
	}
}

func TestLogLineIsReadableWithoutGo(t *testing.T) {
	c := Call{
		Start: time.Date(2026, 11, 13, 17, 40, 0, 0, time.UTC),
		End:   time.Date(2026, 11, 13, 18, 38, 0, 0, time.UTC),
		Held:  58*time.Minute + 400*time.Millisecond,
		Live:  12 * time.Minute,
		Apps:  map[string]time.Duration{"Discord.exe": 58 * time.Minute},
	}
	b, err := c.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"seconds":3480`, `"mic_live_seconds":720`, `"Discord.exe":3480`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s missing %s", b, want)
		}
	}
}

func TestOpenCallSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "open.json")
	c := newClock()
	c.hold(5*time.Minute, muted, "chrome.exe")
	if err := SaveOpen(path, c.tr.Open()); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOpen(path)
	if err != nil || got == nil || !near(got.Held, 5*time.Minute) {
		t.Fatalf("load gave %+v, %v", got, err)
	}
	if err := SaveOpen(path, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadOpen(path); err != nil || got != nil {
		t.Fatalf("after clearing, load gave %+v, %v", got, err)
	}
}

func TestMissingLogIsEmpty(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "none.jsonl"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}
