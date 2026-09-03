# granag

Short for granola-nag.

A Windows tray tool that reminds me to start recording in Granola when a
conversation is under way and Granola is not capturing. No UI beyond the tray
icon and the notification.

I mute the microphone physically, with the tap sensor on a HyperX QuadCast S,
so an unmuted microphone means I am about to talk. What I keep forgetting is
the record button, and a meeting with no transcript is a meeting I have to
remember by hand.

## State

The two probes the detector is built on are written and verified on the real
machine. The notification, the tray icon and the state machine are not.

    granag list      capture devices with their level, and every microphone session
    granag watch     one line a second: level, whether it counts as speech, who holds the mic

Build from WSL, run on Windows:

    GOOS=windows GOARCH=amd64 go build -o granag.exe .

## What the probes read

**Level** — `IAudioMeterInformation` on the capture endpoint, no capture stream
of our own. Read the physical QuadCast rather than the NVIDIA Broadcast virtual
device: Broadcast's noise removal drives silence to an exact zero, so on it a
muted microphone and a quiet one look the same.

A muted QuadCast S is not a zero either. Its tap sensor never reaches Windows —
the mute flag on the endpoint does not move and the device keeps streaming — so
muted silence reads a small constant floor around 0.0002 while speech runs 0.03
to 0.14. Speech is a threshold, not a comparison against zero.

**Who holds the microphone** — `CapabilityAccessManager`'s consent store in the
registry, one subkey per application, `LastUsedTimeStop` zero while capture is
open. This is the bookkeeping behind the microphone icon in the tray, and it
answers "is Granola recording, or merely running" without any Granola API. It
also tells a meeting from a game: if Discord holds the microphone, nobody wants
a transcript.

## What the trigger turned out to be

The level is not part of it. `IAudioMeterInformation` only reports while the
device's audio engine is running, which means while some application is already
capturing — with nothing capturing it reads a flat zero even as you talk into an
unmuted microphone, measured on 2026-09-03. And the small floor it returns
otherwise, around 0.0002, is what digital silence reads: it looks the same
whether the engine is asleep or the microphone is muted in hardware, so it
cannot tell those apart either.

So by the time the level could confirm a conversation, the consent store has
already said so. The trigger is the consent store alone: an application other
than Granola holds the microphone. That fires when the call starts rather than
after the first sentences, costs a dozen registry reads every couple of seconds,
and never lights the microphone indicator.

NVIDIA Broadcast has to be ignored for this to work. It opens the physical
microphone on demand, when something takes its virtual one, so it shows up
*alongside* the real client — counted naively, every call would look like two,
and Broadcast sitting idle would look like a call.

The level survives for two things: telling a broken audio path from a quiet room
(a long, perfect zero while a call is under way means the pipeline is dead, not
that nobody is talking), and a possible in-room mode, where the tool would hold
its own capture stream and so be the capturer the meter needs. That mode would
light the microphone indicator permanently, which is why it is not the default.

## Not done yet

The tray icon and its pause toggle, the Start Menu shortcut that would give the
toast its own name and make buttons inside it possible, and autostart.
