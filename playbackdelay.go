package main

// PLAYBACK_DELAY: hold a tune, handing the program over when the delay is up
// so the DVR gets a session that starts when the viewer does. Everything the
// feature is lives here: the hold itself, the 1xx window that fronts it, the
// discontinuity marker that ends it, the half second of black that goes in at
// the seam, and the cap on the DVR socket's send buffer.
//
// docs/playback-delay.md is the long version — how it works, what was measured,
// and what was tried and thrown away.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// holdDelay is the playback delay as parsed at startup; zero when unset. It is
// the length of the NULL-packet wait, which is what the user asked for less the
// black that goes in front of the picture — holdAsked is what they actually
// set, kept for the log so the two can be told apart.
var (
	holdDelay time.Duration
	holdAsked time.Duration
)

// holdMost is as long as a tune is held, whatever PLAYBACK_DELAY asks for.
// Forty-five seconds is the longest hold watched land at the live edge; sixty
// and ninety come in behind the guide and stay there. Why is not yet settled
// — see holdRate — so this is lifted to ten minutes only to test longer holds,
// and is not a claim that they work. It is a guard against a typo, not a
// measured limit. Put it back to forty-five if a real answer does not arrive.
const holdMost = 10 * time.Minute

const (
	// The wait's byte diet: volume through the DVR's detection window, then
	// a keepalive. Every byte here is one the DVR stores ahead of the show.
	// One packet every two hundred milliseconds, not two every hundred. The
	// old rate put a hundred and sixty-eight kilobytes of NULL packets into a
	// ninety second hold, and every one of them lands in front of the picture
	// where a player cannot cross it. This is a quarter of that and still
	// constant — a packet on the wire five times a second, no gaps — which is
	// the thing that matters: a sparse keepalive with ten second holes timed a
	// tune out, and silence is what a DVR gives up on, not thinness.
	nullPace  = 200 * time.Millisecond
	nullBurst = 1 * tsPacketSize
	// Volume while the DVR decides the body is a stream, a keepalive after:
	// a trickle from the first byte starves it, and it gives up.
	nullDetect = 6 * time.Second
	nullIdle   = 500 * time.Millisecond
	// A DVR decides the body is a stream, and then keeps deciding. The
	// keepalive after nullDetect is three kilobits a second, and a DVR will
	// sit through about twenty seconds of that before concluding the stream
	// has died — which is why a forty-five second hold worked and a sixty
	// second one tuned again part way through. So the volume comes back for
	// nullBeatFor every nullBeat, and the thin stretch never runs longer than
	// four seconds however long the hold is. Four rather than eleven because
	// twenty-one seconds is one measurement on one box, and a DVR with less
	// patience than that one should hold too.
	nullBeat    = 5 * time.Second
	nullBeatFor = 1 * time.Second
	// flushBeforeArm is how far ahead of the gate's arming the stall queue is
	// emptied, so the gate hunts its keyframe through fresh bytes rather than
	// through whatever has been stored up behind it.
	flushBeforeArm = 300 * time.Millisecond
	// keyframeQuiet is how long the wait stays silent past the mark while the
	// gate hunts. A keyframe arrives within a GOP — measured at 0.1s to 2s on
	// this encoder — so three seconds means the hunt has gone wrong, and a
	// silent stream is worse than an unplayable one at that point.
	keyframeQuiet = 3 * time.Second
	// markWindow is how long the discontinuity marker keeps marking the first
	// packet of each new programme PID after the hand-off. Long enough for the
	// audio to arrive, short enough to touch nothing mid-programme.
	markWindow = 2 * time.Second
	// quietBeforeMark is how long before the mark the filler stops, so the
	// seconds directly in front of the picture are not NULL packets. Measured
	// against what is reported in front of the playhead: two to three seconds.
	// quietBeforeMark drains the filler out of the pipe before the programme
	// goes in behind it. NULL packets already written are still in flight in
	// the socket when the video follows them, so they arrive ahead of the
	// picture — which is what "it is ahead of the playhead, it goes in before
	// the video data does" means. Stopping briefly lets them clear.
	//
	// One second, not four: the send buffer is capped at 256 KB and drains in
	// about a third of a second at this bitrate, and this DVR needs constant
	// data — a sparse keepalive timed a tune out. Short enough to be nothing
	// like that starvation, long enough to empty the pipe.
	quietBeforeMark = time.Second
	// How long the encoder's clock must stop outrunning the wall, and the
	// most that may be spent or thrown away deciding.
	liveEdgeSettle = 250 * time.Millisecond
	liveEdgeBudget = 2 * time.Second
	liveEdgeMost   = 4 << 20
)

// maybeWrapNullFrameInsertion wraps body when NULL_FRAME_INSERTION is TRUE, so
// stalls are filled and the encoder at url is reconnected when it drops.
func maybeWrapNullFrameInsertion(body io.ReadCloser, url, label string) io.ReadCloser {
	if !strings.EqualFold(os.Getenv("NULL_FRAME_INSERTION"), "TRUE") {
		return body
	}
	return newStallTolerantReader(body, func() (io.ReadCloser, error) {
		r, e := http.Get(url)
		if e != nil {
			return nil, e
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			return nil, fmt.Errorf("status %s", r.Status)
		}
		return r.Body, nil
	}, label)
}

// liveEdge drops whatever the encoder had queued when it was opened, and
// returns the last PCR it read at the live edge (33-bit, 90 kHz base) so the
// wait's clock can be re-anchored to the encoder's true live rather than to a
// probe taken tens of seconds earlier.
func liveEdge(body io.ReadCloser, label string) (uint64, bool) {
	buf := make([]byte, 64*1024)
	var start, last uint64
	var have bool
	var dropped int64
	var ahead float64
	t0, settled := time.Now(), time.Now()
	for time.Since(t0) < liveEdgeBudget && dropped < liveEdgeMost {
		n, err := body.Read(buf)
		if err != nil {
			return last, have
		}
		dropped += int64(n)
		for i := 0; i+tsPacketSize <= n; i += tsPacketSize {
			p := buf[i : i+tsPacketSize]
			if p[0] != 0x47 || p[3]>>4&2 == 0 || p[4] < 7 || p[5]&0x10 == 0 {
				continue
			}
			last = uint64(p[6])<<25 | uint64(p[7])<<17 | uint64(p[8])<<9 | uint64(p[9])<<1 | uint64(p[10])>>7
			// The clock free-runs and wraps, so anything that is not a
			// forward step of a sane size starts the measurement over.
			if !have || last < start || last-start > 60*90000 {
				start, have, ahead = last, true, 0
				settled = time.Now()
			}
		}
		if have {
			if by := float64(last-start)/90000 - time.Since(t0).Seconds(); by > ahead+0.05 {
				ahead, settled = by, time.Now()
			}
		}
		if time.Since(settled) >= liveEdgeSettle {
			break
		}
	}
	logger("[HOLD] %s dropped %s the encoder had queued (%.1fs ahead)", label, byteCount(dropped), ahead)
	return last, have
}

// tuneHoldStartup parses the delay and prepares the pre-roll, before the listener binds.
func tuneHoldStartup() {
	holdDelay = 0
	if s := os.Getenv("PLAYBACK_DELAY"); strings.TrimSpace(s) != "" {
		d, err := parseHoldDuration(s)
		if err != nil {
			logger("[HOLD] PLAYBACK_DELAY %q %v; tunes are not being held", s, err)
		} else {
			holdDelay = d
			holdAsked = d
			if holdDelay > holdMost {
				logger("[HOLD] PLAYBACK_DELAY %s is longer than %s, which is as long as this build will hold a tune; holding for %s",
					holdWords(holdDelay), holdWords(holdMost), holdWords(holdMost))
				holdDelay = holdMost
			}
		}
	}
	prerollStartup()
	blackStartup()
	// The bookend's black is stream the DVR keeps, so it is part of the wait
	// rather than something added to it: a ninety second setting stays a
	// ninety second hold, of which one second is black. Taken off after
	// blackStartup, because a container that could not make the clip is not
	// paying for it.
	if holdDelay > 0 && len(blackPool) > 0 {
		if holdDelay > blackCosts {
			holdDelay -= blackCosts
		} else {
			logger("[HOLD] PLAYBACK_DELAY %s is shorter than the %s of black in front of the picture, so the wait itself is nothing and the tune is only the black",
				holdWords(holdAsked), blackWords(blackCosts))
			holdDelay = time.Millisecond
		}
	}
	detect := strings.EqualFold(os.Getenv("PLAYBACK_DETECTION"), "TRUE")
	if holdDelay > 0 && detect {
		logger("[HOLD] PLAYBACK_DELAY is set, so PLAYBACK_DETECTION does not run: the delay decides when the program starts")
	}
	switch {
	case holdDelay > 0 && prerollTS != "":
		logger("[HOLD] hold %v with the pre-roll", holdDelay)
	case holdDelay > 0:
		logger("[HOLD] hold %s: %s of NULL packets, then %s of black and the picture",
			holdWords(holdAsked), holdWords(holdDelay), blackWords(blackSeamFor))
	case detect && prerollTS != "":
		logger("[HOLD] pre-roll shows while playback detection holds a tune")
	case prerollTS != "":
		logger("[PREROLL] mounted, but nothing holds a tune: set PLAYBACK_DELAY or PLAYBACK_DETECTION to see it at tune time. It still covers stalls under NULL_FRAME_INSERTION")
	}
}

// holdWords says a hold's length the way a person reads it.
func holdWords(d time.Duration) string {
	d = d.Round(time.Second)
	m, sec := int(d/time.Minute), int((d%time.Minute)/time.Second)
	unit := func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	switch {
	case m > 0 && sec > 0:
		return unit(m, "minute") + " " + unit(sec, "second")
	case m > 0:
		return unit(m, "minute")
	default:
		return unit(sec, "second")
	}
}

// holdUnit reads spelled-out units: "1 hour", "90 seconds", "2 mins".
var holdUnit = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)

// parseHoldDuration reads the delay: bare seconds, or 45s / 1m30s / 1h.
func parseHoldDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("is negative")
		}
		return time.Duration(n * float64(time.Second)), nil
	}
	norm := holdUnit.ReplaceAllStringFunc(s, func(m string) string {
		parts := holdUnit.FindStringSubmatch(m)
		return parts[1] + strings.ToLower(parts[2][:1])
	})
	norm = strings.ReplaceAll(norm, " ", "")
	d, err := time.ParseDuration(norm)
	if err != nil {
		return 0, fmt.Errorf("is not a number of seconds or a duration like 45s, 2m or 1h")
	}
	if d < 0 {
		return 0, fmt.Errorf("is negative")
	}
	return d, nil
}

// lateEncoder is the hold as a source: filler, then an encoder opened late.
type lateEncoder struct {
	url     string
	label   string
	t0      time.Time
	until   time.Time
	preroll *prerollPlayer
	pend    []byte
	// tuner and name are what captions are wrapped with, applied to the
	// encoder's own stream rather than to the hold in front of it.
	tuner int
	name  string

	// handoff carries the gated encoder from the goroutine that has been
	// draining it through the wait to the read that is serving the DVR.
	handoff chan *handoffResult
	// refresh is the encoder's one reopen. Its clock is started at the
	// hand-off, not at the open: the encoder is now opened at tune start, and
	// a shed spent during the wait is a shed the viewer never gets.
	refresh *refreshSource
	// drain is the gated chain drainEarly is reading, kept so Close can shut
	// the encoder even when the hand-off has not happened yet.
	drain io.ReadCloser
	// gate is the drained gate, kept so the wait can send the encoder's own
	// tables with its filler. lastTables is when they last went out.
	// primer is real picture sent before the filler, so the player has a time
	// base to carry across the wait.
	primer []byte
	// black fills the wait with real frames; pendBlack is what is left of the
	// last chunk it handed over.
	black     *prerollPlayer
	pendBlack []byte
	// blackFrom is when black first went out, which is when its clock starts.
	blackFrom  time.Time
	gate       *gateReader
	lastTables time.Time
	// quietSaid keeps the "no keyframe" note to once a tune.
	quietSaid atomic.Bool

	mu     sync.Mutex
	body   io.ReadCloser
	closed bool
	nulls  int64
	// stall is the drained path's stall reader, kept so the hand-off and the
	// pace watcher can say how deep its queue stands instead of leaving the
	// depth to be inferred from drop events. nil on the pre-roll path, which
	// never drains. Written and read under mu.
	stall *stallTolerantReader
}

// handoffResult is the gated encoder, primed past its keyframe wait: first is
// the bytes the gate released (tables and the keyframe), body the rest.
type handoffResult struct {
	first []byte
	body  io.ReadCloser
}

// newLateEncoder holds from t0 until the delay is up, then opens url. early is
// a pre-roll already playing, which it takes over and shows for the wait.
// heldRecently remembers when a channel last finished its hold, so a DVR that
// reconnects does not get held all over again.
var heldRecently sync.Map // name -> time.Time of the last hand-off

// holdAgainAfter is how long a channel keeps the benefit of a hold it has
// already served. Inside this, a fresh request is the DVR reconnecting to
// something already tuned and playing, not a new tune-in to clean up.
// holdAgainAfter is how long a channel keeps the benefit of a hold it has
// already served. It covers a DVR reconnecting to a session it just had, and
// nothing more — at ten minutes it also swallowed a deliberate re-tune, which
// re-runs the scripts and tunes the box again, so the hold is exactly what
// that needs. Seconds, not minutes.
const holdAgainAfter = 20 * time.Second

func newLateEncoder(url, label string, t0 time.Time, early *prerollPlayer, tuner int, name string) *lateEncoder {
	until := t0.Add(holdDelay)
	// The hold exists to cover the box tuning in, and that happens once. But a
	// hold was started on every request, and the DVR reconnects — on a broken
	// pipe, on a client switching, on its own retry — so each reconnect wrote
	// another ninety seconds of NULL packets into the same recording. That is
	// what "there is real video and there is null, and it pauses when I fast
	// forward into the null zone" is: the timeline is programme, NULL zone,
	// programme, NULL zone, and fast forward runs into the next one and stops,
	// because NULL packets carry no frames for it to land on.
	//
	// So a channel that has already been held keeps the benefit of it. The
	// box is tuned and playing; there is nothing left to cover up.
	if v, ok := heldRecently.Load(name); ok {
		if since := time.Since(v.(time.Time)); since < holdAgainAfter {
			logger("[HOLD] %s %s was held %v ago and is already playing; starting the programme at once rather than holding again",
				label, name, since.Round(time.Second))
			until = t0
		}
	}
	l := &lateEncoder{url: url, label: label, t0: t0, until: until, preroll: early, tuner: tuner, name: name}
	if l.preroll != nil {
		l.preroll.adopted.Store(true)
	} else {
		l.preroll = startPreroll(label)
	}
	// Every part of this program that works keeps the encoder open and reads
	// it for the whole time the DVR is waiting: the stall-tolerant reader
	// fills gaps in a stream it is still reading, and playback detection opens
	// the encoder at tune start and lets the gate drain it until the app is
	// seen playing. Only this hold left the encoder shut and opened it cold at
	// the end, and it is the only one with a ceiling. So it opens here now.
	// A pre-roll hold is left exactly as it was — it is a different feature
	// and it works.
	if l.preroll == nil {
		// Black fills the wait with real frames, so the player has a time base
		// before any filler and can carry it across. Nil if the clip could not
		// be made, which leaves the wait on NULL packets as before.
		l.handoff = make(chan *handoffResult, 1)
		go l.drainEarly()
	}
	return l
}

// drainEarly opens the encoder at tune start and reads it for the whole wait.
// The gate discards everything it reads until the delay is up, so the DVR is
// never sent the wait's video — it gets NULL packets, exactly as before — and
// when the delay passes the gate releases a live keyframe. Nothing is opened
// cold at the end, and the connection the program arrives on has been up and
// flowing the entire time, which is the one thing this hold did not share with
// the features that work.
func (l *lateEncoder) drainEarly() {
	var body io.ReadCloser
	for {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return
		}
		resp, err := http.Get(l.url)
		if err == nil && resp.StatusCode != 200 {
			resp.Body.Close()
			err = fmt.Errorf("status %s", resp.Status)
		}
		if err == nil {
			// No reopen on this path. The break exists to make the DVR discard
			// what it has stored ahead (rule 11), and it earned that when a
			// two megabyte queue and an autotuned kernel buffer were holding
			// seconds of stale stream. Those are bounded now.
			//
			// What is left behaves like a player-side start-up buffer: it
			// grows for the first few seconds after the hand-off and then
			// holds, which is a player taking a few seconds of stream before
			// it begins and then playing at 1x for ever after. A break does
			// not shed that — it makes the player do it again. Which is why no
			// break timing ever worked: twenty seconds, thirty, forty, or four
			// of them. Playback detection, the hold that works, never breaks
			// the connection at all.
			l.refresh = nil
			// Captions wrap the encoder itself, inside the gate, rather than
			// the gate's output. Outside it they see their first frame at the
			// hand-off, and the log shows what that costs: Vulkan enumerated,
			// two backends dlopened and the devices registered, all in the
			// same second the program starts — the whole engine booting
			// between the release and the DVR's first frame. That is rule 13's
			// caption mistake and rule 6's "must be effectively free", and it
			// happened because until now there was no video to give them any
			// earlier. There is now: the encoder drains for the whole wait, so
			// the engine gets its first frame at the tune, warms up while
			// nothing is watching, and is already running at the hand-off.
			// The wait's own filler is never handed to them — this is the
			// encoder's stream, not the NULL packets in front of it.
			st := l.stallTolerant(resp.Body)
			l.mu.Lock()
			l.stall = st
			l.mu.Unlock()
			src := maybeWrapCaptions(st, l.tuner, l.name)
			// Timed: the gate arms itself when the delay is up and takes the
			// first keyframe after it. Until then it reads and throws away.
			// No discontinuity marker. Playback detection does not mark — main.go
			// builds its gate and hands the body straight on — and detection is
			// the hold that works. This one marked, and marking is what splits
			// the streams apart: the player is told the video's time base is
			// new and flushes it, and what it plays while the video
			// re-anchors is audio with no picture. Fast forward navigates by
			// video keyframes, finds none in that stretch, and MPV pauses
			// instead of moving — which is exactly what is seen.
			//
			// Marking every PID instead of only the video was tried first and
			// did not clear it: 0x64, 0x65 and 0x852 all marked, the same
			// pause on fast forward. So the marker goes, and the seam is what
			// detection's seam is — the encoder's own stream, told nothing.
			// CLAUDE.md already records "making a discontinuity marker
			// actually mark, on the one path that was working because it was
			// inert" as one of four fixes that were coherent and wrong; this
			// is that marker.
			g := newGateReader(src, nil, true, l.until, nil)
			l.mu.Lock()
			l.gate = g
			l.mu.Unlock()
			body = g
			// Empty the queue just before the gate arms. The gate takes the
			// first keyframe after the delay is up, and a queue that is full at
			// that moment means it takes one out of two megabytes of stored-up
			// stream: the program starts seconds behind and stays there,
			// however carefully everything after the hand-off is arranged. The
			// log showed exactly that — "start on keyframe, 514ms from the
			// mark", "threw away 2.0 MB", "program starts", all in the same
			// second. Emptying it there is too late; the frames the gate chose
			// have already gone out. So it happens first, and what the gate
			// arms against is the live edge.
			go func(until time.Time) {
				if d := time.Until(until.Add(-flushBeforeArm)); d > 0 {
					time.Sleep(d)
				}
				st.flush(l.label)
			}(l.until)
			// Close has to be able to reach this. Until now it could not: it
			// only ever closed l.body, which is not set until the hand-off is
			// claimed, so a tune whose client left during the wait leaked the
			// encoder connection and the producer goroutine behind it — and
			// that producer keeps pulling the encoder at full rate for ever,
			// with nobody reading. Watched happening: a tuner with nothing
			// playing threw away three thousand chunks and climbing.
			l.mu.Lock()
			l.drain = body
			closed := l.closed
			l.mu.Unlock()
			if closed {
				body.Close()
				return
			}
			logger("[HOLD] %s encoder open and draining for the wait", l.label)
			break
		}
		logger("[HOLD] %s encoder would not open for the wait (%v); trying again", l.label, err)
		time.Sleep(time.Second)
	}
	// Read until the gate releases. It returns nothing while it is discarding
	// and while it hunts the keyframe, so a zero-byte read is not the end.
	buf := make([]byte, 64*1024)
	// Two ways out besides the hand-off, because a drain that outlives its
	// tune does not merely waste work: it holds the encoder connection open,
	// and an encoder will not hand the same stream to a second reader, so the
	// next tune on that tuner cannot get video at all. That is a failed tune,
	// which is the worst thing this program can do. Watched happening: a tuner
	// with nothing playing threw away six thousand chunks and climbing while
	// the DVR reported "streaming to the tuner failed".
	//
	// So the loop checks whether the tune has been closed, rather than relying
	// only on Close reaching in and breaking the read — and it gives up on its
	// own a bounded time past the mark, whatever else has gone wrong.
	giveUp := l.until.Add(drainGiveUp)
	for {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			body.Close()
			return
		}
		if time.Now().After(giveUp) {
			logger("[HOLD] %s nothing took the hand-off within %v of the mark; letting the encoder go rather than holding it open", l.label, drainGiveUp)
			body.Close()
			return
		}
		n, err := body.Read(buf)
		if n > 0 {
			l.handoff <- &handoffResult{first: append([]byte(nil), buf[:n]...), body: body}
			return
		}
		if err != nil {
			logger("[HOLD] %s the drained encoder ended before the hand-off: %v", l.label, err)
			body.Close()
			l.handoff <- nil
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dietFrom is when this tune's body opened, which is when the DVR starts
// deciding whether it has a stream: after the stretch held on 1xx when that
// is in play, and at the request itself when it is not.
func (l *lateEncoder) dietFrom() time.Time {
	if prerollTS == "" && hintsWork.Load() {
		return l.t0.Add(hintCeiling)
	}
	return l.t0
}

func (l *lateEncoder) Read(p []byte) (int, error) {
	l.mu.Lock()
	body, closed := l.body, l.closed
	l.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	// Pending bytes first, then the body: the clock hand-off leaves the gate's
	// first release (tables and the keyframe) in pend and sets body at once, so
	// draining pend before reading body keeps a large first chunk whole.
	if len(l.pend) > 0 {
		n := copy(p, l.pend)
		l.pend = l.pend[n:]
		return n, nil
	}
	if body != nil {
		return body.Read(p)
	}
	if l.preroll != nil {
		if d := time.Until(l.until); d > 0 {
			return l.showPreroll(p, d)
		}
		return l.open(p)
	}
	// The encoder has been open and draining since the tune, so the release
	// can arrive at any moment — including in the middle of a filler wait. A
	// look that does not block, followed by a sleep, meant the program sat in
	// the channel until the sleep ended: measured at 468ms of a 500ms
	// keepalive stretch, against a gate that had released a keyframe 0ms
	// behind the newest packet it had read. The picture was fresh; it was the
	// filler's own wait that made it stale, and it is the same 480ms whatever
	// else is changed, because nothing else touched it. So wait for the
	// release and the next burst of filler together, and let the release cut
	// the wait short.
	left := time.Until(l.until)
	// Past the mark, the gate is hunting its keyframe — measured between a
	// hundred milliseconds and two seconds — and filler sent during that hunt
	// lands in the recording immediately in front of the program. NULL packets
	// carry no frames, so a viewer cannot play or seek through them: the
	// playhead stops at the last picture and the DVR's live edge sits at the
	// end of the NULLs, which is what "two or three seconds ahead of me that I
	// cannot fast forward" is made of. So once the mark has passed, nothing
	// more is sent. The DVR waits in silence for the length of a keyframe hunt
	// instead of being handed something it will keep and cannot show.
	//
	// Bounded, because silence is not free either: after keyframeQuiet the
	// filler comes back rather than let a DVR conclude the stream has died.
	// That is a hunt gone wrong, and the log says so.
	// The last stretch before the mark is the same problem as the hunt after
	// it. Filler sent in the seconds immediately before the program is what
	// ends up directly in front of the picture: NULL packets, no frames,
	// nothing a playhead can cross and nothing fast forward can land on —
	// "two or three seconds of nulls in front of me". So the wait goes quiet
	// early and stays quiet through the hand-off, and the program is the first
	// thing the DVR sees for that whole stretch.
	//
	// A DVR sits through far longer than this before deciding a stream has
	// died — the diet's own keepalive leaves it in a three kilobit trickle for
	// four seconds at a time — so a few seconds of nothing at the very end,
	// with a program arriving at the end of it, is well inside what it takes.
	if left <= quietBeforeMark {
		// Measured from the MARK, not from entering the quiet. The first go at
		// this timed keyframeQuiet from whenever the quiet began, which is
		// quietBeforeMark early, so it expired a second before the mark and
		// started filling again — putting NULL packets back exactly where they
		// were being removed from. The log said so: "no keyframe within 3s of
		// the mark" printed one second before the mark arrived.
		quietFor := time.Until(l.until.Add(keyframeQuiet))
		if quietFor <= 0 {
			quietFor = time.Millisecond
		}
		select {
		case r := <-l.handoff:
			return l.takeHandoff(p, r)
		case <-time.After(quietFor):
			if !l.quietSaid.Swap(true) {
				logger("[HOLD] %s no keyframe within %v of the mark; filling again so the DVR does not give up", l.label, keyframeQuiet)
			}
			return l.emitNulls(p, nullBurst)
		}
	}
	d, burst := l.nullPace(left)
	// No lock held across this select. There was one — a leftover from the
	// version that played a black clip through the wait and read its state
	// here — and it had no Unlock on any path out. Both ways out take l.mu
	// again (takeHandoff and emitNulls do, and emitNulls again through
	// tablesFor), and a sync.Mutex is not reentrant, so the very first filler
	// read of every held tune deadlocked against itself and died holding the
	// lock.
	//
	// That one line was the whole night's failure. drainEarly's next l.mu.Lock
	// is the line after it creates the stall reader, so the reader's producer
	// ran with nothing ever consuming it: the queue filled, threw itself away
	// on every push for ever — a hundred thousand chunks on a tune that ended
	// minutes earlier — and `encoder open and draining for the wait` never
	// printed. Nothing downstream of that line can happen. No packet reached
	// the gate, so the caption engine never got a first frame and never
	// started; no hand-off was ever posted, so the tune failed; and the tuner
	// was never released, so the next tune found it active and went to the
	// next box, unconditionally. Every symptom, from one unmatched Lock.
	wait := time.NewTimer(d)
	select {
	case r := <-l.handoff:
		wait.Stop()
		return l.takeHandoff(p, r)
	case <-wait.C:
	}
	return l.emitNulls(p, burst)
}

// blackMost and blackLeast bound it. blackFor is how long black plays at the start of a wait. It is there to give
// the player a picture and a time base before any filler, not to fill the
// whole hold: at a broadcast bitrate a ninety second wait of black is sixty
// odd megabytes, to do a job the first few seconds have already done.
const (
	// One second is two keyframes of the clip plus its tables — everything a
	// player needs to take a picture and a time base from, and nothing spare.
	// Five was tried first and is more black than a recording should carry.
	blackMost  = 1 * time.Second
	blackLeast = time.Second
)

// blackPatience is how long the wait will hold out for the black clip's next
// frames before falling back to NULL packets. Long enough that a real-time
// clip never loses the race — anything less threads NULLs between its frames —
// and short enough that a clip that has actually died is covered.
// drainGiveUp is how long past the mark a drain will wait for something to
// take its hand-off before closing the encoder itself. The keyframe hunt is a
// second or two, so this is far longer than any healthy tune needs; it exists
// only so a drain can never run for ever holding a tuner's encoder.
const drainGiveUp = 30 * time.Second

const blackPatience = 2 * time.Second

// stripNulls removes NULL packets from a buffer of transport stream, and says
// how many bytes went. They carry no frame, so nothing is lost by dropping
// them and a player has nothing to stall on.
func stripNulls(b []byte) ([]byte, int) {
	out := b[:0:0]
	gone := 0
	for i := 0; i+tsPacketSize <= len(b); i += tsPacketSize {
		pkt := b[i : i+tsPacketSize]
		if pkt[0] == 0x47 && int(pkt[1]&0x1F)<<8|int(pkt[2]) == 0x1FFF {
			gone += tsPacketSize
			continue
		}
		out = append(out, pkt...)
	}
	// A partial packet at the end is kept as it is; it is the start of
	// something the next read finishes.
	if tail := len(b) % tsPacketSize; tail != 0 {
		out = append(out, b[len(b)-tail:]...)
	}
	return out, gone
}

// takeHandoff swaps the filler for the released program and starts the
// reopen's clock, which must run from here and not from the encoder's open.
func (l *lateEncoder) takeHandoff(p []byte, r *handoffResult) (int, error) {
	if r == nil || r.body == nil {
		return 0, io.EOF
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		r.body.Close()
		return 0, io.EOF
	}
	// The pace watcher rides the drained hand-off too — it was only ever
	// wired on the paths that open the encoder late, so the one path the
	// user actually runs had gone silent.
	// The gate's first release goes out ahead of everything and never passed
	// the watcher, so NULL packets in it were invisible: the count said zero
	// because it only ever saw what came after. It is tables, a keyframe, and
	// whatever else was in the gate's buffer behind them — and anything
	// frameless in there lands directly in front of the picture, which is the
	// worst place in the stream for it. Strip it and say what was found.
	// One black frame, immediately before the picture and nothing in between.
	//
	// Whatever sits directly in front of the programme is what a playhead must
	// cross to reach it, and NULL packets carry no frame to land on. A frame
	// here gives the player something to decode and a time base to keep, and
	// the programme then follows video with video. It does not need to be a
	// second of black, or five, or the whole wait: one frame is all it takes,
	// and one frame is all that lands in a recording.
	first, _ := stripNulls(r.first)
	// Say it went out. The startup line only reports that a frame was made;
	// whether one ever reached a seam has never been in the log, so the one
	// thing this was built to do could not be confirmed from outside — "I
	// didn't see any lines about it creating black frames". Once per tune.
	if blk := blackPool; len(blk) > 0 {
		first = append(append([]byte(nil), blk...), first...)
		logger("[BLACK] %s %s of black went out at the seam, immediately in front of the picture, against %s of NULL packets in the wait",
			l.label, byteCount(int64(len(blk))), byteCount(l.nulls))
	} else {
		logger("[BLACK] %s no black went out, so the picture follows the wait directly", l.label)
	}
	l.body, l.pend = r.body, first
	body := l.body
	nulls := l.nulls
	l.mu.Unlock()
	if l.refresh != nil {
		l.refresh.arm(time.Now().Add(refreshAfterHold(holdDelay)))
	}
	heldRecently.Store(l.name, time.Now())
	logger("[HOLD] %s hold %v, %s sent, program starts", l.label, time.Since(l.t0).Round(time.Millisecond), byteCount(nulls))
	if len(l.pend) > 0 {
		n := copy(p, l.pend)
		l.pend = l.pend[n:]
		return n, nil
	}
	return body.Read(p)
}

// showPreroll passes the pre-roll on for what is left of the delay. The delay
// is what ends the wait, so a pre-roll longer than it is cut off, and one
// shorter is followed by NULL packets rather than starting the program early.
func (l *lateEncoder) showPreroll(p []byte, d time.Duration) (int, error) {
	gap := stallReadGap
	if d < gap {
		gap = d
	}
	select {
	case data, ok := <-l.preroll.out():
		if !ok {
			logger("[HOLD] %s pre-roll ended early; NULL packets for the rest of the wait", l.label)
			l.preroll = nil
			return l.serveNulls(p, d)
		}
		l.pend = append(l.pend, data...)
		n := copy(p, l.pend)
		l.pend = l.pend[n:]
		return n, nil
	case <-time.After(gap):
		if time.Until(l.until) <= 0 {
			return l.open(p)
		}
		return l.serveNulls(p, 0)
	}
}

// serveNulls sends NULL packets on the byte diet: volume through the DVR's
// detection window, a keepalive after. Every byte here is one the DVR stores
// ahead of the show.
func (l *lateEncoder) serveNulls(p []byte, d time.Duration) (int, error) {
	d, burst := l.nullPace(d)
	time.Sleep(d)
	return l.emitNulls(p, burst)
}

// nullPace is how long to wait before the next burst of filler and how big
// that burst may be, for a caller with d of the wait left to fill. Split out
// of serveNulls so a caller that has something better to do than sleep can
// wait on the diet's clock and on that other thing at once.
func (l *lateEncoder) nullPace(d time.Duration) (time.Duration, int) {
	pace, burst := holdRate(time.Since(l.dietFrom()))
	// A sparse keepalive was tried here — one packet every ten seconds after
	// the detection window — on the theory that fewer NULL packets means less
	// padding in front of the picture. The tune timed out. The DVR does not
	// sit through that, whatever the twenty second figure in the notes says,
	// and a failed tune is the worst thing this program can do. The diet is
	// what holdRate says it is.
	//
	// This had been tried before, and had failed before. Leaving the finding
	// here so it is not tried a third time.
	if d > pace || d <= 0 {
		d = pace
	}
	return d, burst
}

// emitNulls writes one burst of filler into p and counts what it sent.
//
// The filler carries no tables. A version of this sent the encoder's real PAT
// and PMT alongside the NULL packets, on the theory that a wait inside a
// declared programme is padding rather than unidentifiable stream. It never
// once ran: the tables come from l.gate, and l.gate is set by drainEarly one
// line past the Lock that deadlocked, so l.gate was nil on every tune this
// code has ever seen. It is removed rather than finally let loose, because
// CLAUDE.md records what tables in the wait did the last time they reached a
// DVR — it locked onto them and never played at all — and the first thing a
// fixed hold should do is not that.
func (l *lateEncoder) emitNulls(p []byte, burst int) (int, error) {
	if len(p) > burst {
		p = p[:burst]
	}
	n := nullPackets(p)
	l.mu.Lock()
	l.nulls += int64(n)
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	return n, nil
}

func (l *lateEncoder) open(p []byte) (int, error) {
	var preroll int64
	if l.preroll != nil {
		preroll = l.preroll.stop()
		l.preroll = nil
	}
	// http.Get, not a client with a Timeout: that field covers reading the
	// body, so it breaks the stream by force at a moment nothing chose and
	// leaves nothing able to decline. The break is wanted — see refreshAfterHold —
	// but it is made deliberately below, and made so it can fail safely.
	resp, err := http.Get(l.url)
	if err == nil && resp.StatusCode != 200 {
		resp.Body.Close()
		err = fmt.Errorf("status %s", resp.Status)
	}
	if err != nil {
		logger("[HOLD] %s encoder would not open after the hold: %v", l.label, err)
		return 0, err
	}
	liveEdge(resp.Body, l.label)
	armed := make(chan struct{})
	close(armed)
	// Captions wrap the encoder's stream, not the hold in front of it.
	// Wrapping the hold hands the caption engine the pre-roll to work on and
	// lets it rewrite the pre-roll's own video packets on the way past.
	// A NULL hold carried no PCR, so the jump to the program's clock is a real
	// discontinuity to declare, and the gate starts it on a keyframe.
	// Armed here and now: this path opens the encoder at the hand-off, so the
	// hand-off is this moment. refreshing hands back an unarmed source because
	// the drained path opens at the tune and must not spend its shed during
	// the wait — but nothing arms it here unless it is said, and an unarmed
	// source never sheds at all. Silent, and only on the pre-roll's path.
	rs := l.refreshing(resp.Body)
	rs.arm(time.Now().Add(refreshAfterHold(holdDelay)))
	body := markDiscontinuity(maybeWrapCaptions(
		newGateReader(l.stallTolerant(rs), armed, true, time.Now(), nil),
		l.tuner, l.name))
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		body.Close()
		return 0, io.EOF
	}
	l.body = body
	nulls := l.nulls
	l.mu.Unlock()
	logger("[HOLD] %s hold %v, %s sent, program starts", l.label, time.Since(l.t0).Round(time.Millisecond), byteCount(nulls+preroll))
	return body.Read(p)
}

func (l *lateEncoder) Close() error {
	l.mu.Lock()
	body, drain := l.body, l.drain
	l.closed = true
	l.mu.Unlock()
	if l.preroll != nil {
		l.preroll.stop()
		l.preroll = nil
	}
	// A hand-off nobody claimed still holds an open encoder. drainEarly posts
	// the released body to l.handoff and moves on; if the client has already
	// gone, Read is never called again, so the body sits in the channel with
	// its producer goroutine reading the encoder for ever. Take it and close
	// it. Non-blocking: an empty channel means the hand-off never happened,
	// and the drain below is what holds the connection in that case.
	select {
	case r := <-l.handoff:
		if r != nil && r.body != nil {
			r.body.Close()
		}
	default:
	}
	if drain != nil {
		drain.Close()
	}
	if body != nil {
		return body.Close()
	}
	return nil
}

// --- Watching the stream's pace after the hand-off ---
// Every instrument on the hold answers at the hand-off, and the question that
// remains is what happens after it: a hand-off measured dead live can sit
// behind on the TV, and nothing here said anything while it did. The encoder's
// PCR advances with the wall, so the stream's own timestamps are the yardstick:
// a stream that leaves here behind the wall by more and more is ah4c falling
// behind; one that keeps pace while the TV sits behind puts the lag downstream
// of this program. The watcher only reads and reports — the bytes pass through
// as they arrived.

// packetPCR reads the PCR (27 MHz) from a packet's adaptation field.
func packetPCR(pkt []byte) (uint64, bool) {
	if pkt[3]&0x20 == 0 || pkt[4] < 7 || pkt[5]&0x10 == 0 {
		return 0, false
	}
	base := uint64(pkt[6])<<25 | uint64(pkt[7])<<17 | uint64(pkt[8])<<9 |
		uint64(pkt[9])<<1 | uint64(pkt[10])>>7
	ext := uint64(pkt[10]&0x01)<<8 | uint64(pkt[11])
	return base*300 + ext, true
}

// --- The hand-off's discontinuity marker ---
// Telling the DVR the time base is new, so it does not read the jump from
// filler to program as corruption. ffmpeg spells this initial_discontinuity;
// there is no muxer in the path here, so it is set on the way past.

// firstDiscontinuity sets the discontinuity indicator on the first packet of
// each PID that carries an adaptation field, then steps aside.
type firstDiscontinuity struct {
	label string
	// first is when the first PID was marked; marking runs for markWindow
	// after it so every stream in the programme gets its wall, not just the
	// video that happens to be in the gate's first release.
	first time.Time
	io.ReadCloser
	seen map[int]bool
	done bool
}

func markDiscontinuity(src io.ReadCloser) io.ReadCloser {
	return &firstDiscontinuity{ReadCloser: src, seen: map[int]bool{}}
}

func (f *firstDiscontinuity) Read(p []byte) (int, error) {
	n, err := f.ReadCloser.Read(p)
	if f.done || n <= 0 {
		return n, err
	}
	marked := 0
	var on []string
	for i := 0; i+tsPacketSize <= n; i += tsPacketSize {
		pkt := p[i : i+tsPacketSize]
		if pkt[0] != 0x47 || pkt[3]>>4&2 == 0 || pkt[4] == 0 {
			continue
		}
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		if pid == 0x1FFF || f.seen[pid] {
			continue
		}
		f.seen[pid] = true
		pkt[5] |= 0x80
		marked++
		on = append(on, fmt.Sprintf("0x%X", pid))
	}
	if marked > 0 && f.first.IsZero() {
		f.first = time.Now()
	}
	// Do NOT stop at the first read. The gate's first release is tables and a
	// keyframe — all video — so stopping there marked the video PID and
	// nothing else, for ever: the log said "discontinuity marked on 0x64" and
	// that was the whole wall. Audio arrives in the next read, by which point
	// the marker had switched itself off, so the audio stream was never told
	// its time base was new. A player handed video that says "flush and
	// re-anchor" and audio that says nothing cannot reconcile the two, and
	// will not carry the playhead across the boundary — which leaves packets
	// in front of it that it cannot play and fast forward cannot land on.
	//
	// So keep marking the first packet of every programme PID until markWindow
	// after the first one, which is long enough for every stream in the PMT to
	// have shown up and short enough that nothing mid-programme is touched.
	if !f.first.IsZero() && time.Since(f.first) > markWindow {
		f.done = true
		// This is the wall between the tuning filler and the programme, and it
		// has never said whether it went up. A marker that silently fails to
		// mark looks exactly like one that worked, and this hold has been
		// caught by a silent instrument more than once tonight. The filler is
		// PID 0x1FFF only, which no programme uses, so the two are already
		// separate by PID; this is what tells the player the time base on the
		// programme's own PIDs is new and it should not try to carry anything
		// across.
	}
	return n, err
}

// --- The 1xx window ---
// A 1xx is protocol, not content: it puts nothing in the body, so the seconds
// spent on it cost the DVR no bytes. Bounded by the DVR's header clock, which
// no 1xx resets, so the hints stop short of it and the body carries the rest.

const (
	// hintEvery is how often a held request is kept alive.
	hintEvery = time.Second
	// hintCeiling is how long hints may run. The DVR's clock for the real
	// response headers runs from the request and no 1xx resets it: measured
	// at twenty-two seconds, so this keeps a wide margin under it.
	hintCeilingDefault = 18 * time.Second
	// hintProbe is how long to watch for a DVR that refuses 1xx outright.
	hintProbe = 750 * time.Millisecond
)

// hintCeiling is how long a hold may run on 1xx.
var hintCeiling = hintCeilingDefault

// hintsWork is cleared when a DVR refuses the hold outright, since taking the
// connection over cannot be undone.
var hintsWork atomic.Bool

func init() { hintsWork.Store(true) }

// hintHold is one request held on 1xx, with the real response written by hand.
type hintHold struct {
	conn  net.Conn
	rw    *bufio.ReadWriter
	sent  int
	began time.Time
	label string
}

// beginHintHold takes the connection over and probes the DVR with one hint,
// returning nil if it will not have them.
func beginHintHold(w http.ResponseWriter, label string) *hintHold {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		logger("[HOLD] %s the connection could not be taken over (%v); filling the body", label, err)
		return nil
	}
	h := &hintHold{conn: conn, rw: rw, began: time.Now(), label: label}
	if !h.hint() {
		h.refused(label, "would not take the first one")
		return nil
	}
	// A DVR that rejects informational responses closes at once; one that
	// accepts them says nothing, which is a read that times out.
	conn.SetReadDeadline(time.Now().Add(hintProbe))
	var b [1]byte
	if _, err := rw.Read(b[:]); err != nil {
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			h.refused(label, err.Error())
			return nil
		}
	}
	conn.SetReadDeadline(time.Time{})
	return h
}

// refused turns hints off for the process and drops the connection.
func (h *hintHold) refused(label, why string) {
	hintsWork.Store(false)
	logger("[HOLD] %s the DVR refused a held response (%s); holds from here fill the body", label, why)
	h.conn.Close()
}

// hint writes one informational response.
func (h *hintHold) hint() bool {
	if _, err := h.rw.WriteString("HTTP/1.1 103 Early Hints\r\n\r\n"); err != nil {
		return false
	}
	if err := h.rw.Flush(); err != nil {
		return false
	}
	h.sent++
	return true
}

// wait holds until until, and reports whether the DVR stayed.
func (h *hintHold) wait(until time.Time, label string) bool {
	for {
		left := time.Until(until)
		if left <= 0 {
			return true
		}
		if left > hintEvery {
			left = hintEvery
		}
		time.Sleep(left)
		if !h.hint() {
			hintsWork.Store(false)
			logger("[HOLD] %s the DVR gave up %v into the hold; holds from here fill the body", label, time.Since(h.began).Round(time.Millisecond))
			return false
		}
	}
}

// stream writes the real response and copies the program into it.
func (h *hintHold) stream(src io.Reader) (int64, error) {
	if _, err := h.rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: video/mp2t\r\nConnection: close\r\n\r\n"); err != nil {
		return 0, err
	}
	if err := h.rw.Flush(); err != nil {
		return 0, err
	}
	return copyFlush(bufWriter{h.rw}, src)
}

func (h *hintHold) Close() error { return h.conn.Close() }

// done ends a hop cleanly: the write side is shut first so the response is
// delivered rather than reset, which a bare close can do when anything is
// still unread on the connection.
func (h *hintHold) done() {
	if tc, ok := h.conn.(*net.TCPConn); ok {
		tc.CloseWrite()
		tc.SetReadDeadline(time.Now().Add(2 * time.Second))
		io.Copy(io.Discard, tc)
	}
	h.conn.Close()
}

// bufWriter flushes after every write, so no frame's tail waits on the next.
type bufWriter struct{ rw *bufio.ReadWriter }

func (b bufWriter) Write(p []byte) (int, error) { return b.rw.Write(p) }
func (b bufWriter) Flush()                      { b.rw.Flush() }

// holdOnHints holds a delayed tune on 1xx for as long as the DVR's clock
// allows, then hands back the connection to stream the rest. The second
// return says the connection has been taken over.
func holdOnHints(w http.ResponseWriter, src io.Reader, tuner, channel string) (*hintHold, bool) {
	if hintCeiling == 0 || holdDelay == 0 || prerollTS != "" || !hintsWork.Load() {
		return nil, false
	}
	label := "tuner=" + tuner + " channel=" + channel
	until := time.Now().Add(holdDelay)
	stop := time.Now().Add(hintCeiling)
	if until.Before(stop) {
		stop = until
	}
	h := beginHintHold(w, label)
	if h == nil {
		return nil, false
	}
	// The tune's scripts run off these reads; the filler itself is discarded.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 32*1024)
		for time.Until(stop) > time.Second {
			if _, err := src.Read(buf); err != nil {
				return
			}
		}
	}()
	ok := h.wait(stop, label)
	<-drained
	if !ok {
		h.Close()
		return nil, true
	}
	return h, true
}

// holdRate is how fast filler goes out, given how long the DVR has had a body
// to look at. Volume while it is deciding it has a stream, a keepalive after,
// and volume again for a moment every nullBeat so it goes on deciding that.
// Kept as its own function so the shape can be checked without a DVR.
func holdRate(since time.Duration) (time.Duration, int) {
	if since <= nullDetect {
		return nullPace, nullBurst
	}
	if (since-nullDetect)%nullBeat < nullBeatFor {
		return nullPace, nullBurst
	}
	return nullIdle, tsPacketSize
}

// stallTolerant wraps the encoder in the stall reader whether or not
// NULL_FRAME_INSERTION is switched on. A held tune is opened on a client that
// times out reading the body, so the hold is what breaks the stream every
// twenty seconds and the hold has to be what survives it. Without this, a
// container running the delay with NULL frame insertion off loses the stream
// twenty seconds after the program starts, every time.
func (l *lateEncoder) stallTolerant(body io.ReadCloser) *stallTolerantReader {
	return newStallTolerantReader(body, func() (io.ReadCloser, error) {
		r, e := http.Get(l.url)
		if e != nil {
			return nil, e
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			return nil, fmt.Errorf("status %s", r.Status)
		}
		return r.Body, nil
	}, l.label)
}

// The reopen's timing is measured, not chosen: at a ninety second hold, five
// seconds into the program left the viewer five seconds behind; fifteen,
// twenty-seven and thirty all landed at the live edge. What twenty does at
// ninety was never on that list, and every build that went back to a flat
// twenty has come back behind — the shed has to fall outside whatever the
// player is still doing with the program it just acquired. refreshBase is what
// every hold up to refreshPer was watched with; past that the point scales
// with the hold, never above refreshMost, the longest point watched working.
const (
	refreshBase = 20 * time.Second
	// refreshEvery is how often the break repeats after the first. A drift
	// that grows needs pulling back more than once; a single shed only ever
	// fixed the moment it happened.
	refreshEvery = 30 * time.Second
	refreshPer   = 45 * time.Second
	refreshMost  = 30 * time.Second
)

// refreshAfterHold is how long after the program starts the encoder is
// reopened, for a hold of the given length. Its own function so the arithmetic
// can be checked without a DVR: twenty seconds at forty-five or under, thirty
// at ninety or longer.
func refreshAfterHold(hold time.Duration) time.Duration {
	if hold <= refreshPer {
		return refreshBase
	}
	// Through float64: a Duration is nanoseconds, and nanoseconds times
	// nanoseconds overflows int64 for any hold longer than nine seconds.
	d := time.Duration(float64(hold) * float64(refreshBase) / float64(refreshPer))
	if d > refreshMost {
		d = refreshMost
	}
	return d
}

// refreshing reopens the encoder once, shortly after the program starts, and
// only if the encoder will have it. The new connection is opened before the old
// one is closed, so an encoder that refuses a second reader — and some do,
// while a tuner owns the stream — costs nothing at all: the refresh is declined,
// said so once, and never tried again for this tune.
func (l *lateEncoder) refreshing(body io.ReadCloser) *refreshSource {
	return &refreshSource{ReadCloser: body, label: l.label, open: func() (io.ReadCloser, error) {
		r, e := http.Get(l.url)
		if e != nil {
			return nil, e
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			return nil, fmt.Errorf("status %s", r.Status)
		}
		return r.Body, nil
	}}
}

type refreshSource struct {
	io.ReadCloser
	open func() (io.ReadCloser, error)
	// at is when the shed is due, as unix nanoseconds; zero until armed. It is
	// written by the DVR's goroutine at the hand-off and read by the drain's,
	// so it is atomic rather than a plain time.
	at    atomic.Int64
	done  int
	label string
}

// arm starts the shed's clock. Until this is called there is no shed, so a
// connection opened at tune start does not spend it during the wait.
func (r *refreshSource) arm(at time.Time) { r.at.Store(at.UnixNano()) }

func (r *refreshSource) Read(p []byte) (int, error) {
	// Once was right when there was something to shed once. Every reservoir in
	// this program is bounded now — the queue is half a megabyte, the kernel's
	// send buffer a quarter, no filler goes out after the hand-off — and what
	// is left is a viewer who starts at the live edge and then drifts back,
	// continuously, while the stream itself holds the wall to within fifteen
	// milliseconds. Nothing here is over-delivering. The player is falling
	// behind on its own, and the break is the only thing this program has that
	// reaches into it: it makes the DVR discard what it has stored ahead.
	//
	// A single break cannot hold a drift. It pulls the viewer forward once and
	// the drift resumes. So it repeats for as long as the tune runs.
	if at := r.at.Load(); at != 0 && time.Now().UnixNano() >= at {
		r.at.Store(time.Now().Add(refreshEvery).UnixNano())
		r.done++
		fresh, err := r.open()
		if err != nil {
			logger("[HOLD] %s the encoder would not open again (%v); leaving the stream as it is, the next one still comes", r.label, err)
		} else {
			old := r.ReadCloser
			r.ReadCloser = fresh
			old.Close()
			logger("[HOLD] %s reopened the encoder (%d), dropping what the DVR had stored ahead of the show", r.label, r.done)
		}
	}
	return r.ReadCloser.Read(p)
}

// --- Black at the seam ---
// The hold's filler is NULL packets, which carry no frame, so a playhead
// reaching them has nothing to land on and stops. That matters in exactly one
// place: directly in front of the picture. Everything earlier is behind the
// viewer.
//
// So black goes out immediately before the programme, with nothing between the
// two, and it is real video: frames with timestamps, keyframes a scrubber can
// land on, and a time base the player carries into the programme.
//
// blackSeamFor is the magic number and it has been found. Half a second of
// black immediately in front of the picture put a ninety second hold at the
// live edge, with the timeline where it belongs and fast forward working —
// after a night in which nothing else did. Two shorter lengths were watched
// first and neither moved anything: one frame, thirty-three milliseconds,
// which the user could not see at all; and one TS packet, which cannot hold a
// coded picture and so showed nothing. Half a second is about fifteen frames,
// with a keyframe twice a second for a scrubber to land on.
//
// The other end was tried and taken out again. The seam fixed the picture and
// left NULL packets at the head of the recording, so the same half second went
// in front of those too — and rewind stopped working. One clip used twice puts
// PTS 0 to half a second at the head, ninety seconds of clockless NULL packets,
// and then PTS 0 to half a second again at the seam: time runs backwards inside
// the file and a scrubber cannot cross it. Seam-only worked, seam-and-head did
// not, one change between the two.
//
// So the head stays as it was. If those leading NULL packets are worth
// answering later, the answer is not a second copy of this clip — it is a
// separate one whose timestamps start after the seam's, or an
// -output_ts_offset on the seam copy. Recorded so it is not rebuilt the
// obvious way a third time.
//
// Constants, not settings. These numbers were being searched for and they are
// the answer for everyone; an environment variable would ship the search.
const (
	blackSeamFor = 500 * time.Millisecond
	// blackCosts is what the black adds to a wait, and is taken back off
	// PLAYBACK_DELAY so a hold lasts what the user asked it to. Without this a
	// ninety second setting is a ninety and a half second hold, which is a
	// setting that quietly means something else.
	blackCosts = blackSeamFor
)

// blackPool is the black as a transport stream, or nil if it could not be made.
// It is exactly blackSeamFor long and goes out whole, once, at the seam.
var blackPool []byte

// blackStartup makes it once, before the listener binds, where nothing can be
// tuning yet (rule 10). A container that cannot make it still comes up, and
// hands over exactly as it did before black existed.
func blackStartup() {
	if prerollTS != "" {
		// A pre-roll fills the wait itself; it needs no black from here.
		return
	}
	const at = "/tmp/blackframe.ts"
	// -g 15 puts a keyframe twice a second. Fast forward navigates by
	// keyframes, so a run with only one at the front is a run a scrubber
	// cannot move through — which is the complaint this exists to answer.
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=1920x1080:r=30",
		"-t", fmt.Sprintf("%.3f", blackSeamFor.Seconds()),
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "15",
		"-pix_fmt", "yuv420p", "-f", "mpegts", at)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger("[BLACK] could not make the black (%v): %s; hand-offs go out as they did before", err, firstLine(string(out)))
		return
	}
	b, err := os.ReadFile(at)
	if err != nil || len(b) == 0 {
		logger("[BLACK] the black came out empty; hand-offs go out as they did before")
		return
	}
	// Whole TS packets only. A part packet at the seam is a torn packet
	// immediately in front of the programme, which is the one place in the
	// stream that cannot afford one.
	blackPool = b[:len(b)/tsPacketSize*tsPacketSize]
	logger("[BLACK] made %s of black, %s, to go in front of the picture at the hand-off",
		blackWords(blackSeamFor), byteCount(int64(len(blackPool))))
}

// blackWords says a black length. Not holdWords: that rounds to the second, so
// half a second reads as "1 second" — a log line that lies about the one number
// being tuned.
func blackWords(d time.Duration) string {
	if d < time.Second {
		return d.String()
	}
	return holdWords(d)
}


// dvrSendBuffer is how many bytes the kernel may hold for the DVR. At a
// broadcast bitrate this is a couple of hundred milliseconds — enough that a
// write is not blocking on every packet, and far too little to hide a second
// of video in. The default is autotuned into the megabytes, which is seconds.
const dvrSendBuffer = 256 * 1024

// liveListener caps the send buffer on every connection it accepts, so no
// connection this program serves can bank more than dvrSendBuffer of stream.
type liveListener struct{ net.Listener }

func (l liveListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		// Best effort: a kernel that will not take the size still serves the
		// stream, it just keeps its own idea of how much to hold.
		_ = tc.SetWriteBuffer(dvrSendBuffer)
	}
	return c, nil
}

// serveLive runs the router on addr with the send buffer capped. It replaces
// r.Run, which builds its own listener and leaves the buffer to the kernel.
func serveLive(r *gin.Engine, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Say what Run said. gin.RunListener announces itself as "Listening and
	// serving HTTP on listener what's bind with address@[::]:7654" — gin's own
	// wording, and it reads like a fault report rather than a service starting.
	// This line has said "Listening and serving HTTP on :7654" for as long as
	// the program has existed and capping a socket option is no reason for it
	// to change, so print that and let gin say nothing.
	//
	// Safe to silence: the engine is gin.New, not gin.Default, and the request
	// log is this program's own. Nothing else writes to DefaultWriter, and
	// RunListener does not return.
	fmt.Fprintf(gin.DefaultWriter, "[GIN-debug] Listening and serving HTTP on %s\n", addr)
	gin.DefaultWriter = io.Discard
	return r.RunListener(liveListener{ln})
}

// --- Watching the writes to the DVR ---
// Every instrument so far rides the read side: the pace watcher proves the
// bytes leave lateEncoder on the wall's clock, and the stall queue reports how
// deep it stands. The one stretch nobody has ever measured is the last one —
// the write into the DVR's socket. The kernel's send buffer can hold megabytes,
// a megabyte is seconds of video, and bytes standing there are downstream of
// every flush and drop this program has: nothing inside ah4c can shed them. A
// write only blocks when that buffer is full, so the time spent blocked in
// Write is the one number that says whether the DVR is draining ah4c or damming
// it. Zero means the lag the viewer sees lives past the DVR's ingest, where no
// byte-stream change here can reach it; whole seconds mean the DVR itself reads
// slower than the encoder sends, and now there is a log line saying which
// instead of an argument either way.

// writeStallEvery is how often the blocked time is reported: the pace
// watcher's cadence, so the two lines land side by side in the log.
const writeStallEvery = 15 * time.Second
