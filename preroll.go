package main

// The pre-roll: a video or still image bind-mounted at prerollMount, played
// wherever the DVR would otherwise get NULL packets and looping until the real

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// prerollMount is where the compose file binds the pre-roll: the file
	// PREROLL_FILE names, or the ah4c/preroll directory when it is blank.
	prerollMount = "/opt/preroll"
	// prerollCache is where the prepared transport stream lives.
	prerollCache = "/tmp/preroll.ts"
	// prerollStillSeconds is how long a clip a still image is made into. It
	// loops, so this only bounds the file's size.
	prerollStillSeconds = 10
	// prerollFrame is the frame a still is fitted into, whatever size and
	// shape it arrives in. 1080p is what a DVR and its players expect.
	prerollFrame = "1920:1080"
	// prerollPrepareBudget bounds preparation, so a container handed a file
	// it cannot turn into a transport stream still comes up.
	prerollPrepareBudget = 10 * time.Minute
)

// prerollTS is the prepared transport stream's path, or "" when there is no
// pre-roll to play. Set once at startup, read on every tune.
var prerollTS string

// prerollProbe is what ffprobe reports about the file, as far as preparation
// needs to know.
type prerollProbe struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		RFrameRate string `json:"r_frame_rate"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

// prerollPlan is how the file becomes a transport stream: the ffmpeg arguments
// between the program name and the output path, and a word for the log.
type prerollPlan struct {
	// rate is what the clip runs at once prepared: a video's own rate, kept,
	// or stillRate for a picture, which has none. The black is made to match
	// it so the two share one parameter set.
	rate int
	args []string
	kind string
}

// tsVideoCodecs and tsAudioCodecs are what a transport stream can carry
// without re-encoding, and what the DVR is used to being handed.
var (
	tsVideoCodecs = map[string]bool{"h264": true, "hevc": true, "mpeg2video": true}
	tsAudioCodecs = map[string]bool{"aac": true, "ac3": true, "eac3": true, "mp2": true, "mp3": true}
)

// planPreroll decides how src becomes a transport stream. Pure, so the
// decision can be tested without ffmpeg.
func planPreroll(src string, info prerollProbe) (prerollPlan, error) {
	video, audio := "", ""
	rateText := ""
	for _, s := range info.Streams {
		switch s.CodecType {
		case "video":
			if video == "" {
				video, rateText = s.CodecName, s.RFrameRate
			}
		case "audio":
			if audio == "" {
				audio = s.CodecName
			}
		}
	}
	if video == "" {
		return prerollPlan{}, fmt.Errorf("has no video stream")
	}
	still := stillFormat(info)
	// A picture has no rate and gets stillRate; a video keeps its own. If the
	// probe could not say, stillRate stands in rather than a guess at the
	// program's — the program never sees this stream.
	rate := stillRate
	if !still {
		if r := parseRate(rateText); r > 0 {
			rate = r
		}
	}
	var args []string
	var kind []string
	if still {
		args = append(args, "-loop", "1", "-framerate", fmt.Sprint(stillRate), "-t", fmt.Sprint(prerollStillSeconds))
		kind = append(kind, fmt.Sprintf("%s still cropped to %s as a %d second clip", video, prerollFrame, prerollStillSeconds))
	}
	args = append(args, "-i", src)
	if audio == "" {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000")
	}
	args = append(args, "-map", "0:v:0")
	if audio == "" {
		args = append(args, "-map", "1:a:0", "-shortest")
	} else {
		args = append(args, "-map", "0:a:0")
	}
	// The pre-roll must use the encoder's codec or the player freezes at the
	// hand-off — it will not switch its decoder. Keep an existing H.265 stream;
	// otherwise the Trixie image's libx265 converts it before the listener binds.
	if wantHEVC() && video == "hevc" {
		args = append(args, "-c:v", "copy")
		kind = append(kind, "H.265 video copied to match the encoder")
	} else {
		// Even dimensions and yuv420p are accepted by both software encoders.
		vf := "scale=trunc(iw/2)*2:trunc(ih/2)*2"
		if still {
			// A photograph is whatever size the camera made it. Encoded at
			// that size a phone picture is a 4032x3024 H.264 stream at level
			// 6, past what most decoders will touch, and it never appears at
			// all; a portrait picture becomes a portrait channel; and a
			// picture of any odd size makes a pre-roll whose resolution
			// differs from the program's, so the stream changes resolution
			// part way through. Video pre-rolls never had this because a video
			// is already a sensible size.
			//
			// So every still becomes the same thing: scaled up until it covers
			// a broadcast frame, then cropped to it from the center. It fills
			// the screen, it is never stretched, and it is the resolution the
			// program arrives at, so nothing changes at the hand-off.
			// No setsar: the ffmpeg bundled here does not have that filter,
			// and a still's pixels are square already.
			vf = "scale=" + prerollFrame + ":force_original_aspect_ratio=increase," +
				"crop=" + prerollFrame
		}
		// The clip's own rate is kept. Nothing forces it: a twenty-four frame
		// film stays twenty-four, and a fifty frame clip stays fifty. The
		// program is a separate stream on its own decoder and never sees it.
		args = append(args, "-vf", vf)
		if wantHEVC() {
			args = append(args, fillerHEVCEncodeArgs(rate)...)
		} else {
			args = append(args, fillerEncodeArgs(rate)...)
		}
		if !still {
			kind = append(kind, video+" video encoded to match the black")
		}
	}
	switch {
	case audio == "":
		args = append(args, "-c:a", "aac")
		kind = append(kind, "silent aac audio added")
	case tsAudioCodecs[audio]:
		args = append(args, "-c:a", "copy")
		kind = append(kind, audio+" audio copied")
	default:
		args = append(args, "-c:a", "aac")
		kind = append(kind, audio+" audio encoded to aac")
	}
	args = append(args, "-f", "mpegts")
	return prerollPlan{rate: rate, args: args, kind: strings.Join(kind, ", ")}, nil
}

// prerollStartup prepares whatever is mounted at prerollMount into
// prerollCache, before the listener binds. Anything that goes wrong is logged
func prerollStartup() {
	prerollTS = ""
	st, err := os.Stat(prerollMount)
	if err != nil {
		// Nothing mounted: ah4c is running outside the compose file.
		return
	}
	src := prerollMount
	if st.IsDir() {
		src = prerollInDir(prerollMount)
		if src == "" {
			// The empty default directory: no pre-roll, and nothing to say.
			return
		}
	} else if !st.Mode().IsRegular() {
		return
	}
	preparePreroll(src)
}

// prerollInDir picks preroll.* from a directory if there is one, otherwise the
// only file, otherwise the first by name with a note in the log. Hidden files
func prerollInDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger("[PREROLL] %s cannot be listed (%v); holds will use NULL packets", dir, err)
		return ""
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	pick := files[0]
	for _, f := range files {
		if strings.HasPrefix(strings.ToLower(f), "preroll.") {
			pick = f
			break
		}
	}
	if len(files) > 1 {
		logger("[PREROLL] %s holds %d files; using %s (name one preroll.* to choose)", dir, len(files), pick)
	}
	return filepath.Join(dir, pick)
}

// preparePreroll turns the file at src into a transport stream at
// prerollCache and points prerollTS at it, or logs why it could not.
func preparePreroll(src string) {
	ctx, cancel := context.WithTimeout(context.Background(), prerollPrepareBudget)
	defer cancel()
	t0 := time.Now()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_name,r_frame_rate:format=format_name",
		"-of", "json", src).Output()
	if err != nil {
		logger("[PREROLL] ffprobe could not read %s: %v; holds will use NULL packets", src, probeError(err))
		return
	}
	var info prerollProbe
	if err := json.Unmarshal(out, &info); err != nil {
		logger("[PREROLL] ffprobe's report on %s did not parse: %v; holds will use NULL packets", src, err)
		return
	}
	// The ffmpeg bundled here decodes almost no still formats — PNG among
	// them — so a still is decoded with Go's own decoders and handed over as a
	// JPEG, which is the one image format it always reads.
	if stillFormat(info) {
		if jpg, jerr := stillToJPEG(src); jerr != nil {
			logger("[PREROLL] %s could not be read as a picture (%v); letting ffmpeg try it as it is", src, jerr)
		} else if out, perr := exec.CommandContext(ctx, "ffprobe", "-v", "error",
			"-show_entries", "stream=codec_type,codec_name,r_frame_rate:format=format_name",
			"-of", "json", jpg).Output(); perr == nil {
			var reread prerollProbe
			if json.Unmarshal(out, &reread) == nil {
				src, info = jpg, reread
			}
		}
	}
	plan, err := planPreroll(src, info)
	if err != nil {
		logger("[PREROLL] %s %v; holds will use NULL packets", src, err)
		return
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, plan.args...)
	args = append(args, prerollCache)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(prerollCache)
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			msg = fmt.Sprintf("took longer than %v", prerollPrepareBudget)
		}
		logger("[PREROLL] preparing %s failed (%s): %v; holds will use NULL packets", src, plan.kind, errorLine(msg, err))
		return
	}
	st, err := os.Stat(prerollCache)
	if err != nil || st.Size() == 0 {
		logger("[PREROLL] preparing %s produced nothing; holds will use NULL packets", src)
		return
	}
	prerollTS = prerollCache
	prerollRate = plan.rate
	logger("[PREROLL] prepared %s in %v: %s at %d fps, %s at %s", src,
		time.Since(t0).Round(time.Millisecond), plan.kind, plan.rate, byteCount(st.Size()), prerollCache)
}

// probeError adds ffprobe's own words to an exec error, which otherwise only
// says "exit status 1".
func probeError(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return errorLine(msg, err)
		}
	}
	return err.Error()
}

// errorLine is the first line of what ffmpeg or ffprobe said, or err when
// they said nothing.
func errorLine(msg string, err error) string {
	if msg == "" {
		return err.Error()
	}
	return firstLine(msg)
}

// prerollPlayer is one ffmpeg looping the prepared pre-roll at real time,
// delivered as whole packets on a channel a reader can select on.
type prerollPlayer struct {
	cmd   *exec.Cmd
	ch    chan []byte
	done  chan struct{}
	once  sync.Once
	sent  atomic.Int64
	label string
	// adopted is set by a hold that took over a player started ahead of the
	// tune, so whoever started it knows not to stop it.
	adopted atomic.Bool
}

// startPreroll starts the pre-roll, or returns nil to fall back to NULL
// packets. A nil player's out() never delivers, so it can sit in a select.
func startPreroll(label string) *prerollPlayer {
	if prerollTS == "" {
		return nil
	}
	// -muxdelay 0 -muxpreload 0 so the clip's PTS does not run ahead of its own
	// PCR. ffmpeg's default mux delay put it 0.733s ahead, where the encoder
	// that follows sends PTS equal to PCR, and the seam then has to open a gap
	// the size of that lead to keep the program from landing under frames the
	// pre-roll has already scheduled. With no lead the gap closes to a single
	// frame, and the pre-roll can never again be the side with the longer one.
	args := []string{"-re", "-stream_loop", "-1", "-i", prerollTS,
		"-c", "copy", "-muxdelay", "0", "-muxpreload", "0"}
	args = append(args, fillerPIDArgs()...)
	return startPlayer(label, "PREROLL", append(args, "-f", "mpegts", "pipe:1")...)
}

// startPlayer runs one ffmpeg writing MPEG-TS to a channel, or nil if it will
// not start.
func startPlayer(label, tag string, args ...string) *prerollPlayer {
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		logger("[%s] %s could not start ffmpeg: %v; using NULL packets", tag, label, err)
		return nil
	}
	p := &prerollPlayer{cmd: cmd, ch: make(chan []byte, 4), done: make(chan struct{}), label: label}
	go p.pump(stdout)
	return p
}

// out is the channel the pre-roll's packets arrive on; it closes when the
// pre-roll ends on its own. On a nil player it is nil, which never delivers.
func (p *prerollPlayer) out() <-chan []byte {
	if p == nil {
		return nil
	}
	return p.ch
}

// pump moves ffmpeg's output onto the channel in whole packets.
func (p *prerollPlayer) pump(r io.Reader) {
	defer close(p.ch)
	defer p.cmd.Wait()
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			var data []byte
			data, carry = wholePackets(carry, buf[:n])
			if len(data) > 0 {
				select {
				case p.ch <- data:
					p.sent.Add(int64(len(data)))
				case <-p.done:
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// stop ends the pre-roll and returns how many bytes of it were handed on.
// Safe to call more than once, and on a player that already ended.
func (p *prerollPlayer) stop() int64 {
	p.once.Do(func() {
		close(p.done)
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
	})
	return p.sent.Load()
}

// tuneHold is playback detection's reason to hold, and what to send meanwhile.
// The playback delay does not come this way — it holds in lateEncoder, which
// leaves the encoder shut until the delay is up.
type tuneHold struct {
	// ready closes when the gate may release.
	ready <-chan struct{}
	label string
	t0    time.Time
	// fill is whether the DVR is sent anything during the hold.
	fill bool
	// early is a pre-roll already playing, to carry on with.
	early *prerollPlayer
}

// newTuneHold builds the hold, or nil when nothing holds the tune.
func newTuneHold(t0 time.Time, detect <-chan struct{}, label string, early *prerollPlayer) *tuneHold {
	if detect == nil {
		return nil
	}
	return &tuneHold{label: label, t0: t0, ready: detect, fill: prerollTS != "", early: early}
}

// wrap puts the hold's filler in front of src, which must be behind the gate.
func (h *tuneHold) wrap(src io.ReadCloser) io.ReadCloser {
	if h == nil || !h.fill {
		return src
	}
	return newHoldReader(src, h)
}

// holdReader serves filler until the gate's first real bytes, then steps aside.
type holdReader struct {
	src     io.ReadCloser
	hold    *tuneHold
	first   chan holdFirst
	preroll *prerollPlayer
	pend    []byte
	open    bool
	nulls   int64
	// sent is every byte handed downstream, so the hand-off can tell whether
	// the filler stopped on a packet boundary.
	sent int64
}

type holdFirst struct {
	data []byte
	err  error
}

func newHoldReader(src io.ReadCloser, hold *tuneHold) *holdReader {
	h := &holdReader{src: src, hold: hold, first: make(chan holdFirst, 1)}
	if hold.early != nil {
		h.preroll = hold.early
		h.preroll.adopted.Store(true)
	} else {
		h.preroll = startPreroll(hold.label)
	}
	show := ""
	if h.preroll != nil {
		show = ", showing the pre-roll"
	}
	logger("[HOLD] %s hold until the app is seen playing%s", hold.label, show)
	// Read until there are bytes or a real error, not once.
	//
	// One read was safe against gateReader, which loops internally and never
	// hands back (0, nil) — it blocks until it has something. It is not safe
	// against captionStream, which returns (0, nil) on purpose the first time
	// it sees bytes: it keeps that first chunk, starts its pump, and reports
	// nothing. So with captions on, a single read here ended the hold carrying
	// no data at all — the pre-roll stopped, the hand-off was declared, and
	// the gate's tables and keyframe were not in it.
	//
	// This is the third time tonight that (0, nil) has broken a caller that
	// did not expect it, and the second time it took down a tune. io.Reader
	// permits it; assume nothing here reads only once.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 || err != nil {
				h.first <- holdFirst{data: buf[:n], err: err}
				return
			}
		}
	}()
	return h
}

func (h *holdReader) Read(p []byte) (int, error) {
	if len(h.pend) > 0 {
		n := copy(p, h.pend)
		h.pend = h.pend[n:]
		h.sent += int64(n)
		return n, nil
	}
	if h.open {
		n, err := h.src.Read(p)
		h.sent += int64(n)
		return n, err
	}
	select {
	case f := <-h.first:
		return h.handoff(p, f)
	default:
	}
	select {
	case f := <-h.first:
		return h.handoff(p, f)
	case data, ok := <-h.preroll.out():
		if !ok {
			logger("[HOLD] %s pre-roll ended early; NULL packets until the hand-off", h.hold.label)
			h.preroll = nil
			return h.serveNulls(p), nil
		}
		return h.serve(p, data), nil
	case <-time.After(stallReadGap):
		return h.serveNulls(p), nil
	}
}

func (h *holdReader) serveNulls(p []byte) int {
	n := nullPackets(p)
	h.nulls += int64(n)
	h.sent += int64(n)
	return n
}

// handoff ends the filler and starts the real stream, on a packet boundary.
func (h *holdReader) handoff(p []byte, f holdFirst) (int, error) {
	h.open = true
	var preroll int64
	if h.preroll != nil {
		preroll = h.preroll.stop()
		h.preroll = nil
	}
	if f.err != nil && len(f.data) == 0 && len(h.pend) == 0 {
		logger("[HOLD] %s stream ended before the hand-off: %v", h.hold.label, f.err)
		return 0, f.err
	}
	since := time.Since(h.hold.t0)
	what := "NULL packets"
	if preroll > 0 {
		what = "pre-roll"
	}
	logger("[HOLD] %s hand-off at %v; sent %s of %s", h.hold.label, since.Round(time.Millisecond), byteCount(preroll+h.nulls), what)
	// Start the program on a packet boundary.
	//
	// ffmpeg's pre-roll is read from a pipe in whatever sizes the pipe gives,
	// and stopping it cuts wherever it had got to — measured at 132 bytes into
	// a packet on a real tune. Appending the program to that fragment glues
	// half a filler packet to the front of the program's first one, and every
	// packet after it is 56 bytes out of step for the rest of the stream.
	//
	// It has always done this. It became visible when the clock rewriter
	// arrived downstream, because that carves packets at fixed offsets and so
	// began writing timestamps into arbitrary payload from the seam onward —
	// which is what flickered. A demuxer on its own resyncs and loses a frame;
	// this made it lose all of them.
	//
	// So the filler is finished off first: trim the part packet when it is
	// still in hand, and complete it when it has already gone out.
	if k := (h.sent + int64(len(h.pend))) % tsPacketSize; k != 0 {
		if int64(len(h.pend)) >= k {
			h.pend = h.pend[:int64(len(h.pend))-k]
		} else {
			h.pend = append(h.pend, bytes.Repeat([]byte{0xFF}, int(tsPacketSize-k))...)
		}
	}
	// Half a second of black between the pre-roll and the program, the same as
	// the delay's own wait gets. The trim above has just put the stream on a
	// packet boundary, so this lands whole.
	// No black here. The pre-roll is its own stream and the program starts
	// on its own decoder; the half second of black is what a NULL-packet
	// wait needs to give the player a time base, and a pre-roll has had one
	// for the whole wait. The build the maintainer confirmed working sent
	// none — by accident, as it happened — and this makes that deliberate.
	// The encoder's clock goes out untouched.
	h.pend = append(h.pend, f.data...)
	n := copy(p, h.pend)
	h.pend = h.pend[n:]
	return n, nil
}

func (h *holdReader) serve(p, data []byte) int {
	h.pend = append(h.pend, data...)
	n := copy(p, h.pend)
	h.pend = h.pend[n:]
	h.sent += int64(n)
	return n
}

func (h *holdReader) Close() error {
	if h.preroll != nil {
		h.preroll.stop()
		h.preroll = nil
	}
	return h.src.Close()
}

// nullPackets fills p with whole NULL packets, so filler always lands on a
// packet boundary.
func nullPackets(p []byte) int {
	// A caller with less than a packet of room gets a part packet, and that is
	// the least bad of the options. Returning nothing would be (0, nil) from
	// every reader above this, which is the shape that has broken two callers
	// here already and spun a third. Nothing reads this small — copyFlush uses
	// thirty-two kilobytes — and if anything ever does, the fragment is one
	// packet of damage where a spin is a dead tune.
	if len(p) < tsPacketSize {
		return copy(p, nullTSPacket[:])
	}
	n := min(len(p)/tsPacketSize*tsPacketSize, len(nullFill))
	return copy(p, nullFill[:n])
}

// wholePackets splits carry plus b into whole packets and a partial tail.
func wholePackets(carry, b []byte) (data, rest []byte) {
	all := append(append([]byte{}, carry...), b...)
	whole := len(all) / tsPacketSize * tsPacketSize
	return all[:whole:whole], append([]byte(nil), all[whole:]...)
}

// byteCount prints a byte count the way the log reads it: whole units.
func byteCount(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// --- Answering the DVR before the scripts run ---
// The seconds the pre and tune scripts take would otherwise pile up in front
// of the program; answered at once, they are covered by the pre-roll instead.

// earlyTune is what the hold needs from the request: the moment the DVR asked,
// and the pre-roll already on screen.
type earlyTune struct {
	t0      time.Time
	preroll *prerollPlayer
}

// from is when the delay is counted from. The DVR's request, so the viewer
// waits PLAYBACK_DELAY and not that plus however long the scripts took.
func (e *earlyTune) from(fallback time.Time) time.Time {
	if e == nil {
		return fallback
	}
	return e.t0
}

// player is the pre-roll already playing, or nil.
func (e *earlyTune) player() *prerollPlayer {
	if e == nil {
		return nil
	}
	return e.preroll
}

// tuneEarly answers at once when a pre-roll or a hold is set, else waits.
func tuneEarly(idx, channel string) (io.ReadCloser, error) {
	return tuneEarlyWith(idx, channel, tune)
}

// tuneEarlyWith is tuneEarly over any tune function.
func tuneEarlyWith(idx, channel string, tuneFn func(string, string, *earlyTune) (io.ReadCloser, error)) (io.ReadCloser, error) {
	if prerollTS == "" && holdDelay == 0 {
		return tuneFn(idx, channel, nil)
	}
	t0 := time.Now()
	label := fmt.Sprintf("tuner=%s channel=%s", idx, channel)
	e := &earlyReader{label: label, t0: t0, result: make(chan tuneResult, 1)}
	e.preroll = startPreroll(label)
	go func() {
		r, err := tuneFn(idx, channel, &earlyTune{t0: t0, preroll: e.preroll})
		e.result <- tuneResult{r, err}
	}()
	if e.preroll != nil {
		logger("[PREROLL] %s answered at once with the pre-roll", label)
	} else {
		// holdAsked, not holdDelay: what the operator set, not the NULL-packet
		// wait left after the black at the seam is taken out of it. Reporting
		// the internal number would have a PLAYBACK_DELAY of one second
		// announce itself as five hundred milliseconds, which is true of the
		// filler and wrong about the tune.
		logger("[HOLD] %s holding this tune for %s", label, holdWords(holdAsked))
	}
	// One clock and one set of PIDs over the WHOLE response, early filler and
	// late program alike. The splice used to wrap only the tune's result, so
	// the pre-roll went out raw on its own PIDs during the scripts window and
	// renumbered afterward — a PID change mid-pre-roll, which is the one thing
	// the player will not follow, and it froze on the pre-roll before the
	// program ever arrived. Wrapping the early reader here means the filler is
	// renumbered onto the output PIDs from its first packet, unbroken into the
	// program.
	//
	// The NULL-packet wait needs it too, and its own regression proved it. The
	// half second of black at the seam goes out on the filler's PIDs while the
	// program arrives on the encoder's — two video PIDs, and the player locks
	// onto the black's and never switches to the program, frozen on black. It
	// went unseen only because the build before it could not make the black at
	// all. So every hold is spliced: the black and the program are renumbered
	// onto the one PID, and the NULL packets, which carry nothing, pass through
	// untouched. Detection without a pre-roll never reaches here.
	if prerollTS != "" {
		return spliceClock(e, label), nil
	}
	return e, nil
}

type tuneResult struct {
	r   io.ReadCloser
	err error
}

// earlyReader serves filler until the tune produces a stream.
type earlyReader struct {
	label   string
	t0      time.Time
	preroll *prerollPlayer
	result  chan tuneResult
	src     io.ReadCloser
	pend    []byte
	open    bool
	failed  error
	nulls   int64
	mu      sync.Mutex
	closed  bool
}

func (e *earlyReader) Read(p []byte) (int, error) {
	if len(e.pend) > 0 {
		n := copy(p, e.pend)
		e.pend = e.pend[n:]
		return n, nil
	}
	if e.failed != nil {
		return 0, e.failed
	}
	if e.open {
		return e.src.Read(p)
	}
	// The tune's stream always wins over filler, so look for it before waiting.
	select {
	case res := <-e.result:
		return e.arrive(p, res)
	default:
	}
	select {
	case res := <-e.result:
		return e.arrive(p, res)
	case data, ok := <-e.preroll.out():
		if !ok {
			logger("[PREROLL] %s pre-roll ended early; NULL packets", e.label)
			e.preroll = nil
			return e.serveNulls(p), nil
		}
		e.pend = append(e.pend, data...)
		n := copy(p, e.pend)
		e.pend = e.pend[n:]
		return n, nil
	case <-time.After(stallReadGap):
		return e.serveNulls(p), nil
	}
}

// serveNulls sends NULL packets, which carry no time.
//
// One packet per read, not a whole buffer. This covers the seconds the tuning
// scripts take, and it used to fill the caller's buffer every time — sixty-five
// kilobytes a go, a hundred and sixty kilobytes across three and a half
// seconds, which measured as ninety-five per cent of every NULL packet in the
// stream. The ninety second hold behind it sends eight. Those bytes sit at the
// very front of what the DVR keeps, ahead of anything a player can start on,
// and a viewer cannot cross them because NULL packets carry no frames.
//
// The stream stays constant — a packet every read gap, no silence — because
// silence here is what a DVR gives up on, and it is the first thing it sees.
// It is the volume that had no reason to be this high.
func (e *earlyReader) serveNulls(p []byte) int {
	n := 0
	if len(p) >= tsPacketSize {
		n = copy(p, nullTSPacket[:])
	} else {
		n = copy(p, nullTSPacket[:len(p)])
	}
	e.nulls += int64(n)
	return n
}

// arrive takes the tune's result; an adopted pre-roll keeps playing.
func (e *earlyReader) arrive(p []byte, res tuneResult) (int, error) {
	took := time.Since(e.t0).Round(time.Millisecond)
	if res.err != nil {
		if e.preroll != nil {
			e.preroll.stop()
			e.preroll = nil
		}
		e.failed = res.err
		logger("[PREROLL] %s tune failed after %v; ending the stream so the DVR retries: %v", e.label, took, res.err)
		if len(e.pend) > 0 {
			n := copy(p, e.pend)
			e.pend = e.pend[n:]
			return n, nil
		}
		return 0, res.err
	}
	e.mu.Lock()
	e.src, e.open = res.r, true
	closed := e.closed
	e.mu.Unlock()
	if closed {
		res.r.Close()
		return 0, io.EOF
	}
	if e.preroll != nil && !e.preroll.adopted.Load() {
		sent := e.preroll.stop()
		e.preroll = nil
		logger("[PREROLL] %s stream ready in %v; %s of pre-roll played", e.label, took, byteCount(sent+e.nulls))
	} else if e.preroll != nil {
		logger("[PREROLL] %s stream ready in %v; pre-roll carries on under the hold", e.label, took)
	} else {
		logger("[HOLD] %s stream ready in %v; %s of NULL packets covered the scripts", e.label, took, byteCount(e.nulls))
	}
	if len(e.pend) > 0 {
		n := copy(p, e.pend)
		e.pend = e.pend[n:]
		return n, nil
	}
	return e.src.Read(p)
}

// Close stops the filler and closes the tune's stream — which is what releases
// the tuner and runs the stop script, so a tune still running is waited for.
func (e *earlyReader) Close() error {
	e.mu.Lock()
	e.closed = true
	src, open := e.src, e.open
	e.mu.Unlock()
	if e.preroll != nil {
		e.preroll.stop()
		e.preroll = nil
	}
	if open {
		return src.Close()
	}
	if e.failed != nil {
		return nil
	}
	// Still running: close its stream when it exists, without making the
	// departed DVR wait.
	go func() {
		if res := <-e.result; res.r != nil {
			logger("[PREROLL] %s DVR left mid-tune; closing the stream it produced", e.label)
			res.r.Close()
		}
	}()
	return nil
}

// stillFormat reports whether ffprobe is describing a still image rather than
// a video. It reports one as a one-frame video from an image demuxer, and
// those are named image2 or end in _pipe.
func stillFormat(info prerollProbe) bool {
	return strings.Contains(info.Format.FormatName, "image2") ||
		strings.HasSuffix(info.Format.FormatName, "_pipe")
}

// stillToJPEG decodes a picture with Go's own decoders and writes it back as a
// JPEG. The ffmpeg bundled with this image has no decoder for PNG, BMP, GIF,
// WebP or TIFF — a PNG pre-roll failed with "Decoder (codec png) not found"
// and the hold quietly fell back to NULL packets — while mjpeg is always
// there. Go reads PNG, JPEG and GIF from the standard library, so the formats
// people actually drop in are covered without asking anything of ffmpeg.
func stillToJPEG(src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	img, kind, err := image.Decode(f)
	if err != nil {
		return "", err
	}
	out := filepath.Join(filepath.Dir(prerollCache), "preroll-still.jpg")
	w, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer w.Close()
	if err := jpeg.Encode(w, img, &jpeg.Options{Quality: 95}); err != nil {
		return "", err
	}
	b := img.Bounds()
	logger("[PREROLL] read %s as a %dx%d %s and handed it over as a JPEG, which is the one still format ffmpeg here decodes",
		filepath.Base(src), b.Dx(), b.Dy(), kind)
	return out, nil
}

// --- One clock to the DVR ---
//
// A pre-roll is real video and carries its own PCR and PTS, running from zero
// and restarting every time the clip loops. The program that follows carries
// the encoder's clock, which is an unrelated number tens of hours away. So the
// DVR is handed two timelines with a cliff between them, and a player has only
// bad options at that cliff: told the time base is new it flushes video and
// goes blind while it re-anchors, which is a spinning circle; not told, it
// reads the jump as being far behind live and races to catch up, dropping
// frames until it settles. Both were watched. They are the same fault seen
// from two sides.
//
// The NULL-packet wait never had this problem because NULL packets carry no
// timestamps at all — there is no first timeline to conflict with the second.
// A pre-roll cannot do that and still be a picture.
//
// So the cliff is removed instead of being announced. Every timestamp going to
// the DVR is mapped onto one clock that only ever moves forward: the pre-roll's
// loops are flattened into a continuous run, and when the program arrives its
// clock is offset to carry straight on from where the pre-roll stopped. After
// the first packet of the program the offset is a constant, so this is a few
// integer adds per packet and nothing else.
//
// Only where there is a pre-roll. The delay's own wait is NULL packets and half
// a second of black and has been watched landing at the live edge; it has no
// two timelines to reconcile and is not touched.

// ptsMod is the 33-bit wrap of a 90 kHz timestamp; pcrMod the 27 MHz one.
const (
	ptsMod = uint64(1) << 33
	pcrMod = ptsMod * 300
	// splicePickup is the gap left between the last filler timestamp and the
	// first program one — one frame at 30fps. Zero would put two pictures at
	// the same instant; a large value would put a gap in the timeline that a
	// player waits out.
	splicePickup = uint64(3000) // 90 kHz
	// spliceJump is how far a timestamp must move for it to be a new source
	// rather than the stream running on. A clip loop and a hand-off both land
	// far outside this; ordinary jitter and B-frame reordering do not.
	spliceJump = uint64(10 * 90000)
)

// clockSplice maps every timestamp it passes onto a single forward-only clock.
type clockSplice struct {
	io.ReadCloser
	label string
	// out is the last timestamp written, in 90 kHz. delta is what is added to
	// an input timestamp to get an output one, and in is the last input seen.
	out, high, delta, in uint64
	// psiVer is the version to declare on each table PID, and psiSeen is the
	// content it was last declared for. pmtPID is learned from the PAT of
	// whichever source is current.
	psiVer  map[int]byte
	psiSeen map[int]uint32
	pmtPID  int
	// pending is set when a presentation time announced a new source before
	// any of its clock had arrived; the next PCR is then adopted rather than
	// treated as a second new source.
	pending bool
	// marking is whether the first packet of each PID after a source change
	// is still to be flagged as a discontinuity; marked is which have been,
	// and markFrom is when the first was.
	marking  bool
	marked   map[int]bool
	markFrom time.Time
	// srcVideo and srcAudio are the current source's elementary PIDs, from its
	// own PMT; everything is renumbered onto the fixed output PIDs so the
	// player never sees the video PID change. cc is the regenerated continuity
	// counter per output PID, so it runs unbroken across the seam.
	srcVideo, srcAudio int
	cc                 map[int]byte
	started, said      bool
	// pend is rewritten and ready to go out; tail is what has been read but
	// does not yet make a whole packet. synced is whether the packet boundary
	// has been found, and err is kept until pend has drained.
	pend, tail, scratch []byte
	synced              bool
	err                 error
}

// spliceClock gives the DVR one clock whatever filled the wait.
func spliceClock(src io.ReadCloser, label string) io.ReadCloser {
	return &clockSplice{ReadCloser: src, label: label}
}

// Read hands back only whole packets that have been rewritten.
//
// The first version rewrote whatever the underlying reader returned, starting
// at byte zero and assuming that was a packet boundary. It is not: a chunk is
// thirty-two kilobytes and 32768 is not a multiple of 188, so every chunk ends
// part way through a packet and the next one starts part way through. The scan
// then read rubbish as packets, bailed at the first byte that was not 0x47,
// and left most of the chunk untouched — so the DVR was handed one stream with
// some timestamps offset and some not. That is what stuttered.
//
// So the boundary is held across reads: bytes accumulate in tail, whole
// packets are rewritten and moved to pend, and the part packet at the end
// waits for the rest of itself.
func (c *clockSplice) Read(p []byte) (int, error) {
	for len(c.pend) == 0 {
		if c.err != nil {
			// Nothing left to align; hand back whatever is stranded so the
			// stream ends where it ended rather than a packet short.
			if len(c.tail) > 0 {
				c.pend, c.tail = c.tail, nil
				break
			}
			return 0, c.err
		}
		if c.scratch == nil {
			c.scratch = make([]byte, 32*1024)
		}
		n, err := c.ReadCloser.Read(c.scratch)
		if n > 0 {
			c.tail = append(c.tail, c.scratch[:n]...)
			c.fill()
		}
		if err != nil {
			c.err = err
		}
	}
	n := copy(p, c.pend)
	c.pend = c.pend[n:]
	return n, nil
}

// fill moves every whole packet in tail through the rewriter and on to pend.
func (c *clockSplice) fill() {
	if !c.synced {
		c.sync()
		if !c.synced {
			return
		}
	}
	// Alignment is checked every time, not assumed once. A source that ends
	// mid-packet shifts everything after it, and carving fixed offsets through
	// that writes timestamps into payload — which is worse than the misaligned
	// stream it started from.
	if c.tail[0] != 0x47 {
		c.synced = false
		c.sync()
		if !c.synced {
			return
		}
	}
	whole := len(c.tail) / tsPacketSize * tsPacketSize
	if whole == 0 {
		return
	}
	c.rewrite(c.tail[:whole])
	c.pend = append(c.pend, c.tail[:whole]...)
	c.tail = append(c.tail[:0], c.tail[whole:]...)
}

// sync finds the packet boundary: a sync byte with another one exactly a
// packet later. One 0x47 on its own is a coincidence often enough to matter in
// a stream that is mostly video payload.
func (c *clockSplice) sync() {
	for i := 0; i+2*tsPacketSize <= len(c.tail); i++ {
		if c.tail[i] == 0x47 && c.tail[i+tsPacketSize] == 0x47 {
			c.tail = append(c.tail[:0], c.tail[i:]...)
			c.synced = true
			return
		}
	}
	// Keep only what could still be the start of a pair.
	if drop := len(c.tail) - 2*tsPacketSize; drop > 0 {
		c.tail = append(c.tail[:0], c.tail[drop:]...)
	}
}

// rewrite maps the timestamps in b, in place. Bytes that are not whole packets
// are left alone: a torn packet cannot be parsed safely and skipping it costs
// one packet's timestamps, where guessing at it would corrupt the payload.
func (c *clockSplice) rewrite(b []byte) {
	for i := 0; i+tsPacketSize <= len(b); i += tsPacketSize {
		pkt := b[i : i+tsPacketSize]
		if pkt[0] != 0x47 {
			// Skip the one packet, do not abandon the buffer. Giving up here
			// left the rest of a thirty-two kilobyte read — about a hundred
			// and seventy packets — carrying the encoder's raw timestamps,
			// hours away from the clock either side of them. One torn packet
			// is a frame; a hundred and seventy unmapped ones is the seam
			// breaking all over again, in the middle of the program.
			continue
		}
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		if pid == 0x1FFF {
			continue // NULL packets carry nothing to map
		}
		if pid == 0 {
			c.notePMTPID(pkt)
			c.patchPAT(pkt)
			c.bumpPSI(pkt)
			c.stamp(pkt, 0)
			continue
		}
		if pid == c.pmtPID {
			c.patchPMT(pkt)
			c.bumpPSI(pkt)
			c.setPID(pkt, outPMTPID)
			c.stamp(pkt, outPMTPID)
			continue
		}
		// Clocks first, then the packet is renumbered onto the one output PID
		// its stream shares, its continuity counter regenerated so it runs
		// unbroken, and the first packet after a source change flagged so the
		// decoder re-activates the incoming SPS. One PID throughout means the
		// player never switches video tracks — the one thing it will not do
		// mid-stream, and the freeze when it was asked to on separate PIDs.
		c.mapPCR(pkt)
		c.mapPES(pkt)
		var out int
		switch {
		case c.srcVideo != 0 && pid == c.srcVideo:
			out = outVideoPID
		case c.srcAudio != 0 && pid == c.srcAudio:
			out = outAudioPID
		default:
			continue
		}
		c.markPacket(pkt, out)
		c.setPID(pkt, out)
		c.stamp(pkt, out)
	}
}

// The clock is carried by two high-water marks, not one.
//
// out is the furthest PCR sent and high is the furthest presentation time. The
// pickup has to be measured from whichever is later, and it is high that
// matters: a pre-roll's PTS runs ahead of its own PCR — measured at 0.73s on a
// real tune, where the encoder that follows sends PTS equal to PCR. Picking up
// from the PCR alone therefore started the program 0.8 seconds underneath
// frames the pre-roll had already scheduled, and a decoder handed presentation
// times that go backwards shows them in the wrong order. That is what
// flickered.
//
// newSource is the pickup: whatever timestamp announced the new source is
// mapped to just past everything already sent, and the offset that achieves it
// becomes the offset for every timestamp after — so PCR, PTS and DTS all keep
// their spacing.
func (c *clockSplice) newSource(ts uint64, fromPCR bool) {
	ref := c.out
	if forward(c.high, ref) {
		ref = c.high
	}
	c.delta = (ref + splicePickup - ts) & (ptsMod - 1)
	// c.in is the transmission clock's last reading and only a PCR may set it.
	// A source announced by a presentation time — an audio PES ahead of the
	// first PCR, which a real tune produced — used to write that PTS into c.in,
	// so the source's own first PCR then looked like a third source and the
	// offset was picked twice, after some of the new source's timestamps had
	// already gone out on the first one. Measured at half a second of timeline
	// running backwards inside one GOP. Now the next PCR is adopted instead.
	if fromPCR {
		c.in = ts
	} else {
		c.pending = true
	}
	// Every PID of the new source gets its first packet flagged as a
	// discontinuity. The clocks are continuous, so it is not one — but the flag
	// is what a player acts on when a source changes underneath it: it re-reads
	// the tables and re-selects its tracks, which is the difference between the
	// build that switched to the program and the one that sat on the pre-roll's
	// last frame for ever. Rule 11: the marker looked inert and was removed as
	// dead; it was the thing that worked.
	c.marking, c.marked, c.markFrom = true, map[int]bool{}, time.Time{}
	if !c.said {
		c.said = true
		logger("[HOLD] %s the program's clock was carried on from the pre-roll's rather than left as a jump", c.label)
	}
}

// markPacket flags the first packet of each elementary PID after a source
// change, for markWindow after the first one so audio arriving a read or two
// behind the video is told as well. Only a packet with an adaptation field can
// carry the flag; the next one that has one is marked instead.
func (c *clockSplice) markPacket(pkt []byte, pid int) {
	if !c.marking || c.marked[pid] {
		return
	}
	if pkt[3]&0x20 == 0 || pkt[4] == 0 {
		return
	}
	pkt[5] |= 0x80
	c.marked[pid] = true
	if c.markFrom.IsZero() {
		c.markFrom = time.Now()
	} else if time.Since(c.markFrom) > markWindow {
		c.marking = false
	}
}

// advance follows the transmission clock. PCR never steps back within one
// continuity, so any backward step is a new source however small it looks — a
// clip shorter than spliceJump looping would otherwise read as the stream
// running on and be handed to the DVR going backwards.
func (c *clockSplice) advance(pcr uint64) {
	if !c.started {
		c.started, c.delta, c.out, c.high, c.in = true, 0, pcr, pcr, pcr
		return
	}
	switch {
	case c.pending:
		// A presentation time already announced this source and set the
		// offset; this is its clock arriving. Adopt it.
		c.pending, c.in = false, pcr
	case !forward(pcr, c.in):
		c.newSource(pcr, true)
	default:
		c.in = pcr
	}
	if o := (pcr + c.delta) & (ptsMod - 1); forward(o, c.out) {
		c.out = o
	}
}

// at maps a presentation or decode time.
//
// It can be the first thing seen from a new source: the gate releases on a
// random access indicator, which is a different bit from the PCR flag, so the
// program's first PES can reach here before any program PCR does. Measured on
// a real tune — an audio PTS arrived one packet ahead of the first PCR and was
// given the pre-roll's offset, landing six seconds adrift. So a timestamp far
// from the clock in either direction announces a new source too.
//
// Either direction, because DTS sits below PTS on every reordered frame and
// audio interleaves below video. Comparing unsigned distances instead made
// every one of those look like a new source, and the picture crawled.
func (c *clockSplice) at(ts uint64) uint64 {
	if !c.started {
		return ts
	}
	if !near(ts, c.in) {
		c.newSource(ts, false)
	}
	o := (ts + c.delta) & (ptsMod - 1)
	if forward(o, c.high) {
		c.high = o
	}
	return o
}

// near is whether two timestamps are within spliceJump of one another, either
// way round. forward is whether a is ahead of b by less than that.
func near(a, b uint64) bool { return forward(a, b) || forward(b, a) }

func forward(a, b uint64) bool { return (a-b)&(ptsMod-1) < spliceJump }

func (c *clockSplice) mapPCR(pkt []byte) {
	if pkt[3]&0x20 == 0 || pkt[4] < 7 || pkt[5]&0x10 == 0 {
		return
	}
	base := uint64(pkt[6])<<25 | uint64(pkt[7])<<17 | uint64(pkt[8])<<9 |
		uint64(pkt[9])<<1 | uint64(pkt[10])>>7
	c.advance(base)
	out := c.at(base)
	pkt[6] = byte(out >> 25)
	pkt[7] = byte(out >> 17)
	pkt[8] = byte(out >> 9)
	pkt[9] = byte(out >> 1)
	pkt[10] = byte(out&1)<<7 | pkt[10]&0x7F
}

func (c *clockSplice) mapPES(pkt []byte) {
	if pkt[1]&0x40 == 0 { // not the start of a PES packet
		return
	}
	off := 4
	if pkt[3]&0x20 != 0 {
		off += 1 + int(pkt[4])
	}
	// off+14, not off+13: es[9:14] needs fourteen bytes from off. At off 175
	// the short slice is still legal Go — the packet's capacity runs past its
	// length — so writeTS clobbered the next packet's sync byte instead of
	// failing, measured as 0x97 and 0xf1 where 0x47 belonged. The DTS guard
	// below has always used the right figure.
	if off+14 > tsPacketSize {
		return
	}
	es := pkt[off:]
	if es[0] != 0 || es[1] != 0 || es[2] != 1 {
		return
	}
	// Only the stream kinds that actually carry the optional PES header. A
	// padding or private_stream_2 packet has payload where that header would
	// be, so reading es[7] as flags there and writing five bytes back is not a
	// wrong timestamp, it is corrupt payload. Nothing in this stream sends one
	// today; the guard is here because "nothing sends one today" is how the
	// last several faults in this file started.
	//
	// This layer is the same for H.264 and HEVC. PCR lives in the adaptation
	// field and PTS/DTS in the PES header, and neither knows what codec is
	// inside — so an HEVC encoder is mapped by exactly this code, and the
	// only thing HEVC changes is how expensive it is to get this wrong.
	if !pesHasHeader(es[3]) {
		return
	}
	if es[6]&0xC0 != 0x80 { // not an MPEG-2 PES optional header
		return
	}
	flags := es[7] >> 6
	if flags < 2 || int(es[8]) < 5 {
		return // no PTS
	}
	writeTS(es[9:14], c.at(readTS(es[9:14])))
	if flags == 3 && int(es[8]) >= 10 && off+19 <= tsPacketSize {
		writeTS(es[14:19], c.at(readTS(es[14:19])))
	}
}

// pesHasHeader says whether a stream_id carries the optional PES header that
// holds PTS and DTS. Everything does except these, per ISO 13818-1 2.4.3.7.
func pesHasHeader(streamID byte) bool {
	switch streamID {
	case 0xBC, // program_stream_map
		0xBE, // padding_stream
		0xBF, // private_stream_2
		0xF0, // ECM
		0xF1, // EMM
		0xF2, // DSMCC
		0xF8, // H.222.1 type E
		0xFF: // program_stream_directory
		return false
	}
	return true
}

// readTS and writeTS are the five-byte PTS/DTS field, marker bits and all. The
// top four bits are the prefix that says which of PTS and DTS this is, and are
// left exactly as they were found.
func readTS(f []byte) uint64 {
	return uint64(f[0]>>1&0x07)<<30 |
		uint64(f[1])<<22 |
		uint64(f[2]>>1&0x7F)<<15 |
		uint64(f[3])<<7 |
		uint64(f[4]>>1&0x7F)
}

func writeTS(f []byte, ts uint64) {
	f[0] = f[0]&0xF0 | byte(ts>>30&0x07)<<1 | 1
	f[1] = byte(ts >> 22)
	f[2] = byte(ts>>15&0x7F)<<1 | 1
	f[3] = byte(ts >> 7)
	f[4] = byte(ts&0x7F)<<1 | 1
}

// --- Telling the player the tables changed ---
//
// Every source here declares program 1 with version_number 0: the pre-roll's
// tables, the black's, and the encoder's. A demuxer caches a table and only
// re-reads it when the version moves, so after the filler it kept demuxing the
// PIDs the first table named — 256 and 257 from ffmpeg — while the program
// arrived on 100 and 101. Those PIDs had stopped, so the picture stopped.
//
// Measured before it was understood: two distinct tables in one capture, both
// reading ver=0, one change between them. It was in the notes as an oddity and
// walked past.
//
// So the version is stepped at every source change. That is the whole purpose
// of the field, and it costs one byte plus a checksum.

// crcMPEG is the CRC-32/MPEG-2 that closes a PSI section: polynomial
// 0x04C11DB7, all ones in, no reflection, no final xor.
func crcMPEG(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// bumpPSI gives a table a version that moves when, and only when, the table's
// own contents move — then fixes the checksum to match.
//
// Content, not sources. The first attempt stepped one counter per source
// change, and three sources need three versions where that counter managed
// two: the pre-roll declared programme 1 version 0 on PIDs 256 and 257, the
// black declared version 1 on PID 256, and the programme declared version 1 on
// PIDs 100 and 101 — the same version as the black. A demuxer had already
// cached version 1 and ignored the programme's table, so it went on demuxing
// 256 for ever. That is the infinite pre-roll, and it was measured: four
// distinct tables in one capture, two pairs sharing a version.
//
// Keyed off the content, miscounting cannot happen. A table that repeats keeps
// its version, which is exactly what the field promises a demuxer; a table
// that differs in any way at all gets the next one and is re-read.
//
// Sections spanning packets are left alone: rewriting half a section is worse
// than an unstepped version, and nothing here splits them.
func (c *clockSplice) bumpPSI(pkt []byte) {
	if pkt[1]&0x40 == 0 {
		return
	}
	pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
	off := 4
	if pkt[3]&0x20 != 0 {
		off += 1 + int(pkt[4])
	}
	if off >= tsPacketSize {
		return
	}
	off += 1 + int(pkt[off])
	if off+8 > tsPacketSize {
		return
	}
	sec := pkt[off:]
	if sec[0] != 0x00 && sec[0] != 0x02 {
		return
	}
	slen := int(sec[1]&0x0F)<<8 | int(sec[2])
	end := 3 + slen
	if slen < 9 || end > len(sec) {
		return
	}
	// What the table says, with the version taken out, so a repeat of the same
	// table hashes the same however it has been stamped on the way past.
	was := sec[5]
	sec[5] = was & 0xC1
	sum := crcMPEG(sec[:end-4])
	sec[5] = was
	if c.psiVer == nil {
		c.psiVer, c.psiSeen = map[int]byte{}, map[int]uint32{}
	}
	if seen, ok := c.psiSeen[pid]; !ok {
		c.psiSeen[pid], c.psiVer[pid] = sum, was>>1&0x1F
	} else if seen != sum {
		c.psiSeen[pid] = sum
		c.psiVer[pid] = (c.psiVer[pid] + 1) & 0x1F
	}
	sec[5] = was&0xC0 | c.psiVer[pid]<<1 | was&0x01
	crc := crcMPEG(sec[:end-4])
	sec[end-4] = byte(crc >> 24)
	sec[end-3] = byte(crc >> 16)
	sec[end-2] = byte(crc >> 8)
	sec[end-1] = byte(crc)
}

// notePMTPID learns which PID carries the program table from the PAT, so the
// table can be found however the source numbers it. ffmpeg says 0x1000 and an
// encoder may say anything.
func (c *clockSplice) notePMTPID(pkt []byte) {
	off := 4
	if pkt[3]&0x20 != 0 {
		off += 1 + int(pkt[4])
	}
	if pkt[1]&0x40 == 0 || off >= tsPacketSize {
		return
	}
	off += 1 + int(pkt[off])
	if off+12 > tsPacketSize {
		return
	}
	sec := pkt[off:]
	if sec[0] != 0x00 {
		return
	}
	// The first entry with a program number. program_number 0 is the NIT and
	// names no program table; an encoder that lists it first would otherwise
	// have its real table go unread.
	slen := int(sec[1]&0x0F)<<8 | int(sec[2])
	for i := 8; i+4 <= 3+slen-4 && i+4 <= len(sec); i += 4 {
		prog := int(sec[i])<<8 | int(sec[i+1])
		pid := int(sec[i+2]&0x1F)<<8 | int(sec[i+3])
		if prog != 0 && pid > 0 && pid < 0x1FFF {
			c.pmtPID = pid
			return
		}
	}
}

// --- One recipe for everything that fills a wait ---
//
// The pre-roll and the black share one stream and one decoder, and every
// distinct SPS on that stream is a reconfiguration a viewer can see. Made with
// exactly this and nothing else, their parameter sets come out identical, so
// the black follows the pre-roll with no reconfiguration at all. The program
// is a separate stream on its own PIDs and its own decoder; nothing about the
// filler — rate, level, profile — crosses over to it.
//
// No B-frames, because a stream with B-frames cut anywhere but a GOP boundary
// leaves the decoder holding pictures whose references never arrive. One
// reference frame and a keyframe every half second, so any cut is close to
// clean. Colour signalled explicitly, so a photograph's metadata and a lavfi
// source do not produce different VUI.
//
// stillRate is what a picture is played at, since a picture has none of its
// own, and what the black is made at when there is no pre-roll. Thirty, the
// value the NULL-packet path was watched landing at the live edge with.
const stillRate = 30

// prerollRate is what the prepared pre-roll runs at — its own rate, never
// forced — so the black can be made to match it. Zero when there is none.
var prerollRate int

// parseRate reads ffprobe's r_frame_rate, "60000/1001" or "25/1", to the
// nearest whole picture a second. Zero when it cannot.
func parseRate(s string) int {
	num, den, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		den = "1"
	}
	n, err1 := strconv.Atoi(num)
	d, err2 := strconv.Atoi(den)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0
	}
	r := (n + d/2) / d
	if r < 1 || r > 240 {
		return 0
	}
	return r
}

// fillerPIDArgs puts the filler on PIDs no encoder uses. ffmpeg's defaults are
// 0x100, 0x101 and a table on 0x1000, and so are the defaults of every
// libavformat-based encoder — and if the program arrived on the same PIDs as
// the pre-roll, nothing downstream would see a change: the PIDs would already
// be known, so no flag; the tables would be byte-identical, so no version
// step; and one decoder would carry both sources on one PID, which is the
// arrangement reverted for flicker. 0xF00 is in nobody's defaults. Checked in
// the container's ffmpeg under -c copy: video 0xF00, audio 0xF01, table 0xF0F.
func fillerPIDArgs() []string {
	return []string{"-mpegts_start_pid", "0xF00", "-mpegts_pmt_start_pid", "0xF0F"}
}

func fillerEncodeArgs(rate int) []string {
	if rate < 2 {
		rate = stillRate
	}
	return []string{
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "high",
		"-x264-params", fmt.Sprintf("keyint=%d:min-keyint=%d:scenecut=0:bframes=0:ref=1:aud=1", rate/2, rate/2),
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709",
	}
}

func fillerHEVCEncodeArgs(rate int) []string {
	if rate < 2 {
		rate = stillRate
	}
	return []string{
		"-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "main",
		"-x265-params", fmt.Sprintf("keyint=%d:min-keyint=%d:scenecut=0:bframes=0:ref=1:aud=1", rate/2, rate/2),
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709",
	}
}

// --- One program, whatever is feeding it ---
//
// The pre-roll and the program arrive as their own programs on their own PIDs:
// ffmpeg numbers the pre-roll's video 0xF00, and the encoder uses its own. A
// player picks its video track when the stream opens and will not change it
// when the table changes underneath it — measured, with a byte-perfect seam it
// still froze. So nothing downstream is told the streams changed: every source
// is renumbered onto one set of PIDs, its table rewritten to declare them, the
// continuity counters regenerated so each PID runs unbroken. The player sees
// one program that never stops, and the program's own SPS with its keyframe
// re-activates the decoder to the program's real parameters — its own frame
// rate, not the filler's.
const (
	outVideoPID = 0x100
	outAudioPID = 0x101
	outPMTPID   = 0x1000
)

func (c *clockSplice) setPID(pkt []byte, pid int) {
	pkt[1] = pkt[1]&0xE0 | byte(pid>>8)&0x1F
	pkt[2] = byte(pid & 0xFF)
}

// stamp regenerates the continuity counter for a PID this splice synthesises.
// Two sources merged onto one PID each bring their own counter, and the jump
// where they meet is a discontinuity to every demuxer that reads it. Only
// packets carrying payload advance it, which is what the standard says.
func (c *clockSplice) stamp(pkt []byte, pid int) {
	if c.cc == nil {
		c.cc = map[int]byte{}
	}
	if pkt[3]&0x10 == 0 {
		pkt[3] = pkt[3]&0xF0 | c.cc[pid]
		return
	}
	c.cc[pid] = (c.cc[pid] + 1) & 0x0F
	pkt[3] = pkt[3]&0xF0 | c.cc[pid]
}

func psiBody(pkt []byte) []byte {
	if pkt[1]&0x40 == 0 {
		return nil
	}
	off := 4
	if pkt[3]&0x20 != 0 {
		off += 1 + int(pkt[4])
	}
	if off >= tsPacketSize {
		return nil
	}
	off += 1 + int(pkt[off])
	if off+8 > tsPacketSize {
		return nil
	}
	sec := pkt[off:]
	slen := int(sec[1]&0x0F)<<8 | int(sec[2])
	if slen < 9 || 3+slen > len(sec) {
		return nil
	}
	return sec
}

// patchPAT points the single program at the fixed table PID.
func (c *clockSplice) patchPAT(pkt []byte) {
	sec := psiBody(pkt)
	if sec == nil || sec[0] != 0x00 {
		return
	}
	sec[10] = sec[10]&0xE0 | byte(outPMTPID>>8)&0x1F
	sec[11] = byte(outPMTPID & 0xFF)
}

// patchPMT learns the source's elementary PIDs and rewrites the table to
// declare the fixed ones and to put the clock on the fixed video PID.
func (c *clockSplice) patchPMT(pkt []byte) {
	sec := psiBody(pkt)
	if sec == nil || sec[0] != 0x02 {
		return
	}
	slen := int(sec[1]&0x0F)<<8 | int(sec[2])
	end := 3 + slen - 4
	il := int(sec[10]&0x0F)<<8 | int(sec[11])
	i := 12 + il
	if i > end {
		return
	}
	video, audio := 0, 0
	for i+4 < end {
		st := sec[i]
		pid := int(sec[i+1]&0x1F)<<8 | int(sec[i+2])
		esil := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
		if i+5+esil > end {
			return
		}
		var to int
		switch st {
		case 0x01, 0x02, 0x1B, 0x24: // MPEG-2, H.264, HEVC
			if video == 0 {
				video, to = pid, outVideoPID
			}
		case 0x03, 0x04, 0x0F, 0x11, 0x81, 0x87: // MPEG audio, AAC, AC-3
			if audio == 0 {
				audio, to = pid, outAudioPID
			}
		}
		if to != 0 {
			sec[i+1] = sec[i+1]&0xE0 | byte(to>>8)&0x1F
			sec[i+2] = byte(to & 0xFF)
		}
		i += 5 + esil
	}
	if video == 0 {
		return
	}
	c.srcVideo, c.srcAudio = video, audio
	sec[8] = sec[8]&0xE0 | byte(outVideoPID>>8)&0x1F
	sec[9] = byte(outVideoPID & 0xFF)
}
