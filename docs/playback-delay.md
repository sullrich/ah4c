# PLAYBACK_DELAY: holding a tune

How the hold works, why each piece is shaped the way it is, and what was tried
and thrown away. Written 2026-08-27, the night a ninety second hold finally
landed at the live edge.

Say what was measured and what was assumed. Both appear below and they are
labeled.

---

## What it is for

ah4c presents itself to Channels DVR as an M3U tuner. A tune arrives at
`/play/tuner/<channel>`, ah4c runs adb scripts to drive the DirecTV app on an
Android box, and captures that box's HDMI through a LinkPi encoder.

The scripts take a couple of seconds, but the *app* takes far longer to actually
start playing the channel — during which the encoder is faithfully capturing a
loading screen. Without a hold, that is what the DVR records.

`PLAYBACK_DELAY` holds the stream back so what reaches Channels starts with
actual program. In the user's words: **it cleans up the tuning process.**

This is live TV. The client is MPV. There is no recording being made in the
usual sense — but everything the DVR receives it keeps, and the viewer can
scrub through it, which is why what goes into the wait matters so much.

**The one sentence this whole document is about: NULL packets carry no
timestamps.** Everything below follows from that.

## The budget that is not negotiable

**The DVR gives up 30 seconds after the request.** That is the whole budget and
it belongs to the DVR, not to us.

A ninety second hold does not violate it, because the response headers go out
immediately and the body starts flowing at once — it is the *body* that carries
filler rather than program. The DVR's clock is satisfied by bytes arriving, not
by those bytes being interesting.

What actually bounds a hold is how long the DVR is left starving. See
[The keepalive diet](#the-keepalive-diet).

---

## The shape of a held tune

```
request
  │
  ├─ headers out immediately ─────────────────► DVR's 30s clock is satisfied
  │
  ├─ [earlyReader]  one NULL packet per read, ~1 KB total
  │                 covers the 2–4s the adb scripts take
  │
  ├─ [lateEncoder]  the hold proper
  │     │
  │     ├─ drainEarly()  ── opens the encoder AT TUNE START and reads it
  │     │                   for the whole wait, through a timed gate that
  │     │                   throws everything away
  │     │                     │
  │     │                     ├─ stallTolerantReader (queue, NULL fill)
  │     │                     ├─ maybeWrapCaptions   (engine warms up here)
  │     │                     └─ gateReader (timed)  (discards until the mark)
  │     │
  │     └─ Read() ── serves NULL packets on the keepalive diet until the mark
  │
  ├─ the mark ── gate arms, takes the first live keyframe, posts the hand-off
  │
  ├─ takeHandoff() ── strips NULLs from the gate's first release,
  │                   prepends 500ms of black,
  │                   swaps filler for program
  │
  └─ program, on the encoder's own connection, which has been up the whole time
```

### Why the encoder opens at tune start, not at the mark

Every part of this program that works keeps the encoder open and reads it for
the whole time the DVR is waiting. Playback detection does. The stall-tolerant
reader does. Only this hold used to leave the encoder shut and open it cold at
the end, and it was the only one with a ceiling.

Opening early buys three things:

1. The connection the program arrives on has been up and flowing the entire
   time, so the hand-off is a swap rather than a cold start.
2. The gate can arm on a **live** keyframe instead of the first keyframe of a
   fresh connection.
3. The caption engine gets its first frame at the tune and warms up while
   nothing is watching. Before this, Vulkan enumeration, two dlopens and device
   registration all happened in the same second the program started — the whole
   engine booting inside the seam.

### The keepalive diet

`holdRate` decides the filler's pace. Every byte in the wait is a byte the DVR
stores ahead of the show, so the diet is as thin as it can be without the DVR
concluding the stream has died.

| window | rate |
|---|---|
| first `nullDetect` (6s) | volume, so the DVR decides the body is a stream |
| after that | one packet per `nullIdle` (500ms) — about 3 kbit/s |
| every `nullBeat` (5s) | volume returns for `nullBeatFor` (1s) |

**Measured:** a DVR sits through roughly twenty seconds of the thin trickle
before deciding the stream has died. The heartbeat keeps the thin stretch to
four seconds however long the hold is, so hold length stops mattering.

`nullBeat` is why holds longer than 45 seconds became possible at all — before
it, a sixty second hold left the DVR in the trickle for thirty-six seconds and
it tuned again part way through.

### Going quiet at the ends

`quietBeforeMark` (1s) before the mark, the filler stops entirely, and it stays
quiet through the keyframe hunt (bounded by `keyframeQuiet`, 3s).

Filler sent in the last seconds before the program is what ends up *directly in
front of the picture*, and NULL packets carry no frames — a playhead cannot
cross them and fast forward has nothing to land on. That is what the user
described for hours as "two or three seconds ahead of me that I cannot fast
forward through".

The quiet is timed **from the mark**, not from when the quiet began. The first
version timed it from entry, so it expired a second before the mark and started
filling again — putting NULL packets back exactly where they were being removed
from. The log said so: `no keyframe within 3s of the mark`, printed one second
*before* the mark arrived.

---

## The answer: half a second of black at the seam

This is the thing that worked, after a night in which nothing else did.

`takeHandoff` strips any NULL packets out of the gate's first release and puts
**500 ms of black video** immediately in front of the program, with nothing
between the two.

```go
const blackSeamFor = 500 * time.Millisecond
```

The clip is generated once at startup, before the listener binds (rule 10 —
nothing can be tuning yet), with `-g 15` so there is a keyframe twice a second
for a scrubber to land on. About 12 KB. A container that cannot make it hands
over exactly as it did before black existed.

**Why it works.** NULL packets carry no timestamps. That is the whole thing, and
it predicts every result of this investigation:

- Ninety seconds of NULL packets carry no clock and no frames, so a player
  arriving at the end of them has no time base and a playhead has nothing to
  cross. Fast forward finds no keyframe to land on and MPV pauses instead of
  moving. That is what the user described, accurately, for hours: *"there are
  null packets ahead of the playhead and I can't fast forward through them."*
- Half a second of real video immediately before the program carries
  timestamps, so the player anchors and then follows video with video.
- **A pre-roll never had this problem at all**, because a pre-roll is real video
  carrying real timestamps for the entire wait. It is doing the same job as the
  seam black, continuously, which is why `blackStartup` used to skip itself
  when a pre-roll was present — it no longer does; see the seam section.

The effect of the half second is measured. That timestamps specifically are the
mechanism is the best explanation available and it is the one that accounts for
all three cases.

**What the sizes taught us**, all measured on the user's screen:

| at the seam | result |
|---|---|
| one TS packet (188 B) | nothing displayed — cannot hold a coded picture |
| one frame (~8 KB, 33 ms) | *"I didn't see it"* — no change |
| **500 ms (~12 KB)** | **live edge, timeline correct, fast forward works** |

The jump from 33 ms to 500 ms is the whole difference. A frame is not a
crossing; half a second is.

### The black's codec: ENCODER_CODEC

The black in front of the program must be the encoder's own codec. A player will
not switch its video decoder at the hand-off, so a black in the wrong codec
freezes the picture on itself instead of crossing to the program. `ENCODER_CODEC`
is `h264` (the default) or `h265`. The H.264 black is generated at startup; the
H.265 black is a half-second clip embedded in the binary, which
`ENCODER_CODEC=h265` selects.

The pre-roll follows the same rule: it is the first coded video the player sees,
so on an H.265 encoder it must itself be H.265. An H.265 pre-roll is copied; an
H.264 clip, other video, or still image is encoded with the Trixie image's
`libx265` during startup. If preparation fails, the hold shows the built-in H.265
black instead. Nothing can bridge unlike codecs at the hand-off: a NULL gap, a
black seam, and a stream-type relabel were all tried and all froze.

### PLAYBACK_DELAY pays for its own black

The black is stream the DVR keeps, so it is part of the wait rather than added
to it:

```
PLAYBACK_DELAY=1m30s  →  89.5s of NULL packets + 500ms of black = 90s
```

Without this, ninety seconds asked for is a ninety-and-a-half second hold, which
is a setting that quietly means something else. **Measured:** `hold 1m30.038s`
against ninety asked for, where every run before the subtraction came in at
`1m31.3s`.

A delay at or under the half second floors at a millisecond of wait rather than
going to zero and never holding at all. This arithmetic has a test, because two
rules in this file have been silently wrong before and both were conditions
buried in a loop.

---

## Things that were tried and are wrong

Each of these was coherent, was built, and failed. They are here so they are not
rebuilt.

### Black at the head of the wait as well

Tried the same 500 ms at the front of the wait, so the NULL packets at the head
of the recording would be bracketed too. **Watched, and it was worse.**

The reason is in how it was built: one clip used at both ends puts PTS 0→0.5s at
the head, ninety seconds of clockless NULL packets, then PTS 0→0.5s **again** at
the seam. Time runs backwards inside the file and a scrubber cannot cross it.
Seam-only worked, seam-and-head did not, one change between them.

If those leading NULLs are ever worth answering, the answer is **not** a second
copy of this clip — it is a separate clip whose timestamps start after the
seam's, or an `-output_ts_offset` on one of them.

### Filling the whole wait with black

Works, and is forbidden. Ninety seconds of black is ninety seconds of black in
the recording, and about sixty megabytes of it.

### Tables in the filler

Sending the encoder's real PAT and PMT alongside the NULL packets, so the wait
is padding inside a declared program rather than unidentifiable stream.

An earlier version of this *invented* tables naming PIDs nothing was ever sent
on; the DVR locked onto them and never played at all. The later version used the
encoder's own tables read off the draining connection — and never once executed,
because the tables come from `l.gate`, which `drainEarly` sets one line past the
deadlock described below. It was deleted rather than finally let loose.

### A synthesized clock through the wait (`holdclock.go`)

Got a ninety second hold to millisecond-close on every instrument and the
picture stayed behind. Convicted by its own log and deleted.

### Breaking the encoder connection after the hand-off

Twenty seconds, thirty, forty, four repeated sheds — every timing was tried and
every one came back behind. **Playback detection, the hold that works, never
breaks the connection at all.** What is left after the hand-off behaves like a
player-side start-up buffer: it grows for a few seconds and then holds, which is
a player taking some stream before it begins and then playing at 1× forever. A
break does not shed that; it makes the player do it again.

Note the tension with rule 11 in `CLAUDE.md`, which records the encoder's body
timeout as load-bearing on a *different* path. Both findings stand; they are
about different connections.

### Raising the filler's volume

The diet was tried at 0.5 Mbit/s and 5 Mbit/s. Neither moved anything, and both
put megabytes of unplayable stream in front of the viewer.

### A sparse keepalive

One packet every ten seconds after the detection window, on the theory that
fewer NULL packets means less padding in front of the picture. **The tune timed
out.** This had been tried before and failed before.

---

## The bug that faked four bugs

Worth its own section, because it cost most of a night and because the shape of
it recurs.

In `lateEncoder.Read`, above the filler's select:

```go
l.mu.Lock()            // ← no Unlock on any path out
wait := time.NewTimer(d)
select {
case r := <-l.handoff:
    return l.takeHandoff(p, r)   // takes l.mu
case <-wait.C:
}
return l.emitNulls(p, burst)     // takes l.mu, twice
```

A leftover from the version that played a black clip through the wait and read
its state there. `sync.Mutex` is not reentrant, so **the first filler read of
every held tune deadlocked against itself** and died still holding the lock.

`drainEarly`'s next `l.mu.Lock()` is the line right after it builds the stall
reader. So from that instant the reader's producer ran with nothing consuming
it, and the visible symptoms were:

- **Tunes failed** — no hand-off was ever posted.
- **Captions never started** — no packet reached the gate, and
  `captionStream.Read` only starts its pump on a read that returns bytes.
- **The tuner was never released** — so the next tune found it active and went
  to the next box, unconditionally, with no way to get the lock back.
- **An endless drop flood** — the queue filled and threw itself away on every
  push: 155,000 chunks logged on a tune that had ended fifteen minutes earlier.

Four unrelated-looking failures, one shared choke point.

**What found it was the log line that never appeared:** `encoder open and
draining for the wait`. Everything downstream of that line was broken and
everything downstream of it was what had been changed. A missing line is
evidence; a reader that has stopped produces no errors because nothing flows
through it.

**The cheap check**, since the compiler and `go vet` both say nothing:

```sh
awk '/^func /{fn=$0;l=0;u=0} /l\.mu\.Lock\(\)/{l++} /l\.mu\.Unlock\(\)/{u++} \
     /^}/{if(fn!=""&&l!=u) print l,u,fn; fn=""}' playbackdelay.go
```

An early-return-with-Unlock pattern shows as 1 lock / 2 unlock and is fine. 1
lock / 0 unlock is the bug.

---

## Numbers that are measured

Everything here came off an instrument or the user's log, not an argument.

- After the hand-off: **0 bytes of NULL packets** against 100+ MB of program, at
  a steady 6.9 Mbit/s, pace within ±20 ms.
- Writes to the DVR block **26–31 ms per 15 s** — 0.2%. Channels takes every
  byte instantly and is not back-pressuring.
- The stall queue stands at **0–4 of 16**, deepest 5–6 across a whole wait.
  `queueDepth` of 64 was 2 MB of latency hoard; 4 was an over-correction that
  punched holes on every encoder burst; 16 is right.
- A ninety second hold sends about **49 KB** of filler in total, plus ~1 KB
  covering the scripts. The scripts filler used to be 160 KB — 95% of every NULL
  packet in the stream — because it filled the caller's whole buffer per read.
- Keyframe hunt after the mark: **0.4–1.4 s** typically, occasionally 5 s.
- `hold 1m30.038s` for a ninety second setting.

## Numbers that are assumed

- That the black works *because* it gives the player a decodable picture and a
  time base to carry across the seam. The effect is measured; the mechanism is
  the best explanation available.
- That the head-black regression is caused by the duplicated PTS range.
  Consistent with everything seen, not independently confirmed.

---

## Where the code lives

Two files, deliberately.

- **`playbackdelay.go`** — the hold: `lateEncoder`, `drainEarly`, `takeHandoff`,
  the diet (`holdRate`), the black (`blackStartup`), the 1xx hint hold, the
  capped listener (`serveLive`).
- **`preroll.go`** — the pre-roll feature and `earlyReader`, the filler that
  covers the adb scripts.

`main.go` gets wiring only: the `gateReader` and `stallTolerantReader` it already
owned, a call to `newLateEncoder`, and `serveLive` in place of `r.Run`.

## Rules this cost

These are in `CLAUDE.md` and were each paid for.

- **A log line beats an argument.** Four coherent, tested, wrong things shipped
  in one evening. What found every real cause was a measurement or a line of the
  user's log.
- **Change one thing between comparisons.** Forty-five seconds was tested on one
  build and sixty on another, and hours went into explaining a difference that
  may not have existed.
- **A thing that looks like a bug may be the thing that works.** Check what goes
  quiet before deleting something that looks wrong.
- **Listen to the literal words.** "There are null packets ahead of the playhead
  and I can't fast forward through them" was a precise description of the fault
  for hours before it was taken literally.
