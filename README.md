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

## The open question

The meter only reports while the device's audio engine is running, which means
while some application is capturing. With nothing capturing it reads a flat
zero — so the level cannot, on its own, tell that a conversation started.

That leaves two shapes for the trigger, and they differ in what they cost:

- **The consent store alone.** Fire when an application other than Granola
  holds the microphone. Free, invisible, and it fires when the call starts
  rather than after the first sentences. Blind to a meeting in the same room,
  where nothing captures at all.
- **Our own capture stream.** Hold a shared-mode stream open so the meter
  always answers, and detect speech directly. Covers the room, at the price of
  the Windows microphone indicator burning permanently and this tool sitting in
  the list of applications listening to the microphone.
