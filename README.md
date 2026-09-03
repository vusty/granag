# granag

Short for granola-nag.

A Windows tray tool that reminds me to start recording in Granola when a
conversation is under way and Granola is not capturing. No UI beyond the tray
icon and the notification.

I mute the microphone physically, with the tap sensor on a HyperX QuadCast S,
so an unmuted microphone means I am about to talk. What I keep forgetting is
the record button, and a meeting with no transcript is a meeting I have to
remember by hand.

## Use

    granag run              the reminder, with the tray icon; -no-tray to stay in the terminal
    granag autostart on     start at logon, through the Run key
    granag toast            fire one notification, to prove toasts reach the screen
    granag list             capture devices with their level, and every microphone session
    granag watch            one line a second: level, whether it counts as speech, who holds the mic

Idle it sits at about 14 MB and no measurable CPU: the work is a dozen registry
reads every two seconds.

The tray icon is green while reminders are on and grey while paused, and its
menu opens Granola, pauses, pauses for an hour, or quits. The hourly pause
exists because a plain toggle gets left off.

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

## How it decides

The trigger is the microphone being live:

    the level says the microphone is unmuted
      and Granola is not holding it
      and Discord is not holding it
      and reminders are not paused
      and that has held for 10 seconds
    -> remind

The microphone is muted in hardware except when a conversation is about to
happen, so an unmuted microphone is already the decision to talk - which is why
the debounce is ten seconds rather than a wait for someone to speak, and why no
call application need be involved at all. A meeting in the room counts the same
as a call.

Discord calls it off because gaming is talking with the microphone live and
wanting no transcript. The tray pause covers everything else.

## What that costs, and why the level needs a stream

`IAudioMeterInformation` reports nothing while no application captures: with
nothing capturing it reads a flat floor even as you talk into an unmuted
microphone. So the tool holds a shared-mode capture stream of its own, which
makes it the application the meter needs. Shared mode takes nothing from
anyone - calls, Granola and this stream coexist - but Windows lights the
microphone indicator for as long as the tool runs and lists it among the
applications listening. Since the microphone is muted in hardware anyway, the
indicator ends up telling the truth: lit while the device is live.

Measured on a QuadCast S with the stream held, which is what the threshold is
built on:

| state | level |
|---|---|
| muted with the tap sensor | 0.0002 |
| unmuted, silent room | 0.019 - 0.054 |
| unmuted, speaking | 0.03 - 1.0 |

The default threshold of 0.002 sits an order of magnitude above the muted floor
and an order below a quiet room, so neither a quieter room nor a lower gain knob
moves it into doubt. The tap sensor itself is invisible to Windows - the mute
flag on the endpoint never moves - so the level is the only way to see it.

Read the physical device, not the NVIDIA Broadcast virtual one: Broadcast's
noise removal drives a silent room down to 0.0008, where muted and unmuted stop
being distinguishable.

## The input volume

Windows drops the capture endpoints' input volume to zero across some restarts.
A zero-volume microphone is silent to the other side while everything still
looks connected - the failure you learn about a minute into a call. The gain
that matters is the knob on the microphone, so this slider has no business
anywhere but maximum, and `run` puts every capture device back there on start
and every thirty seconds after. `-keep-volume=false` turns that off.

It gets there by stepping the volume up rather than by setting a value: the
setters take a float by value, and on amd64 a float argument travels in an XMM
register, which Go's syscall bridge cannot fill.

## What the consent store is still for

`CapabilityAccessManager` records one session per application, with
`LastUsedTimeStop` zero while capture is open. It answers two questions the
level cannot: whether Granola is recording rather than merely running - checked
against a real 31-minute call and against Granola sitting open and idle, which
holds no microphone at all - and whether Discord has the microphone, which calls
the reminder off.

## Not done yet

A Start Menu shortcut carrying an AppUserModelID of our own. That is what would
put the tool's name on the notification instead of "Windows PowerShell", and it
is the same prerequisite for buttons inside the toast. The tray menu already
does what those buttons would, so this is cosmetics until proven otherwise.
