package main

// The pre-roll: a video or still image bind-mounted at prerollMount, played
// wherever the DVR would otherwise get NULL packets and looping until the real

import (
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
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

// prerollPlan is how the file becomes a transport stream: the ffmpeg arguments
// between the program name and the output path, and a word for the log.
type prerollPlan struct {
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
	for _, s := range info.Streams {
		switch s.CodecType {
		case "video":
			if video == "" {
				video = s.CodecName
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
	var args []string
	var kind []string
	if still {
		args = append(args, "-loop", "1", "-framerate", "30", "-t", fmt.Sprint(prerollStillSeconds))
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
	if !still && tsVideoCodecs[video] {
		args = append(args, "-c:v", "copy")
		kind = append(kind, video+" video copied")
	} else {
		// -c:v h264 picks whichever H.264 encoder this ffmpeg was built with.
		// Even dimensions and yuv420p are what every H.264 encoder accepts;
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
		args = append(args, "-vf", vf, "-c:v", "h264", "-pix_fmt", "yuv420p", "-g", "30")
		if !still {
			kind = append(kind, video+" video encoded to h264")
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
	return prerollPlan{args: args, kind: strings.Join(kind, ", ")}, nil
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
		"-show_entries", "stream=codec_type,codec_name:format=format_name",
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
			"-show_entries", "stream=codec_type,codec_name:format=format_name",
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
	logger("[PREROLL] prepared %s in %v: %s, %s at %s", src,
		time.Since(t0).Round(time.Millisecond), plan.kind, byteCount(st.Size()), prerollCache)
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
	return startPlayer(label, "PREROLL", "-re", "-stream_loop", "-1", "-i", prerollTS,
		"-c", "copy", "-f", "mpegts", "pipe:1")
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
		return n, nil
	}
	if h.open {
		return h.src.Read(p)
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
	out, delta, in uint64
	started, said  bool
	rest           []byte
}

// spliceClock gives the DVR one clock whatever filled the wait.
func spliceClock(src io.ReadCloser, label string) io.ReadCloser {
	return &clockSplice{ReadCloser: src, label: label}
}

func (c *clockSplice) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		c.rewrite(p[:n])
	}
	return n, err
}

// rewrite maps the timestamps in b, in place. Bytes that are not whole packets
// are left alone: a torn packet cannot be parsed safely and skipping it costs
// one packet's timestamps, where guessing at it would corrupt the payload.
func (c *clockSplice) rewrite(b []byte) {
	for i := 0; i+tsPacketSize <= len(b); i += tsPacketSize {
		pkt := b[i : i+tsPacketSize]
		if pkt[0] != 0x47 {
			return // lost alignment; leave the rest untouched
		}
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		if pid == 0x1FFF {
			continue // NULL packets carry nothing to map
		}
		c.mapPCR(pkt)
		c.mapPES(pkt)
	}
}

// pick decides where an input timestamp lands on the output clock, and moves
// the clock forward. A step outside spliceJump is a new source — a clip that
// has looped, or the program arriving — and picks up from where the last one
// stopped rather than following the input's own number.
func (c *clockSplice) pick(in uint64) uint64 {
	switch {
	case !c.started:
		c.started, c.delta, c.out, c.in = true, 0, in, in
		return in
	case (in-c.in)&(ptsMod-1) < spliceJump:
		// Running on, including a small step backwards for B-frame order.
	default:
		c.delta = (c.out + splicePickup - in) & (ptsMod - 1)
		if !c.said {
			c.said = true
			logger("[HOLD] %s the program's clock was carried on from the pre-roll's rather than left as a jump", c.label)
		}
	}
	c.in = in
	out := (in + c.delta) & (ptsMod - 1)
	if (out-c.out)&(ptsMod-1) < spliceJump {
		c.out = out
	}
	return out
}

func (c *clockSplice) mapPCR(pkt []byte) {
	if pkt[3]&0x20 == 0 || pkt[4] < 7 || pkt[5]&0x10 == 0 {
		return
	}
	base := uint64(pkt[6])<<25 | uint64(pkt[7])<<17 | uint64(pkt[8])<<9 |
		uint64(pkt[9])<<1 | uint64(pkt[10])>>7
	out := c.pick(base)
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
	if off+13 > tsPacketSize {
		return
	}
	es := pkt[off:]
	if es[0] != 0 || es[1] != 0 || es[2] != 1 {
		return
	}
	flags := es[7] >> 6
	if flags < 2 || int(es[8]) < 5 {
		return // no PTS
	}
	writeTS(es[9:14], c.pick(readTS(es[9:14])))
	if flags == 3 && int(es[8]) >= 10 && off+19 <= tsPacketSize {
		writeTS(es[14:19], c.pick(readTS(es[14:19])))
	}
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
