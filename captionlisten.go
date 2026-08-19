package main

// Listening: pulling audio out of the stream, deciding where a phrase ends,
// and handing it to the recognizer.
//
// The segmenter is the part with the judgment in it — what counts as speech,
// where to cut, what to throw away — and every rule that discards audio says so
// in the log, because a silent discard is the hardest fault here to see.

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Listening
// ---------------------------------------------------------------------------

// captionEngine turns the transport stream into captions: ffmpeg decodes the
// audio, a voice activity check cuts it into phrases, and each phrase is
// recognized and handed to the CEA-608 encoder.
type captionEngine struct {
	// quirks is what this stream's model asks of the code around it; see the
	// "What a model needs from us" section. Everything model-specific arrives
	// through here and nowhere else.
	quirks  modelQuirks
	enc     *cea608
	label   string
	cfg     captionConfig
	model   recognizer
	ffmpeg  *exec.Cmd
	audioIn io.WriteCloser
	audioCh chan []byte
	closed  chan struct{}
	once    sync.Once
	// mu guards ffmpeg and audioIn, which are replaced when the decoder is
	// restarted underneath the goroutine writing to it.
	mu sync.Mutex
	// cfg and model2 are held until the first stream bytes arrive, when the
	// expensive part of starting up is finally allowed to happen.
	cfg2      captionConfig
	model2    captionModel
	startOnce sync.Once
	begun     int64
	// firstFeed is when the first stream bytes arrived, as unix nanoseconds.
	// The expensive start waits out captionSettle from that moment; see feed.
	firstFeed int64
	// doneOnce makes closing done idempotent. Three different paths finish an
	// engine — a failed start, the recognizer returning, and a stream that
	// closed before it ever began — and two of them could race: closing a
	// channel twice is a panic, a panic in a goroutine takes the whole process
	// with it, and this process is every tuner somebody is watching.
	doneOnce sync.Once
	// ready is set once the decoder is running and there is something to feed.
	ready int64

	// done is closed when the listening goroutine has returned. Nothing the
	// engine owns may be freed before then: the recognizer is native code, and
	// freeing a session out from under a call in flight is a crash, not an
	// error.
	done chan struct{}
	// streaming is set when the chosen model transcribes continuously; the
	// phrase segmenter is not used in that case.
	streaming bool
	// phrases carries cut phrases from the reader to the recognizer, so that a
	// slow model never stops the audio being read. It is small on purpose: a
	// phrase is a couple of hundred kilobytes, and a recognizer far enough
	// behind to fill this is not going to catch up by being given more room.
	// phrases carries cut phrases from the reader to the recognizer. dropped is
	// written by the reader and read by the recognizer, so it is atomic.
	phrases chan phraseItem
	dropped int64
	// skippedStale counts phrases thrown away for being old. Live captions
	// describe now or say nothing: a pipeline that has fallen behind must thin
	// out and catch up, never serve the past in order. Only recognize touches it.
	skippedStale int64
	// gated counts stretches the noise gate held back: loud enough to pass a
	// level test, too steady to be somebody talking.
	gated int64
	// tooShort counts stretches passed over for having too little speech in
	// them, and audioLost counts reads dropped because the decoder was behind.
	// Both used to happen in silence.
	tooShort  int64
	audioLost int64
	// recogLost counts chunks the recognizer was too far behind to be given,
	// and empty counts phrases the model was given and answered nothing for.
	recogLost int64
	empty     int64
	// lastAudio is when the decoder last produced a byte, as unix nanoseconds.
	// Watched by a goroutine that kills a decoder which has gone quiet.
	lastAudio int64
	// slow counts phrases that took longer to recognize than they were to say.
	// Only the recognizer goroutine touches it.
	slow int
	// tail is the end of the phrase last shown. A forced cut carries a moment
	// of audio forward so it does not slice through a word, and that moment is
	// then recognized twice, so the repeat is trimmed against this.
	tail []string
}

// newCaptionEngine returns immediately and finishes starting in the background.
//
// It must return immediately. This is called on the tune path, so anything slow
// here is time the viewer spends looking at nothing: loading a large model
// takes longer than the tune is allowed to take, and doing it here meant the
// tune timed out before a single frame of video was delivered. Captions are a
// convenience and the picture is not, so the stream is never held up for them.
//
// The audio decoder is not started until the model is ready either. Nothing
// would be draining it in the meantime, and an ffmpeg whose output nobody reads
// blocks, stops reading its own input, and ends up being fed a corrupted
// stream. Audio offered before then is dropped, which costs the first few
// seconds of captions on a cold start and nothing at all on a warm one.
func newCaptionEngine(cfg captionConfig, m captionModel, label string) (*captionEngine, error) {
	e := &captionEngine{
		quirks:  quirksFor(m),
		enc:     newCEA608(cfg.Style, cfg.Uppercase, cfg.OnScreenSec, cfg.SpeedWPM, captionLag(m, cfg)),
		label:   label,
		cfg:     cfg,
		audioCh: make(chan []byte, 64),
		phrases: make(chan phraseItem, 3),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	// Deliberately not started here. Loading weights moves gigabytes through
	// memory, and doing that while the tune is still proving itself slows the
	// thing that actually matters — even on its own goroutine, the bandwidth is
	// shared. The load waits until the stream has been flowing for a while
	// (captionSettle in feed), which is when the tune has actually won rather
	// than merely begun.
	e.cfg2, e.model2 = cfg, m
	return e, nil
}

// finish marks the engine done, exactly once, whoever gets there first.
func (e *captionEngine) finish() {
	e.doneOnce.Do(func() { close(e.done) })
}

// begin starts the slow work, once, when the first stream bytes arrive.
func (e *captionEngine) begin() {
	e.startOnce.Do(func() {
		atomic.StoreInt64(&e.begun, 1)
		go e.start(e.cfg2, e.model2)
	})
}

// start does the slow part off the tune path.
// captionStarts admits one caption engine at a time.
//
// Three streams asked for at once start three tunes at once, and each one hands
// its engine a start as soon as its own gate opens. The first pays for
// everything — the backend scan, the device enumeration, the weights — and the
// other two pile onto it while the tuners that have not opened yet are still
// polling their boxes over adb for playback. The tune that is furthest behind
// is the one that gets starved, and it is the one with the least budget left.
//
// One at a time. Nothing is lost by it: the second and third were going to wait
// on the shared model anyway, and waiting outside the load is cheaper than
// waiting inside it. What changes is that they no longer wait *while* competing.
var captionStarts = make(chan struct{}, 1)

func (e *captionEngine) start(cfg captionConfig, m captionModel) {
	select {
	case captionStarts <- struct{}{}:
		defer func() { <-captionStarts }()
	case <-e.closed:
		return
	}
	// This runs in the background with native code below it; nothing it does
	// may take the process down. The stream it serves is already flowing.
	defer func() {
		if r := recover(); r != nil {
			logger("[CC] %s captions failed to start (%v); this tune runs without them", e.label, r)
			e.finish()
		}
	}()
	began := time.Now()
	// The stream may end while this is queued behind another load, and a model
	// loaded for a stream that has gone is pure waste — gigabytes and half a
	// minute of it.
	alive := func() bool {
		select {
		case <-e.closed:
			return false
		default:
			return true
		}
	}

	// Failing to start is not the end of it. The stream this serves runs for
	// hours, and most of the ways a start can fail — an engine still
	// downloading, a wedged load that a retry gets fresh, another stream
	// holding the load gate — are better in a minute than they are now. So the
	// reason is said plainly, and then it is tried again for as long as the
	// stream is alive, backing off so a genuinely broken setup logs a line
	// every couple of minutes rather than a scroll.
	var model recognizer
	backoff := 15 * time.Second
	for attempt := 1; ; attempt++ {
		var err error
		model, err = loadRecognizer(m, cfg, alive, e.closed)
		if err == nil {
			break
		}
		logger("[CC] %s captions have not started (attempt %d): %v — retrying in %s while the stream plays",
			e.label, attempt, err, backoff)
		select {
		case <-e.closed:
			e.finish()
			return
		case <-time.After(backoff):
		}
		// Capped low: what unblocks a failed start is usually an external
		// event — a driver finishing, the machine going quiet — and waiting
		// minutes to notice it means captions sitting out recoverable time.
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	select {
	case <-e.closed:
		// The stream ended while the weights were loading.
		model.Close()
		e.finish()
		return
	default:
	}

	pcm, err := e.startDecoder()
	if err != nil {
		logger("[CC] %s could not start the audio decoder: %v", e.label, err)
		model.Close()
		e.finish()
		return
	}

	e.mu.Lock()
	e.model = model
	e.mu.Unlock()

	if m.Streaming {
		// The same spelling the weights were loaded with.
		//
		// loadRecognizer corrects the language onto whatever locale the model
		// accepts, but it does that to its own copy of cfg — so the load got
		// en-US and this asked for en, which Nemotron rejects. It then fell
		// back to a phrase at a time, which uses the same segmenter and lands
		// with the same sentence latency, so the model that had just been
		// chosen for being continuous behaved exactly like the one it was
		// chosen over.
		if err := model.beginStream(modelLanguage(m, cfg.Language)); err != nil {
			logger("[CC] %s could not start continuous recognition (%v), falling back to phrase at a time", e.label, err)
		} else {
			e.streaming = true
		}
	}

	atomic.StoreInt64(&e.lastAudio, time.Now().UnixNano())
	go e.watchDecoder()
	go e.pumpAudio()
	// Only now is there anything on the other end of the queue.
	atomic.StoreInt64(&e.ready, 1)
	if e.streaming {
		// A streaming model is fed a tenth of a second at a time and returns in
		// a fraction of it, so reading and recognizing stay on one goroutine
		// there; it is the phrase-at-a-time path that can block for seconds.
		go e.listenStreaming(pcm)
	} else {
		go e.recognize()
		go e.listen(pcm)
	}
	mode := "phrase at a time"
	if e.streaming {
		mode = "continuous"
	}
	logger("[CC] %s captions on: %s, %s, %s, %s, ready in %s",
		e.label, m.Key, cfg.Language, mode, runningOn(currentEngineVariant()),
		time.Since(began).Round(time.Millisecond))
}

// loadRecognizer opens a model on whichever engine can run it.
func loadRecognizer(m captionModel, cfg captionConfig, alive func() bool, stop <-chan struct{}) (recognizer, error) {
	// Nothing below may fight a tune — and "below" includes more than the
	// weights: the engine's first initialization compiles the GPU backend's
	// shaders, an all-cores burst that is just as capable of starving a
	// young tune as the load is, and it runs before any load. So the quiet
	// gate stands in front of everything, and a machine that will not go
	// quiet fails this attempt fast; the caller retries at the next lull.
	if !waitTuneQuiet(30 * time.Second) {
		return nil, fmt.Errorf("a tune has been starting the whole wait; will retry at a quiet moment")
	}
	// Spell the language the way this model wants it before anything is loaded;
	// cfg is a copy, so the correction lives exactly as long as this model.
	cfg.Language = modelLanguage(m, cfg.Language)
	if !m.Streaming {
		// One shared copy serves every tuner; see txBatchService.
		variant, err := captionVariantFor(m)
		if err != nil {
			return nil, err
		}
		if err := initTranscribeDeadline(variant); err != nil {
			return nil, err
		}
		weights, err := filepath.Abs(modelPath(m))
		if err != nil {
			return nil, err
		}
		svc, err := acquireTxBatchService(weights, txBackend(variant), cfg, alive)
		if err != nil {
			return nil, err
		}
		return &batchClient{svc: svc, stop: stop}, nil
	}
	return loadTranscribe(modelPath(m), cfg, alive)
}

// runtimeOf is the engine a model needs. A catalog entry that names none is a
// Parakeet one, which is what every entry was before there was a second engine.
func runtimeOf(m captionModel) string {
	return rtTranscribe
}

// startDecoder launches ffmpeg and returns the pipe its audio comes out of.
//
// ffmpeg is already in the image and is only asked for the audio, so the video
// never passes through a codec: the caption bytes are the only change this
// feature makes to the stream.
//
// Probe and analysis are held to a second so a tune starts captioning quickly.
// They are not switched off: "nobuffer" and "low_delay" make ffmpeg emit silence
// for these encoders rather than audio, which shows up as captions that simply
// never appear.
func (e *captionEngine) startDecoder() (io.ReadCloser, error) {
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-probesize", "1000000", "-analyzeduration", "1000000",
		"-f", "mpegts", "-i", "pipe:0",
		"-vn", "-sn", "-dn",
		"-ac", "1", "-ar", strconv.Itoa(asrSampleRate), "-f", "s16le", "pipe:1")
	audioIn, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pcm, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// ffmpeg is already held to errors only, but live television produces a
	// steady trickle of them and five tuners produce five trickles. They go
	// through the log at a rate a person can read rather than straight to
	// stderr, where they buried everything else.
	cmd.Stderr = &decoderLog{label: e.label}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	e.mu.Lock()
	select {
	case <-e.closed:
		// Lost the race with Close, which has already taken the decoder it
		// knew about. Own this one rather than orphaning it.
		e.mu.Unlock()
		audioIn.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("captions are shutting down")
	default:
	}
	e.ffmpeg, e.audioIn = cmd, audioIn
	e.mu.Unlock()
	return pcm, nil
}

// decoderLog carries ffmpeg's stderr into the log, saying the first of a run of
// complaints and then how many followed rather than every one of them.
type decoderLog struct {
	label string
	mu    sync.Mutex
	seen  int
	last  time.Time
}

func (d *decoderLog) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen++
	if time.Since(d.last) < 30*time.Second {
		return len(p), nil
	}
	if d.seen > 1 {
		logger("[CC] %s audio decoder: %s (and %d more)", d.label, firstLine(line), d.seen-1)
	} else {
		logger("[CC] %s audio decoder: %s", d.label, firstLine(line))
	}
	d.seen, d.last = 0, time.Now()
	return len(p), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// restartDecoder brings the audio decoder back after it has stopped.
//
// It should not stop, and after the reader and the recognizer were separated it
// has no reason to. But when it did, captions were gone for the rest of the
// recording while the picture carried on perfectly, and nothing said so: a
// three hour capture lost its captions in the first ten minutes and looked
// fine. Coming back costs a second of speech; not coming back costs the lot.
func (e *captionEngine) restartDecoder(attempt int) (io.ReadCloser, bool) {
	select {
	case <-e.closed:
		return nil, false
	default:
	}
	if attempt > 5 {
		// Only consecutive rapid failures count: the caller clears the tally on
		// every successful read, so this is a decoder that will not start at
		// all rather than one that hiccuped six times over three hours.
		logger("[CC] %s the audio decoder failed %d times in a row; captions are off for this tune", e.label, attempt-1)
		return nil, false
	}
	e.mu.Lock()
	old, oldIn := e.ffmpeg, e.audioIn
	// Cleared while held, so Close cannot pick up the same command and Wait on
	// it concurrently: exec.Cmd.Wait is not safe to call twice.
	e.ffmpeg, e.audioIn = nil, nil
	e.mu.Unlock()
	if oldIn != nil {
		oldIn.Close()
	}
	if old != nil && old.Process != nil {
		old.Process.Kill()
		old.Wait()
	}
	// Checked again after the kill: Close may have run while this was working,
	// and starting a decoder afterwards leaves an ffmpeg nobody owns and a
	// reader blocked on it forever.
	select {
	case <-e.closed:
		return nil, false
	default:
	}
	pcm, err := e.startDecoder()
	if err != nil {
		logger("[CC] %s could not restart the audio decoder: %v", e.label, err)
		return nil, false
	}
	logger("[CC] %s the audio decoder stopped and was restarted (attempt %d); captions resume shortly", e.label, attempt)
	return pcm, true
}

// watchDecoder kills an audio decoder that has stopped producing.
//
// A decoder that dies is noticed by the read failing. A decoder that stays
// alive and stops emitting is not noticed at all, and that is the worse
// failure: ffmpeg keeps swallowing its input and never exits, the read blocks
// for ever, and everything that would have reported the problem is downstream
// of that read. No phrase is cut, so nothing is dropped and nothing is counted;
// the minute-with-no-speech report never runs; the recognizer is idle rather
// than stuck so its deadline never fires. Captions stop dead, the picture
// carries on perfectly, and the log says nothing at all. It is the one way this
// can fail in total silence, which makes it worth a goroutine of its own.
//
// Decoded silence is still bytes — a quiet channel produces zeroes at 32 kB a
// second — so no bytes at all is unambiguous. Killing the process makes the
// blocked read fail, which is the path that already knows how to recover.
func (e *captionEngine) watchDecoder() {
	const (
		check = 5 * time.Second
		// Long enough that a slow encoder handover is not mistaken for death.
		silence = 15 * time.Second
	)
	t := time.NewTicker(check)
	defer t.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-t.C:
		}
		last := atomic.LoadInt64(&e.lastAudio)
		if last == 0 || time.Since(time.Unix(0, last)) < silence {
			continue
		}
		e.mu.Lock()
		cmd := e.ffmpeg
		e.mu.Unlock()
		if cmd == nil || cmd.Process == nil {
			continue
		}
		logger("[CC] %s no audio decoded for %s; restarting the decoder", e.label, silence)
		// Reset first, so the restart is given its own full window rather than
		// being killed again on the next tick.
		atomic.StoreInt64(&e.lastAudio, time.Now().UnixNano())
		cmd.Process.Kill()
	}
}

// pumpAudio hands buffered transport stream bytes to ffmpeg. Writes here must
// never block the video path, so a slow recognizer loses audio rather than
// stalling the DVR.
func (e *captionEngine) pumpAudio() {
	for {
		select {
		case <-e.closed:
			return
		case b, ok := <-e.audioCh:
			if !ok {
				return
			}
			e.mu.Lock()
			w := e.audioIn
			e.mu.Unlock()
			if w == nil {
				continue
			}
			// A write that fails means the decoder has gone. The reader notices
			// the same thing and restarts it, so this keeps going rather than
			// returning and leaving the replacement with nothing to read.
			if _, err := w.Write(b); err != nil {
				continue
			}
		}
	}
}

// Voice activity settings.
//
// Live speech rarely leaves a clean half second of silence: on a newscast the
// talking is continuous, so a segmenter that waits for a real pause never fires
// and every phrase ends up cut at the ceiling instead. That ceiling then
// becomes the caption delay, because nothing can be recognized until the audio
// is complete.
//
// So there are three ways a phrase can end. A real pause closes it at any
// length. Past vadMinPhrase, the short gap between two words is enough, which
// is what normally fires during continuous speech and keeps the cut off the
// middle of a word. The hard ceiling is only a backstop.
const (
	vadFrame = asrSampleRate / 50 // 20 ms
	// Ignore blips shorter than this — but a word is not a blip.
	//
	// Six tenths of a second was set when a duration test was the only thing
	// standing between the model and a stretch of room tone, and it was doing
	// two jobs: telling noise from speech, and telling a blip from a word. It
	// was too blunt for the second. "Yes", "No", "Right", "Thanks", a name
	// called across a room — these are three to five tenths of a second of
	// speech, and every one of them was thrown away whole, which the log now
	// says out loud and which is where the last of the missing words were
	// going.
	//
	// The shape test does the first job properly now: room tone is flat and
	// speech is not, whatever its length. So this is free to be what its name
	// says, a floor under blips rather than a floor under short answers.
	//
	// A quarter of a second, because the paragraph above names the range it is
	// protecting and thirty five hundredths cut into the bottom of it. Three to
	// five tenths is what a one-word reply measures, so a floor at thirty five
	// hundredths keeps the long half of that range and throws away the short
	// half — the shortest "Yes" is exactly the one it was meant to save, and it
	// went into the count this line prints.
	//
	// Nothing is riskier for it. What refuses a door closing is the shape test,
	// not the length: a door is one transient and flat across it, and it fails
	// the crest bar at any duration. This is left only to refuse the clicks and
	// edge effects that are too brief to have a shape worth measuring.
	vadMinSpeech = 0.25
	// The floor under the remainder of a phrase this code cut itself.
	//
	// The blip floor above assumes the stretch arrived on its own. The
	// remainder of a gap cut did not: a phrase that runs long is closed at the
	// widest gap between two words, so whatever follows is the rest of a
	// sentence, and when the speaker stops shortly after it can be a single
	// word. A word measures under the blip floor and was thrown away — "City
	// Strata Elite Card" arriving as "City Strata Elite" with the beat before
	// the last word being exactly where the cut landed.
	//
	// Rejecting a blip nobody asked for and discarding a fragment this code
	// made are not the same decision, so they no longer share a number. This
	// one is a floor under transients only: a stop consonant's closure runs to
	// about a hundred and fifty milliseconds, so below that there is no
	// syllable to transcribe.
	vadMinTail   = 0.15
	vadMinPhrase = 1.8 // past this, a word gap is enough to cut
	// The gap between two spoken words — and it has to be a gap between words
	// rather than a gap inside one.
	//
	// A fifth of a second sounds like a pause and is not. The closure of a stop
	// consonant — the silence before the release of a p, t, k, b, d or g — runs
	// from about fifty to a hundred and fifty milliseconds, which is to say a
	// hundred and fifty is inside a word, not between two. An actual pause
	// between words in fluent speech runs two hundred milliseconds and up.
	//
	// Cutting at a hundred and fifty therefore cut inside words, and did it
	// from two seconds into every phrase. What came out was fragments: the
	// model handed the front half of a word and then the back half of it as a
	// separate phrase, writing a plausible word for each and the right one for
	// neither. Whole short fragments then fell under the speech minimum and
	// were passed over entirely, so words went missing with nothing dropped and
	// nothing in the log. That is the stutter and the missing words, and they
	// are the same fault.
	//
	// A quarter of a second is above the closures and below a real pause.
	vadWordGap   = 0.25
	vadSilence   = 0.45 // a real pause: end the phrase whatever its length
	vadMaxPhrase = 3.5  // fallback only; every model in the catalog sets its own
	vadLead      = 0.35 // audio kept before speech, so words are not clipped

	// The bar for "somebody is talking" is three times an adaptive noise floor,
	// and left to itself that arrangement can talk the detector deaf.
	//
	// The floor rises on every frame that is not already over the bar, and the
	// bar is derived from the floor, so the two feed each other. On a stretch
	// of audio that sits just under the bar — an advert with a music bed is the
	// reliable way to find it — the floor climbs, the bar climbs with it, more
	// frames fall under the bar, and the floor climbs faster. Measured: a bed
	// at 0.0149 settles the bar at 0.0447, which is above ordinary dialogue.
	// Nothing is then loud enough to start a phrase, nothing is ever handed to
	// the recognizer, and captions stop dead until something quiet enough comes
	// along to let the floor decay again. It looks exactly like the recognizer
	// hanging. It is the opposite: the recognizer is idle and starving.
	//
	// Capping the floor was not enough. It only moved the bar from 0.0447 to
	// 0.03, and simulating the arithmetic showed the latch intact: a channel
	// whose speech sits between 0.015 and 0.018 still drives the bar to its cap
	// and then hears nothing, permanently, because recovery needs the audio to
	// go quieter than broadcast ever goes.
	//
	// The bar is therefore also held below a fraction of the loudest thing
	// heard recently. That is what breaks the ratchet rather than narrowing it:
	// whatever the loudest audio on this channel is, it stays audible, because
	// the bar is defined partly by it instead of only by the noise underneath
	// it. A floor is kept so that a genuinely silent channel is not treated as
	// wall-to-wall speech.
	//
	// It fails toward hearing. A noisy channel gets its noise transcribed,
	// which is untidy and was the behavior before any of this; the alternative
	// is captions that stop, which is the bug.
	vadFloorMax = 0.01
	vadBarMax   = 0.018
	// vadBarMin was an absolute minimum, and an absolute minimum is a second
	// way to be deaf: a channel mastered quietly — low encoder gain, a ducked
	// source — never reaches it, and no amount of adapting helps because it is
	// a constant. Simulated, dialogue at 0.011 produced nothing in ten minutes
	// while 0.013 produced 222 phrases. The floor now follows the channel like
	// the rest of the detector, and only refuses to go below the level of
	// genuine digital near-silence, which is what it was really for.
	vadBarSilence = 0.002
	// vadPeakShare is how far below the recent peak the bar is held.
	vadPeakShare = 0.5
	// vadPeakDecay lets the peak fall by about half over thirty seconds, so a
	// loud passage does not keep the bar high long after it has ended.
	vadPeakDecay = 0.99954

	// framesPerMinute is how many voice activity frames make up a minute, used
	// only for the "audio but no speech" report.
	framesPerMinute = 60 * asrSampleRate / vadFrame
)

// phraseIsSpeech is the noise gate the model's own documentation asks for.
//
// The card is explicit about how this model fails: "Cohere Transcribe is eager
// to transcribe, even non-speech sounds. The model thus benefits from
// prepending a noise gate or VAD in order to prevent low-volume, floor noise
// from turning into hallucinations." That is the thank-yous — the model is not
// mishearing anything, it is being handed a stretch of room tone and asked what
// was said in it, and it answers, because answering is what it does.
//
// There is nothing on the engine's side to lean on. Its run parameters carry a
// task, a language, punctuation and inverse text normalization, and that is
// all: no no-speech threshold, no blank suppression, no confidence a caller
// could threshold on. The header says so and the README says nothing at all.
// So the gate is here or it is nowhere.
//
// A level test alone cannot do it, because the bar follows the noise floor
// down — that is what lets it hear quiet dialogue — and a hiss above a very
// low floor passes exactly like speech does. What separates them is not how
// loud they are but how much they vary. Speech is syllables: energy swinging
// hard between vowels and stops, several times a second. Room tone, rain, a
// fan, the hum of an empty studio are all steady by definition, and their
// loudest moment sits close to their average one.
//
// So the test is the ratio between the two. Two is conservative — ordinary
// speech runs well above it, and a stretch that flat is not a sentence anybody
// said.
// The floor a phrase's peak must clear over its own average to count as
// speech, and the length below which that test is allowed to decide anything.
//
// The test has been wrong twice in the same direction — first by measuring the
// wrong frames, then by being set too high for real audio — and both times the
// cost was captions rather than hallucinations. So it no longer rules on its
// own. It applies only to phrases with barely any speech in them, which is
// what a stretch of room tone that tripped the level bar looks like, and a
// phrase carrying a sentence's worth of speech goes to the model whatever
// shape it is.
//
// Two weak signals rather than one strong one, deliberately. Neither flatness
// nor brevity is damning alone: a quiet speaker under a music bed is flat, and
// a short answer is brief. Together they describe the thing the model's card
// warns about and very little else, and being wrong now costs half a second
// rather than an evening.
const (
	vadCrestMin  = 1.3
	vadGateBelow = 1.2
)

// phraseCrest is how far the loudest moment of a phrase stands above its
// average, and whether that is enough to be speech.
func phraseCrest(loudest, levelSum float64, n int) (float64, bool) {
	if n == 0 || levelSum <= 0 {
		return 0, false
	}
	crest := loudest / (levelSum / float64(n))
	return crest, crest >= vadCrestMin
}

// plural picks the right word for a count, because "1 stretches" is the sort of
// thing that makes a log look machine-written.
// plural2 is plural for a count that is already being printed, so it names the
// thing without repeating the number.
func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// supportedToggles narrows what a model was asked for to what it will actually
// take, by asking it.
//
// The engine's rule for an unsupported toggle is to warn and carry on with its
// own default, which sounds harmless and is not: it warns per call, so a model
// that does not expose punctuation control produced two lines of log for every
// phrase of every stream, for ever. The header names the pre-check in the same
// sentence as the warning, and this is it.
//
// Asked and not assumed, because which families expose which toggle is not
// written down anywhere and changes with the engine. A model that gains the
// control later gets it without anybody editing a table.
func supportedToggles(model uintptr, q modelQuirks) (pnc, itn int32) {
	if txModelSupports == nil {
		return txPNCDefault, txITNDefault
	}
	if q.PNC != txPNCDefault && txModelSupports(model, txFeaturePNC) {
		pnc = q.PNC
	}
	if q.ITN != txITNDefault && txModelSupports(model, txFeatureITN) {
		itn = q.ITN
	}
	return pnc, itn
}

// heldBackAsNoise decides whether a phrase is a stretch of room tone rather
// than something somebody said.
//
// It is a function so that it can be tested, and it is tested because this rule
// has been wrong twice and the second time it was wrong silently: the
// restriction below was written into a commit message and never into the code,
// so for a while the gate ran on every phrase of every model. A whole
// advertisement came back with no captions at all, which is what that looks
// like from a sofa — a music bed is flat, every phrase under it measured flat,
// and every one of them was thrown away.
//
// Three things have to agree before anything is discarded. The model has to be
// one that hallucinates on silence, because this exists for that and no other
// reason. The audio has to be flat, which is what room tone is and speech is
// not. And there has to be barely any speech in it, because a phrase with a
// sentence's worth of talking in it is not room tone whatever its shape.
//
// And one thing on its own overrules all three: where the stretch came from.
//
// Flat and brief describes room tone that tripped the level bar, and it also
// describes the last word of a sentence. A single word has no variation in it
// to measure, so it reads flat, and it is under a second, so it reads brief —
// two weak signals that were meant to be independent, both firing on the same
// harmless thing. This gate only ever runs on the one model in the catalog that
// hallucinates on silence, which is why the missing word was only ever seen on
// that model.
//
// A phrase closed at a word gap was closed with the speaker still going, so the
// next stretch is the rest of that sentence: the bar was already tripped by
// speech, and this code is the reason the remainder is short and alone. Judging
// it as though it had arrived out of a quiet room is judging it on a fact this
// code invented.
func heldBackAsNoise(q modelQuirks, isSpeech bool, speechLen float64, tailOfSplit bool) bool {
	if tailOfSplit {
		return false
	}
	return q.NoiseGate && !isSpeech && speechLen < vadGateBelow
}

// vadCutSearch is how far back a forced cut looks for somewhere better to land.
// Long enough to contain a gap between words at any ordinary speaking rate,
// short enough that the phrase does not lose a meaningful amount of its end.
const vadCutSearch = 0.7

// tooShortToSend says whether a stretch of speech is too brief to be worth
// transcribing, and it needs to know where the stretch came from.
//
// A stretch that arrived on its own gets the blip floor: below that it is a
// door, a click, a syllable of a jingle, and the model will write words for it.
// The remainder of a cut this code made gets the transient floor instead,
// because it is known to be speech — it is the back half of a sentence the
// segmenter split, and the only reason it is short is that the speaker stopped.
//
// Both floors are still floors. A tail of nothing is still nothing.
func tooShortToSend(speechLen float64, tailOfSplit bool) bool {
	if tailOfSplit {
		return speechLen < vadMinTail
	}
	return speechLen < vadMinSpeech
}

// What the recognizer knows about itself, published for the segmenter that
// feeds it. Written at each dispatch, read when a phrase is being closed, and
// nothing else depends on them — a stale value costs one phrase.
//
// recogPressure is how many phrases were waiting. phraseCost is how long one
// phrase took, in nanoseconds of compute. phraseStreams is how many streams
// were sharing the recognizer.
var (
	recogPressure atomic.Int64
	phraseCost    atomic.Int64
	phraseStreams atomic.Int64
)

// ccComputeBudget is the share of the recognizer the segmenter will plan to
// use. Half, so that the other half absorbs what planning cannot: phrases do
// not arrive evenly, the encoder runs serially across a batch, and a machine
// doing this is also doing everything else it was already doing.
const ccComputeBudget = 0.5

// phraseCutAt is how much of the phrase window must pass before a gap between
// two words is enough to end a phrase, worked out from what the machine is
// actually doing rather than fixed.
//
// Cutting early is what keeps captions close to the sound. A phrase is not
// transcribed until it is closed, so its first word waits however long the
// phrase ran — close at one second and the caption is a second behind the
// sentence, close at four and it is four. Every stream should therefore cut as
// early as the machine it is running on can afford.
//
// What it costs is one encoder run per phrase, and that cost is per phrase
// rather than per second of audio: this family's encoder takes about the same
// time for a short phrase as a long one, which its own log prints. So halving
// the phrase length doubles the work for the same television, and the shortest
// phrase a machine can sustain is the one where the work still fits.
//
//	one stream produces          1 / interval phrases a second
//	n streams produce            n / interval
//	each costs                   the measured compute of one phrase
//	so the work is              (n / interval) * cost, and that must fit
//	  giving                     interval >= n * cost / budget
//
// Every term is measured on the machine it is running on: the cost of a phrase
// and the number of streams come from the recognizer's own dispatches, and the
// budget is the share of it this will plan to use. A fast model on a fast
// machine gets short phrases and captions close to the sound; a slow one on a
// busy machine gets long ones and captions that arrive rather than pile up. The
// same code does both without being told which it is on.
//
// Two floors, and which applies depends on whether this model is handed context
// in front of its phrases. Without context the floor is the shortest phrase the
// model can work from, because the phrase is all it gets. With context what the
// model sees does not shrink when the phrase does, so the only floor left is
// the shortest caption the display will hold converted back into speech —
// anything shorter reaches the screen as part of the next caption anyway.
//
// Capped at the window, because above it there is no phrase left to lengthen.
// Until the recognizer has run enough to have measured anything, the floor is
// the answer: that is the fast case, and being wrong about it costs one phrase
// of extra work rather than a caption.
func phraseCutAt(window float64, context bool) float64 {
	if window <= 0 {
		return 1
	}
	interval := vadMinPhrase
	if context {
		interval = ccMinOnScreen(1).Seconds()
	}
	cost := time.Duration(phraseCost.Load()).Seconds()
	streams := float64(phraseStreams.Load())
	if cost > 0 && streams > 0 {
		if need := streams * cost / ccComputeBudget; need > interval {
			interval = need
		}
	}
	// A queue that is not draining is the measurement being wrong about this
	// machine right now — something else is using it, or the phrases are longer
	// than the ones that were measured. Take the whole window until it clears.
	if recogPressure.Load() > 0 {
		interval = window
	}
	if interval > window {
		interval = window
	}
	return interval / window
}

// quietestCut finds the best place to end a phrase that has run out of time,
// and returns the sample index to cut at, or zero if there is nowhere better
// than where it already is.
//
// It looks over the last stretch of audio a frame at a time and takes the
// quietest one. A gap between words is the quietest thing in running speech, so
// that is usually what it finds. It insists the dip is a real one — half the
// average of the stretch it searched — because a level passage has a quietest
// frame too, and cutting at it would be no better than cutting at the end.
func quietestCut(audio []float32) int {
	span := int(vadCutSearch * asrSampleRate)
	if len(audio) < span+vadFrame {
		return 0
	}
	start := len(audio) - span
	best, bestRMS, sum, n := 0, math.MaxFloat64, 0.0, 0
	for i := start; i+vadFrame <= len(audio); i += vadFrame {
		var acc float64
		for _, v := range audio[i : i+vadFrame] {
			acc += float64(v) * float64(v)
		}
		rms := math.Sqrt(acc / float64(vadFrame))
		sum, n = sum+rms, n+1
		if rms < bestRMS {
			best, bestRMS = i, rms
		}
	}
	// The dip only has to be a dip, not a chasm.
	//
	// Half the average was the first attempt and it works on a newscast, where
	// a gap between words is many times quieter than the words. It fails on a
	// commercial, which is the case that needed it most: advertising is
	// compressed and limited to sit at one loudness, usually over a music bed
	// that never stops, so nothing in it is half of anything. Finding no dip,
	// this gave up and returned to cutting at whatever instant the timer named
	// — mid-word, in the middle of the fastest and most crowded speech on
	// television.
	//
	// Nine tenths accepts the quietest moment in a flattened mix, which is
	// still the best place available and still likelier to be a word boundary
	// than a point chosen by a clock. The only thing rejected now is audio with
	// no variation at all, where one instant genuinely is as good as another.
	if n == 0 || best == 0 || bestRMS > (sum/float64(n))*0.9 {
		return 0
	}
	// Cut after the quiet frame, so the silence belongs to the phrase ending
	// rather than opening the next one.
	return best + vadFrame
}

// vadBar is the level at which audio counts as somebody talking. It follows the
// noise floor upwards, but only so far: see vadBarMax for why the ceiling
// matters more than the adaptation does.
func vadBar(floor, peak float64) float64 {
	bar := math.Min(floor*3.0, vadBarMax)
	if p := peak * vadPeakShare; p < bar {
		bar = p
	}
	return math.Max(bar, vadBarSilence)
}

// vadPeak follows the loudest recent audio, falling slowly when it goes away.
func vadPeak(peak, rms float64) float64 {
	if rms > peak {
		return rms
	}
	return peak * vadPeakDecay
}

// listenStreaming feeds audio to a cache-aware streaming session and shows text
// the moment the model finalizes it.
//
// There is no phrase segmenter here and no waiting: the model returns words as
// the audio arrives and marks where an utterance ends, which is what keeps this
// about a second behind instead of a phrase behind.
// streamLagSec is how much audio may wait for the recognizer, in seconds.
//
// Seconds and not a number of feeds. It was thirty feeds, which was three
// seconds when a feed was a tenth of one and became half a minute the moment
// the feed became the size the model actually wants — so a recognizer that
// could not keep up stopped skipping and started sinking instead, half a minute
// deep, with nothing in the log because nothing was being dropped.
//
// A queue is only ever lag. What it can do is absorb the moment a model waits
// its turn; what it must not do is let a shortfall accumulate, because past a
// few seconds a caption is not late, it is about something else. Three seconds
// is roughly the shortfall a busy machine produces in bursts, and past it the
// audio is passed over and said so.
const streamLagSec = 3.0

// streamFeedMax bounds a coalesced feed, in samples.
//
// It has to be larger than one chunk or it coalesces nothing, which is what it
// did the moment the chunk grew: a bound of one second against a chunk of one
// and a bit meant every batch was already over it before a second was
// considered. Four seconds is the whole queue in one call, which is the point
// of coalescing, and short enough not to threaten the call's own deadline.
const streamFeedMax = 4 * asrSampleRate

// streamChunkDefault is the feed size for a streaming model that has not named
// one. A second, which is the order of every cache-aware encoder's context, and
// far enough from the old tenth to be obvious in a log.
const streamChunkDefault = 1.0

// streamJob is one thing for the recognizer to do, in the order it was asked.
// Feeds carry audio; the other two carry none and are told apart by finish,
// which ends the session rather than the utterance.
type streamJob struct {
	pcm    []float32
	finish bool
}

func (j streamJob) run(e *captionEngine) *streamResult {
	if j.finish {
		return e.model.finishStream()
	}
	return e.model.idleFlush()
}

func (e *captionEngine) listenStreaming(pcm io.ReadCloser) {
	defer e.finish()
	defer pcm.Close()

	// How much audio goes in per feed, which the model decides and not this.
	//
	// This was a tenth of a second, chosen on the reasoning that it is short
	// enough that nothing waits on a buffer and long enough that the call
	// overhead is irrelevant next to the work inside. The second half of that
	// was wrong for the models it was feeding.
	//
	// A cache-aware streaming encoder runs a forward pass over its attention
	// context on every feed, so the cost is per call rather than per second of
	// audio, and a tenth of a second was buying a tenth of the throughput. Nor
	// did it buy any latency back: the family's default lookahead is 1040 ms,
	// so it cannot commit a word ahead of that however small the pieces are.
	// Ten times the work for nothing, and with several tuners captioned at once
	// it was fifty trips a second at the accelerator between them.
	chunkSec := e.quirks.StreamChunkSec
	if chunkSec <= 0 {
		chunkSec = streamChunkDefault
	}
	chunk := int(chunkSec * asrSampleRate)
	// How long the talking has to stop before the sentence is closed off. Short
	// enough that a natural pause ends the line while the pause is still
	// happening, long enough that the gap between two words never does.
	const flushSilence = 0.6
	raw := make([]byte, chunk*2)
	buf := make([]float32, chunk)
	floor := 0.005
	peak := 0.0
	quiet := 0.0
	// Nothing is pending before anyone has spoken, so the first silence has
	// nothing to flush.
	settled := true
	decoderTries := 0
	// uttered is how much audio the open utterance has consumed; lead holds
	// the last beats of quiet audio so new speech keeps its first word.
	uttered := 0.0
	var lead [][]float32

	take := func(r *streamResult) {
		if r == nil {
			return
		}
		text := r.words()
		eou := r.EOU != 0
		if text == "" && !eou {
			return
		}
		if text != "" {
			// A word break is only forced at the end of an utterance; anything
			// else is the middle of a sentence still being spoken.
			e.show(text, eou)
		}
	}

	// Reading the audio and recognizing it are separate goroutines, and have to
	// be, for the reason written at length over the phrase path — which had
	// exactly this fault and was fixed there and not here.
	//
	// Every call into the model happened inline in this loop, so a model that
	// took a moment stopped draining ffmpeg's output for as long as it was
	// thinking. ffmpeg filled its pipe and blocked, a blocked ffmpeg stopped
	// reading its own input, and the transport stream bytes behind that were
	// dropped — which is not late captions, it is a corrupt stream, and it
	// reported itself as "dropped audio the decoder could not keep up with"
	// hundreds of times a minute. It needs no slow model to happen: three
	// streams sharing one graphics chip is enough, because the model waits its
	// turn for the chip inside the call.
	//
	// One channel rather than one per kind, because the order matters: a flush
	// that overtook the audio it was meant to close off would commit a sentence
	// that had not finished arriving.
	// The queue is a number of seconds, converted into feeds at this model's
	// chunk size rather than counted in feeds.
	depth := int(streamLagSec/chunkSec + 0.5)
	if depth < 2 {
		depth = 2
	}
	jobs := make(chan streamJob, depth)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// Audio that has piled up is fed in one call rather than ten.
		//
		// A hundred milliseconds a feed is the right size for one stream and
		// the wrong size for five. Every call is a trip out to the accelerator
		// — a slot to take, a command buffer to build, a submission, a wait —
		// and that cost is paid per call rather than per second of audio. Five
		// streams at ten calls a second is fifty trips a second for five
		// seconds of audio, and the overhead of the trip stops being irrelevant
		// next to the work inside it, which is the assumption the chunk size
		// was chosen under.
		//
		// So when this falls behind, the queue itself says by how much, and
		// everything waiting is concatenated into a single feed. Ten chunks
		// behind becomes one call instead of ten. It costs nothing when it is
		// keeping up, because then there is never more than one thing waiting —
		// the coalescing only happens when there is something to coalesce,
		// which is exactly when it is needed.
		//
		// Bounded, because a call's own deadline is fixed and an unbounded
		// batch is the one way to outgrow it.
		for job := range jobs {
			if job.pcm != nil {
				batch := job.pcm
				for len(batch) < streamFeedMax {
					select {
					case next := <-jobs:
						if next.pcm == nil {
							// A flush closes the utterance, so it must not
							// overtake the audio it is closing.
							take(e.model.feedStream(batch))
							batch = nil
							take(next.run(e))
						} else {
							batch = append(batch, next.pcm...)
						}
					default:
					}
					if batch == nil {
						break
					}
					if len(jobs) == 0 {
						break
					}
				}
				if batch != nil {
					take(e.model.feedStream(batch))
				}
				continue
			}
			take(job.run(e))
		}
	}()
	defer func() {
		close(jobs)
		<-drained
	}()

	// holed records that a chunk was dropped, so the utterance it belonged to
	// is closed off rather than spliced.
	//
	// This family re-attends over its whole accumulated audio, so audio with a
	// hole in it corrupts everything after the hole, and committed text cannot
	// be taken back off the screen. The same reasoning as the tune gate below,
	// for the same reason.
	holed := false
	post := func(job streamJob) {
		select {
		case jobs <- job:
		default:
			n := atomic.AddInt64(&e.recogLost, 1)
			switch {
			case n == 1:
				// Said once and said fully, because the count on its own does
				// not name a cause and this one has a specific one.
				//
				// A streaming model cannot be shared: every captioned tuner
				// runs its own copy, and each of those needs the accelerator
				// more or less continuously, because it recognizes as the audio
				// arrives rather than in bursts between phrases. Several at once
				// therefore want several times the device, and one graphics chip
				// does not have it however the work is scheduled — the card runs
				// them one at a time whatever is asked of it.
				//
				// A phrase model is the opposite: one copy serves every tuner
				// and the work arrives in batches with gaps between them, which
				// is why the same machine carries many more streams on Cohere
				// or Parakeet than on either Nemotron.
				logger("[CC] %s recognition is behind the audio. A model that transcribes as it listens runs a "+
					"separate copy for each captioned tuner and each one wants the accelerator continuously, so "+
					"several at once ask more of it than it has. Caption fewer tuners, or choose a model that "+
					"transcribes a phrase at a time — those share one copy between all of them.", e.label)
			case n%200 == 0:
				logger("[CC] %s recognition is still behind the audio; %s passed over so far", e.label, plural(n, "chunk", "chunks"))
			}
			holed = true
		}
	}
	feed := func(pcm []float32) {
		if holed {
			holed = false
			post(streamJob{})
		}
		post(streamJob{pcm: append([]float32(nil), pcm...)})
	}
	flush := func() { post(streamJob{}) }

	for {
		select {
		case <-e.closed:
			return
		default:
		}
		if _, err := io.ReadFull(pcm, raw); err != nil {
			post(streamJob{finish: true})
			next, ok := e.restartDecoder(decoderTries + 1)
			if !ok {
				return
			}
			decoderTries++
			pcm.Close()
			pcm = next
			// The continuous session has just been closed off, so open a new
			// one for the audio that is about to start arriving again.
			if err := e.model.beginStream(e.cfg.Language); err != nil {
				logger("[CC] %s could not resume continuous recognition: %v", e.label, err)
				return
			}
			quiet, settled, uttered = 0, true, 0
			continue
		}
		// See the phrase path: the budget counts consecutive failures.
		decoderTries = 0
		atomic.StoreInt64(&e.lastAudio, time.Now().UnixNano())
		var sum float64
		for i := range buf {
			v := float32(int16(uint16(raw[2*i])|uint16(raw[2*i+1])<<8)) / 32768.0
			buf[i] = v
			sum += float64(v) * float64(v)
		}
		// Continuous recognition yields to tunes like every other compute
		// here: while any tune has yet to deliver its video, this chunk is
		// read (so the decoder never blocks) and not recognized. The open
		// utterance is closed off first — this family re-attends over its
		// whole accumulated audio, so feeding it audio with a hole spliced
		// in corrupts everything after the hole, and the committed text a
		// bad splice provokes cannot be taken back off the screen. A channel
		// change costs this stream a sentence, never a corrupted one, and
		// never the other viewer's recording.
		if tunesPending() {
			if !settled {
				flush()
				quiet, settled, uttered = 0, true, 0
			}
			continue
		}
		rms := math.Sqrt(sum / float64(len(buf)))
		peak = vadPeak(peak, rms)
		loud := rms > vadBar(floor, peak)
		if !loud {
			floor = math.Min(0.995*floor+0.005*rms, vadFloorMax)
		}

		// Between utterances, only speech opens a new one. Feeding the model
		// whatever the channel is playing — music beds, crowd noise — sends
		// a small model rambling until the engine cuts the decode off, and
		// the transcript arrives truncated. The lead buffer keeps the last
		// beats of audio so the first word of new speech is not clipped;
		// gaps in what the model hears fall only between utterances, where
		// the reopened stream starts clean.
		if settled && !loud {
			lead = append(lead, append([]float32(nil), buf...))
			if len(lead) > 2 {
				lead = lead[1:]
			}
			continue
		}
		if settled {
			for _, l := range lead {
				feed(l)
				uttered += float64(len(l)) / asrSampleRate
			}
			lead = nil
			settled = false
		}
		feed(buf)
		uttered += chunkSec

		// An utterance cannot run forever: the model decodes each one against
		// a generation budget, and an utterance that outruns it comes back
		// truncated mid-sentence. Close it off at a brief lull once it is
		// eight seconds long, or at twelve seconds wherever the speech is —
		// a seam at a word beats a sentence that ends in nothing.
		if !settled && (uttered >= 12 || (uttered >= 8 && quiet >= 0.2)) {
			flush()
			quiet, settled, uttered = 0, true, 0
			continue
		}

		if loud {
			quiet = 0
			continue
		}
		if quiet += chunkSec; quiet >= flushSilence {
			flush()
			settled, uttered = true, 0
		}
	}
}

// show puts recognized text on screen.
func (e *captionEngine) show(text string, breakAfter bool) {
	e.write(text, breakAfter)
}

// write puts a phrase into the caption encoder.
func (e *captionEngine) write(text string, breakAfter bool) {
	text = respell(text, e.cfg.Spelling)
	e.enc.pushText(text, breakAfter)
}

// listen reads decoded audio, splits it into phrases and hands each one to the
// recognizer.
//
// Reading and recognizing are on separate goroutines, and have to be. They
// shared one until captions stopped for twenty seconds at a time, and the chain
// is worth spelling out because nothing about it is local: a model that took a
// while over a phrase stopped draining ffmpeg's output for exactly as long as
// it was thinking, ffmpeg filled its pipe and blocked, a blocked ffmpeg stopped
// reading its own input, and the transport stream bytes queued behind that were
// dropped by feed. Dropping bytes out of the middle of a transport stream does
// not cost a moment of audio, it corrupts the stream, and ffmpeg then emits
// nothing at all until it finds its way back in. So a model that was merely
// slow produced silence rather than late captions, and it landed mid-sentence
// because a long phrase is what triggered it.
//
// Reading never blocks now. When the recognizer cannot keep up the cost is a
// dropped phrase, which is a missing sentence rather than a broken stream.
func (e *captionEngine) listen(pcm io.ReadCloser) {
	defer pcm.Close()
	defer close(e.phrases)

	raw := make([]byte, vadFrame*2)
	var pending []float32
	speaking := false
	var silenceRun, speechLen float64
	// wasSplit records that the last phrase was closed while the speaker was
	// still going, so the next one is the rest of that sentence.
	var wasSplit bool
	// ctxLen is how many samples at the head of pending are carried context
	// rather than new audio. See the word-gap cut below.
	var ctxLen int
	// loudest and levelSum describe the shape of what is being heard, which is
	// how steady noise is told from speech; see phraseIsSpeech.
	var loudest, levelSum float64
	var levelN int
	floor := 0.005
	peak := 0.0
	decoderTries := 0
	var frames, cutThisMinute int
	// carryNext is set by a forced cut and consumed by the phrase after it.
	carryNext := false
	maxPhrase := phraseWindowFor(e.quirks, e.cfg)

	for {
		select {
		case <-e.closed:
			return
		default:
		}
		if _, err := io.ReadFull(pcm, raw); err != nil {
			next, ok := e.restartDecoder(decoderTries + 1)
			if !ok {
				return
			}
			decoderTries++
			pcm.Close()
			pcm = next
			pending, speaking, silenceRun, speechLen = nil, false, 0, 0
			loudest, levelSum, levelN = 0, 0, 0
			// The phrase that was owed a carry never arrived, so nothing the
			// next one says can be a repeat of it.
			carryNext = false
			continue
		}
		// Audio is flowing again, so the restart budget is for consecutive
		// failures rather than for the whole recording. A three hour capture
		// that hiccups twice an hour should recover every time.
		decoderTries = 0
		atomic.StoreInt64(&e.lastAudio, time.Now().UnixNano())

		// Say when audio is arriving but no speech is being cut out of it. It
		// is the one line that separates "the decoder died" from "the model
		// stopped answering", and it stays quiet while things are working.
		frames++
		if frames%framesPerMinute == 0 {
			if cutThisMinute == 0 {
				logger("[CC] %s a minute of audio with no speech found in it; resetting the level detector", e.label)
				// Whatever the detector has learned, it is not working. The
				// arithmetic above should make this unreachable; it is here
				// because captions that never come back is the failure this
				// file keeps being bitten by, and one minute of silence is a
				// cheap price for being certain it cannot last.
				floor, peak = 0.005, 0
			}
			cutThisMinute = 0
		}

		frame := make([]float32, vadFrame)
		var sum float64
		for i := range frame {
			v := float32(int16(uint16(raw[2*i])|uint16(raw[2*i+1])<<8)) / 32768.0
			frame[i] = v
			sum += float64(v) * float64(v)
		}
		rms := math.Sqrt(sum / float64(len(frame)))

		peak = vadPeak(peak, rms)
		loud := rms > vadBar(floor, peak)
		if !loud {
			floor = math.Min(0.995*floor+0.005*rms, vadFloorMax)
		}
		if loud {
			if !speaking {
				speaking = true
				speechLen = 0
				loudest, levelSum, levelN = 0, 0, 0
				// Everything held is kept. It used to trim back to the lead
				// at this moment, throwing away a third of a pre-roll it had
				// already collected, which is the one thing that cannot be
				// recovered later.
				//
				// This is where words go missing without anything being
				// dropped. A word does not start at the moment it crosses the
				// detector's bar. It starts with the part that never crosses
				// it: the s of seven, the th of thirty, the f of fifty, the
				// breath before a stressed syllable. Those run a fifth of a
				// second and more before the vowel arrives and the level test
				// notices anybody is talking. Cut the pre-roll to a fifth of a
				// second and the model is handed "even" and "irty", and what
				// it writes is a plausible word that is not the one that was
				// said — with nothing dropped anywhere, no counter moved and
				// nothing in the log, because the audio simply never contained
				// the beginning of the word.
			}
			silenceRun = 0
			speechLen += float64(vadFrame) / asrSampleRate
		} else {
			silenceRun += float64(vadFrame) / asrSampleRate
		}
		pending = append(pending, frame...)
		if speaking {
			// Every frame of the phrase, loud or quiet. The gaps between
			// syllables are the whole point: measuring only the frames that
			// already passed the bar measures a set selected for being level,
			// which is how this came to hold back ordinary speech.
			loudest = math.Max(loudest, rms)
			levelSum += rms
			levelN++
		}

		if !speaking {
			// Hold the lead-in window, and a little more than the lead, so the
			// onset is already in hand whenever speech is finally noticed.
			keep := int((vadLead + 0.15) * asrSampleRate)
			if len(pending) > keep {
				pending = pending[len(pending)-keep:]
			}
			continue
		}

		// The context carried in front of this phrase is not part of it: it has
		// been transcribed already and it is here so the model has something to
		// read into. Counting it would close every phrase a context early.
		phrase := float64(len(pending)-ctxLen) / asrSampleRate
		ended := silenceRun >= vadSilence
		// Long phrases are cheaper per second of television, so how long to let
		// one run is the same trade as a streaming model's lookahead and is
		// made with the same setting.
		//
		// How much of the window has to pass before a word gap will do is not
		// fixed, because what it buys changes with the load. See phraseCutAt.
		gapped := phrase >= maxPhrase*phraseCutAt(maxPhrase, e.quirks.ContextSec > 0) && silenceRun >= vadWordGap
		forced := phrase >= maxPhrase
		if !ended && !gapped && !forced {
			continue
		}

		audio, carried := pending, carryNext
		carryNext = false
		if forced {
			// Cut where the speaker drew breath, not where the timer expired.
			//
			// The backstop lands wherever four seconds happens to fall, and
			// four seconds of continuous speech usually falls inside a word.
			// Carrying a fifth of a second forward saves the word for the next
			// phrase but does nothing for this one, which still ends on a
			// fragment — and a fragment is not dropped, it is transcribed. Half
			// of "implants" came back as an "s" on the end of the word before
			// it, and nothing in the log had gone wrong.
			//
			// So the last stretch is searched for the quietest moment and the
			// cut is made there. A gap between words is the quietest thing in
			// running speech, so that is usually where it lands, and both
			// phrases then begin and end on whole words. Nothing is duplicated
			// either, because this splits the audio rather than overlapping it.
			if at := quietestCut(audio); at > 0 {
				pending = append([]float32(nil), audio[at:]...)
				audio = audio[:at]
			} else {
				// No dip to be found, which means speech straight through.
				// Fall back to overlapping, and to trimming the repeat.
				lead := int(vadLead * asrSampleRate)
				if len(audio) > lead {
					pending = append([]float32(nil), audio[len(audio)-lead:]...)
					carryNext = true
				} else {
					pending = nil
				}
			}
			ctxLen = len(pending)
			silenceRun = 0
		} else if gapped && !ended && e.quirks.ContextSec > 0 {
			// A gap between words is not the end of anything, so the next
			// phrase keeps what came before it.
			//
			// Cutting early is what puts captions close to the sound, and the
			// reason it could not be done freely is accuracy: a model handed a
			// second of audio has a second of sentence to work from, and an
			// encoder-decoder does badly on a fragment. That is an argument
			// about how much the model sees, not about how often this cuts —
			// and the two were the same thing only because each phrase was
			// handed over alone.
			//
			// So the tail is carried forward as context. The model sees the
			// preceding audio and the new audio together, trimOverlap takes the
			// repeated words off the front of what comes back, and the phrase
			// can be as short as the machine can afford without the model ever
			// seeing less than it needs.
			//
			// It is close to free, which is the measurement that makes it worth
			// doing: this family's encoder costs about the same for a long
			// phrase as a short one — its own log prints the figure and it
			// barely moves across the lengths in play — so context is paid for
			// in bytes rather than in time.
			ctx := int(e.quirks.ContextSec * asrSampleRate)
			if len(audio) < ctx {
				ctx = len(audio)
			}
			pending = append([]float32(nil), audio[len(audio)-ctx:]...)
			ctxLen = len(pending)
			carryNext = true
			silenceRun = 0
		} else {
			pending = nil
			ctxLen = 0
			speaking = false
		}
		// Whether the phrase being closed here is the remainder of a cut this
		// code made, and whether the next one will be.
		//
		// A phrase closed at a real pause is a finished utterance and what
		// follows it is a new one. A phrase closed at a word gap is not: speech
		// was still running, and this is the only close that hands the next
		// phrase nothing to start from, so that phrase can be a single word.
		//
		// The ceiling is deliberately not counted. It carries its audio forward
		// rather than dropping it, so the phrase after it starts with up to
		// seven tenths of a second already in hand and clears the floor on its
		// own. Counting it would matter on continuous speech, where the ceiling
		// is what fires and fires again: the flag would never clear, and the
		// noise gate would be off for the length of a newscast — which is the
		// content it exists for.
		tailOfSplit := wasSplit
		wasSplit = gapped && !ended
		// The next phrase starts with whatever was carried into it.
		//
		// A forced cut splits the audio exclusively — audio[:at] goes now and
		// audio[at:] waits — so the remainder is up to seven tenths of a second
		// of speech that has already been read and already been counted. Zeroing
		// the counters below then told the next phrase it was holding nothing.
		// If the speaker stopped shortly after the cut, that phrase measured
		// only the silence after it, fell under vadMinSpeech and was thrown away
		// as too short — taking a word that existed in no other phrase, because
		// the split gave it to this one and nowhere else.
		//
		// Intermittent by nature: it needs a forced cut, which is a minority of
		// phrases, and the speaker to stop within a breath of it.
		heldSec := float64(len(pending)-ctxLen) / asrSampleRate
		if heldSec < 0 {
			heldSec = 0
		}
		if tooShortToSend(speechLen, tailOfSplit) {
			// Counted, because it was not. Every other way audio is discarded
			// says so, and a path that throws away sound in silence is a place
			// words can go with nothing in the log to show for it.
			n := atomic.AddInt64(&e.tooShort, 1)
			if n == 1 || n%50 == 0 {
				logger("[CC] %s passed over %s too short to be worth transcribing (%d so far)",
					e.label, plural(n, "stretch", "stretches"), n)
			}
			speechLen, loudest, levelSum, levelN = heldSec, 0, 0, 0
			continue
		}
		if crest, ok := phraseCrest(loudest, levelSum, levelN); heldBackAsNoise(e.quirks, ok, speechLen, tailOfSplit) {
			n := atomic.AddInt64(&e.gated, 1)
			if n == 1 || n%25 == 0 {
				logger("[CC] %s held back %s of steady noise that was not speech (%.1fs of it, peak %.1f times the average, floor is %.1f)",
					e.label, plural(n, "stretch", "stretches"), speechLen, crest, vadCrestMin)
			}
			speechLen, loudest, levelSum, levelN = heldSec, 0, 0, 0
			continue
		}
		speechLen, loudest, levelSum, levelN = heldSec, 0, 0, 0
		cutThisMinute++
		e.queue(audio, carried, ended, forced && !ended && !gapped)
	}
}

// phraseItem is a cut phrase and the moment it was cut. The stamp is what
// keeps the pipeline honest: any stage may compare it against now and refuse
// to spend work on the past.
// span is how much audio this phrase carries, for a log line that would
// otherwise have to say "a stretch" and leave the reader guessing.
func (p phraseItem) span() string {
	return fmt.Sprintf("%.1fs", float64(len(p.pcm))/asrSampleRate)
}

type phraseItem struct {
	pcm []float32
	cut time.Time
	// carried marks a phrase that begins with audio held over from the
	// previous one. Only these can contain a repeated word, and only these
	// are trimmed for it; see trimOverlap.
	carried bool
	// atPause marks a phrase that ended because the speaker stopped, rather
	// than because the segmenter ran out of room. See trimFalseStop.
	atPause bool
	// hardCut marks a phrase the length ceiling ended, with no gap in the
	// audio at all. Only these are certainly mid-sentence.
	hardCut bool
}

// phraseStaleAfter is how old a phrase may be before it is abandoned. Normal
// passage through the pipeline is a second or two; anything past this is a
// backlog, and transcribing a backlog in order is how captions end up narrating
// television from the past. Six seconds is the re-sync guarantee: after a
// channel change shoves the queue behind, the ceiling trims the backlog
// within a phrase or two and the stream is current again, at the cost of a
// dropped sentence instead of permanent lag.
const phraseStaleAfter = 6 * time.Second

// queue hands a phrase to the recognizer, and never waits for it.
func (e *captionEngine) queue(audio []float32, carried, atPause, hardCut bool) {
	select {
	case e.phrases <- phraseItem{pcm: audio, cut: time.Now(), carried: carried, atPause: atPause, hardCut: hardCut}:
	default:
		// The recognizer is still working through what it has. Losing this
		// phrase costs a sentence; waiting here would cost the audio stream,
		// which is the trade that made captions stop altogether.
		n := atomic.AddInt64(&e.dropped, 1)
		if n == 1 || n%10 == 0 {
			logger("[CC] %s behind: %s dropped", e.label, plural(n, "phrase", "phrases"))
		}
	}
}

// recognize turns queued phrases into captions, one at a time.
//
// It owns the recognizer for the life of the engine, which is what lets Close
// wait for this goroutine and then free the model safely: no other goroutine
// ever calls into it on this path.
func (e *captionEngine) recognize() {
	defer e.finish()
	// Against the shared service, a stream keeps two phrases in flight and
	// shows the answers strictly in order. Submitting one and waiting was
	// this stream throttling itself to one phrase per service cycle — the
	// batch call exists to take many at once, and with five tuners cutting
	// phrases faster than one-per-cycle, every stream dropped phrases while
	// the recognizer itself had capacity to spare.
	type pendingPhrase struct {
		reply <-chan txBatchReply
		item  phraseItem
		sent  time.Time
		// giveUp is when to stop waiting on this one, carried per phrase so
		// the wait can be part of a select rather than a call that blocks.
		giveUp time.Time
	}
	async, canAsync := e.model.(interface {
		submit(pcm []float32) (<-chan txBatchReply, error)
	})
	var window []pendingPhrase
	// settleHead shows the oldest in-flight phrase's result; when block is
	// false it only does so if the result is already in.
	settleHead := func(block bool) bool {
		if len(window) == 0 {
			return false
		}
		p := window[0]
		var r txBatchReply
		if block {
			select {
			case r = <-p.reply:
			case <-e.closed:
				return false
			case <-time.After(txRunDeadline + 8*time.Second):
				window = window[1:]
				logger("[CC] %s the shared recognizer did not answer", e.label)
				return true
			}
		} else {
			select {
			case r = <-p.reply:
			default:
				return false
			}
		}
		window = window[1:]
		e.captionResult(p.item, r.text, r.err, time.Since(p.sent))
		return true
	}
	// A finished caption goes on screen the moment it is finished.
	//
	// This waited on the next phrase instead. Results were only ever collected
	// on the way past — a phrase arrived, whatever had come back since last
	// time was shown, and then the loop blocked on the channel again. So a
	// caption that the recognizer had ready in half a second sat there until
	// the speaker finished saying the next thing, which on continuous speech
	// is another two and three quarter seconds. It was the phrase rate setting
	// the delay, and the recognizer's speed barely entered into it: half a
	// second of compute, three seconds on screen.
	//
	// Waiting on both at once is the whole fix. Whichever happens first —
	// another phrase to send, or an answer to show — is dealt with when it
	// happens, and nothing is held for the sake of something unrelated.
	for {
		var in <-chan phraseItem
		if len(window) < maxInFlight {
			// The ceiling on phrases in flight, so the channel is only listened
			// to with room to accept. Not listening is the backpressure: it was
			// a blocking settle before, which is the same bound reached by
			// standing still instead of by waiting for the right thing.
			in = e.phrases
		}
		var head <-chan txBatchReply
		var lost <-chan time.Time
		if len(window) > 0 {
			head = window[0].reply
			lost = time.After(time.Until(window[0].giveUp))
		}
		select {
		case <-e.closed:
			return
		case r := <-head:
			p := window[0]
			window = window[1:]
			e.captionResult(p.item, r.text, r.err, time.Since(p.sent))
		case <-lost:
			// Being made to wait is not the same as having failed, and this
			// could not tell them apart. Inference stops dead while any tune
			// has yet to deliver its video, and a tune that never confirms
			// holds everything for the best part of a minute — comfortably
			// longer than the budget a phrase is given. So a burst of tuners
			// starting, which is exactly what a container start looks like,
			// declared every phrase in flight unanswered, threw away work the
			// recognizer had been told not to do, and said the recognizer was
			// at fault. What a viewer saw was captions collapsing on the
			// stream that was already playing whenever another one started.
			//
			// Time spent held on purpose does not count against the phrase.
			// The budget is for a recognizer that has wedged, and while a tune
			// is in flight there is nothing to conclude, so it starts again.
			// A phrase that does come back too late to be worth showing is
			// still discarded on arrival, by the freshness check that has
			// always been there for it.
			if tunesPending() {
				window[0].giveUp = time.Now().Add(txRunDeadline + 8*time.Second)
				continue
			}
			window = window[1:]
			logger("[CC] %s the shared recognizer did not answer", e.label)
		case item, ok := <-in:
			if !ok {
				// No more phrases coming: show what is still in flight and
				// finish. Nothing is abandoned that has already been paid for.
				for len(window) > 0 {
					if !settleHead(true) {
						return
					}
				}
				return
			}
			// Skip anything that went stale in the queue. Dropping the oldest
			// is what lets the newest stay current: the alternative is every
			// phrase arriving late by however far behind the recognizer once
			// fell.
			if age := time.Since(item.cut); age > phraseStaleAfter {
				n := atomic.AddInt64(&e.skippedStale, 1)
				if n == 1 || n%10 == 0 {
					logger("[CC] %s skipped a phrase %.0fs old to stay current (%d so far)", e.label, age.Seconds(), n)
				}
				continue
			}
			if !canAsync {
				e.caption(item)
				continue
			}
			reply, err := async.submit(item.pcm)
			if err != nil {
				// A full service is a dropped phrase; the falling-behind log
				// already reports the count.
				atomic.AddInt64(&e.dropped, 1)
				continue
			}
			if reply == nil {
				continue
			}
			now := time.Now()
			window = append(window, pendingPhrase{
				reply:  reply,
				item:   item,
				sent:   now,
				giveUp: now.Add(txRunDeadline + 8*time.Second),
			})
		}
	}
}

// caption recognizes one phrase and queues it for display.
func (e *captionEngine) caption(item phraseItem) {
	audio := item.pcm
	start := time.Now()
	text, err := e.model.transcribe(audio)
	e.captionResult(item, text, err, time.Since(start))
}

// captionResult accounts for one phrase's outcome and queues its text for
// display. Called on the recognize goroutine only, in phrase order.
func (e *captionEngine) captionResult(item phraseItem, text string, err error, took time.Duration) {
	if age := time.Since(item.cut); age > phraseStaleAfter+txRunDeadline {
		// It was fresh going in and ancient coming out: the recognizer stalled
		// underneath it. Showing it now would caption the past.
		logger("[CC] %s discarded a caption that took %.0fs to come back", e.label, age.Seconds())
		return
	}
	if err != nil {
		logger("[CC] %s recognition failed after %s: %v", e.label, took.Round(time.Millisecond), err)
		return
	}
	// What matters is what the viewer feels: how old this caption is as it
	// reaches the screen, measured from the moment the phrase was cut. The
	// alternative — wall time against speech length — reads healthy streams
	// as struggling ones, because phrases wait in a send window on purpose.
	// Two in flight means a
	// couple of seconds of age is the design working; real trouble is age
	// climbing past the freshness ceiling, or phrases being dropped, and
	// those are what get said.
	if lag := time.Since(item.cut); lag > 7*time.Second {
		e.slow++
		if e.slow == 1 || e.slow%20 == 0 {
			if drops := atomic.LoadInt64(&e.dropped); drops > 0 {
				logger("[CC] %s captions are running %.0fs behind (%s dropped)", e.label, lag.Seconds(), plural(drops, "phrase", "phrases"))
			} else {
				logger("[CC] %s captions are running %.0fs behind (nothing dropped)", e.label, lag.Seconds())
			}
		}
	}
	fromModel := len(strings.Fields(text))
	text = e.trimOverlap(text, item.carried)
	// Punctuation is only wrong where the ceiling cut with no gap at all.
	//
	// A word gap is a place the speaker paused, and if the model closed the
	// sentence off there it agrees. Stripping that was treating every cut this
	// code made as an interruption, which ran two sentences together
	// unpunctuated whenever somebody paused for less than vadSilence between
	// them.
	text = trimFalseStop(text, !item.hardCut)
	if text == "" {
		// Counted, because it was not.
		//
		// This is the one way audio reached the model and produced nothing at
		// all, and it returned silently — so a stretch of television with no
		// captions on it looked exactly like a stretch with nothing said. Every
		// other way a phrase is discarded says so out loud and this is the rule
		// that applies to all of them: audio can be thrown away, but never
		// without the log being able to say afterward that it was.
		//
		// Music is one way it happens: handed a sung passage an encoder-decoder
		// can emit its end token immediately and hand back an empty string,
		// which is the model declining rather than failing.
		//
		// The trimmers are the other way, and the line has to say which. Both
		// of them run before this and both can empty a phrase — the overlap
		// trimmer takes off words the previous phrase already showed, and it is
		// perfectly capable of taking off all of them. Blaming the model for
		// that would send somebody looking at the model.
		n := atomic.AddInt64(&e.empty, 1)
		if n == 1 || n%25 == 0 {
			logger("[CC] %s nothing to show for %s of audio (%s so far; the model wrote %d %s)",
				e.label, item.span(), plural(n, "stretch", "stretches"),
				fromModel, plural2(fromModel, "word", "words"))
		}
		return
	}
	if e.quirks.Suppress != nil && e.quirks.Suppress(text) {
		n := atomic.AddInt64(&e.gated, 1)
		if n == 1 || n%25 == 0 {
			logger("[CC] %s dropped %q; nothing was said (%d suppressed so far)", e.label, text, n)
		}
		return
	}
	// The line breaks where the speaker stopped, not where this code cut.
	//
	// It broke after every phrase. A phrase is two to four seconds, which is
	// five to ten words, so every row held one phrase and then ended — half a
	// line of a forty-two column window used, and a roll for each one whether
	// the line was full or not. Reported as captions not using the screen and
	// scrolling away faster than they can be read, which is both halves of the
	// same fault.
	//
	// atPause is already the answer and is already carried this far for
	// trimFalseStop, which strips the full stop the model puts on a fragment
	// this code cut mid-sentence. The same fragments should not end a line
	// either: they flow on, fill the width, and wrap where the window wraps.
	// A real pause ends the line, or a place the model closed the sentence off;
	// not where the length ceiling happened to fall.
	e.write(text, item.atPause || (!item.hardCut && endsSentence(text)))
}

// trimFalseStop removes a full stop the speaker did not make.
//
// A phrase model transcribes one cut phrase at a time and punctuates what it is
// given as though that were the whole utterance, because from where it sits that
// is exactly what it looks like. But only one of the three reasons this code cuts
// is the speaker finishing: a real pause of vadSilence or more. The other two —
// a quarter-second word gap once the phrase is long enough, and the hard ceiling
// at maxPhrase — land in the middle of a sentence by design, and the model ends
// the fragment with a full stop anyway. "The president said, and I quote" comes
// back as "The president said." followed by "And I quote." That is not a
// transcription error; it is punctuation applied to a boundary the speaker never
// made, and on screen it is the ugliest thing captions do.
//
// So a phrase that did not end at a pause gives up its closing punctuation. The
// comma that belongs there instead is not put in: the model did not offer one
// and inventing punctuation is a worse habit than omitting it. In roll-up caps,
// which is the default, running the two fragments together reads correctly.
//
// Every sentence-ending mark, not just the full stop.
//
// This took the full stop and deliberately left the question mark and the
// exclamation mark, on the reasoning that those are claims about a sentence's
// shape that a mid-sentence cut is unlikely to make by accident. That was a
// guess dressed up as a rule, and it was wrong: handed "and what about the"
// with nothing after it, a model will happily decide it was a question. With
// the full stops gone the stray question marks were all that was left, which is
// how they came to be noticed.
//
// The argument never depended on which mark it was. At a boundary this code
// invented, every terminal mark is punctuation applied to a fragment, because
// the speaker was still talking. So all three go.
//
// The cost is a real question that happens to end on a word gap rather than a
// pause, which loses its mark. That is the smaller error by a distance: a
// question without its mark reads as a sentence, while a question mark in the
// middle of one reads as a fault — which it is.
//
// An ellipsis is left alone. The model wrote three of them on purpose.
// endsSentence reports whether the model closed the phrase off.
//
// This is the only clause boundary available. Broadcast guidance is to break
// where speech would naturally pause and never inside a clause, and the model
// writing a full stop is it saying a clause ended — better evidence than a gap
// measured in the waveform, which cannot tell a breath from a full stop.
//
// An ellipsis is not an ending. The model writes it for speech that trails off,
// which is the middle of a thought rather than the end of one.
func endsSentence(text string) bool {
	// Closing punctuation of every shape, because a full stop inside a quotation
	// is still a full stop: he said "no." and he said \u201cno.\u201d end the same
	// sentence, and only one of them was being seen.
	t := strings.TrimRight(text, " \"')]\u2019\u201d")
	if t == "" || strings.HasSuffix(t, "...") || strings.HasSuffix(t, "\u2026") {
		return false
	}
	switch t[len(t)-1] {
	case '.', '?', '!':
		return true
	}
	return false
}

func trimFalseStop(text string, atPause bool) string {
	if atPause {
		return text
	}
	t := strings.TrimRight(text, " ")
	if t == "" || strings.HasSuffix(t, "..") {
		return text
	}
	switch t[len(t)-1] {
	case '.', '?', '!':
		return strings.TrimRight(t[:len(t)-1], " ")
	}
	return text
}

// trimOverlap drops the beginning of a phrase where it repeats the end of the
// one before.
//
// A phrase cut at the ceiling rather than at a pause carries a fraction of a
// second of audio into the next one, so that a word is not sliced in half. That
// audio is recognized in both, which put "and" twice across the join and turned
// "downtown" into "downtown" followed by "town". Up to four words of overlap
// are matched and removed.
// trimOverlap removes words this phrase repeats from the end of the last one.
//
// The repeat is not a habit of the model, it is something this code causes: a
// phrase cut at the four second backstop lands mid-word, so a fifth of a second
// of audio is carried into the next phrase to save the word — and that fifth of
// a second gets recognized twice.
//
// Which is why it now only runs on the phrases that carried something. It used
// to run on every phrase and trim on a single matching word, and a single
// matching word across a phrase boundary is not evidence of anything: speech is
// full of "and", "the", "so", "we", and a phrase ending on one followed by a
// phrase opening on one is a coincidence, not a duplicate. Every time that
// happened a real word was deleted. That is the missing word.
//
// A phrase cut at a silence or a word gap carries nothing forward and therefore
// cannot repeat anything, so there is nothing to look for and nothing to lose
// by not looking.
// maxOverlapWords is the most words that can be a repeat of the phrase before.
//
// Four was right when the only thing ever carried forward was a third of a
// second from a forced cut — one word, two at a push. A model carrying context
// in front of every phrase repeats seconds rather than fractions, and a trimmer
// capped below what is carried leaves the difference on screen twice, which is
// what carrying context without touching this produced.
//
// Derived from what this model carries, so the two cannot drift apart again:
// however much audio is in front of the phrase, this is how many words can be
// in it at the fastest speech this program will ever show.
func (e *captionEngine) maxOverlapWords() int {
	n := 4
	if e.quirks.ContextSec > 0 {
		if w := int(math.Ceil(e.quirks.ContextSec * ccMaxPace / ccCharsPerWord)); w > n {
			n = w
		}
	}
	return n
}

func (e *captionEngine) trimOverlap(text string, carried bool) string {
	words := strings.Fields(text)
	if len(words) == 0 || len(e.tail) == 0 || !carried {
		e.rememberTail(words)
		return strings.TrimSpace(text)
	}
	best := 0
	for n := min(len(e.tail), min(len(words), e.maxOverlapWords())); n > 0; n-- {
		match := true
		for i := 0; i < n; i++ {
			if !strings.EqualFold(strings.Trim(e.tail[len(e.tail)-n+i], ".,!?;:"),
				strings.Trim(words[i], ".,!?;:")) {
				match = false
				break
			}
		}
		if match {
			best = n
			break
		}
	}
	words = words[best:]
	e.rememberTail(words)
	return strings.Join(words, " ")
}

// rememberTail keeps the end of what was shown, so the next phrase can be
// checked against it for a repeat.
//
// It keeps as many words as the trimmer can match, and no fewer. Keeping four
// while the trimmer looked for six meant the trimmer could never find what it
// was looking for: a phrase carrying two seconds of context begins in the
// middle of the phrase before it, so the words it repeats are the last six of
// that phrase and a memory of four cannot contain them. Nothing matched, and
// every carried phrase put its context on screen a second time.
func (e *captionEngine) rememberTail(words []string) {
	if len(words) == 0 {
		return
	}
	if n := e.maxOverlapWords(); len(words) > n {
		words = words[len(words)-n:]
	}
	e.tail = append([]string(nil), words...)
}

// captionSettle is how long this stream must have been flowing before its
// caption engine starts up. One second of flow is proof, not a cushion: the
// protecting is done machine-wide by the state gate, which holds heavy work
// while any tune has yet to deliver video, so padding here only delays the
// first caption.
const captionSettle = 1 * time.Second

// The quiet gate holds heavy caption work while any tune is in its fragile
// stretch — and fragile is a state, not an age. A tune is fragile from the
// /play request until its video actually starts flowing to the DVR, however
// long its device takes to prove playback: one box confirms in three seconds
// and another takes its whole forty-second window, and a fixed twelve-second
// age test released the load straight into the slow box's window. After the
// video flows, a short grace covers the DVR settling onto the fresh stream.
const (
	// tuneSettledGrace: how old the newest tune must be once every pending
	// tune has its video flowing. A few seconds for the DVR to buffer the
	// fresh stream; the fragile stretch itself is covered by the pending
	// state, not by this number.
	tuneSettledGrace = 5 * time.Second
	// tunePendingCap: a tune that never reports its video is done holding
	// things up after this long. The safety valve for a /play that died
	// without saying so; without it, one lost tune would starve captions
	// until restart.
	tunePendingCap = 50 * time.Second
)

var (
	tuneMu sync.Mutex
	// tunePending holds the start time of every tune whose video has not yet
	// begun flowing, oldest first.
	tunePending []time.Time
	// lastTuneStart is when a tune last began anywhere on the machine, as
	// unix nanoseconds, for the settled grace.
	lastTuneStart int64
)

// captionTuneStarting marks that a tune is beginning somewhere on the
// machine, which postpones any heavy caption work until its video flows.
func captionTuneStarting() {
	tuneMu.Lock()
	tunePending = append(tunePending, time.Now())
	tuneMu.Unlock()
	atomic.StoreInt64(&lastTuneStart, time.Now().UnixNano())
}

// captionTuneSettled marks that a tune's video has started flowing to the
// DVR — or that the tune failed outright and there is nothing left to
// protect. Paired oldest-first with captionTuneStarting; a tune that never
// reports either way ages out at tunePendingCap.
func captionTuneSettled() {
	tuneMu.Lock()
	if len(tunePending) > 0 {
		tunePending = tunePending[1:]
	}
	tuneMu.Unlock()
}

// prewarmModelFile reads the weights into the page cache, pausing whenever a
// tune is young so the disk always belongs to the tune. It reports whether it
// finished: under a steady run of channel changes it gives up rather than
// carrying disk work into the indefinite future, and the caller retries at a
// quieter time. An unreadable file is reported as done — the engine's own
// load will say what is wrong with it.
func prewarmModelFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 8<<20)
	began := time.Now()
	deadline := began.Add(3 * time.Minute)
	paused := false
	for {
		if time.Now().After(deadline) {
			logger("[CC] Gave up warming %s after %s of mostly yielding to tunes", filepath.Base(path), time.Since(began).Round(time.Second))
			return false
		}
		if tunesPending() {
			if !paused {
				paused = true
				logger("[CC] Pausing the model warm-up for a tune in progress")
			}
			time.Sleep(2 * time.Second)
			continue
		}
		paused = false
		n, err := f.Read(buf)
		if err != nil || n == 0 {
			break
		}
	}
	logger("[CC] Warmed %s into memory in %s", filepath.Base(path), time.Since(began).Round(time.Millisecond))
	return true
}

// waitTuneQuiet waits for the newest tune on the machine to age past
// tuneFresh, up to the bound. False means the machine never went quiet.
func waitTuneQuiet(bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		quiet, wait := tuneQuiet()
		if quiet {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(wait)
	}
}

// waitTuneQuietHeld waits for the machine to have been quiet continuously for
// hold, rather than merely quiet at the instant it is asked.
//
// The difference is the whole difference for work that cannot be interrupted
// once it starts. A container that has just come up is quiet because the DVR
// has not asked for anything yet, not because there is nothing coming — the
// storm is seconds away, and starting a job at that moment is starting it
// directly into the storm. Waiting for a stretch of proven quiet is the only
// way to tell the calm before from the calm after.
//
// Returns false if hold could not be held within bound, and the caller is
// expected to try again rather than proceed anyway.
func waitTuneQuietHeld(hold, bound time.Duration) bool {
	// Held quiet exists to tell the calm before a storm from the calm after
	// one. With the door shut there is no storm to be on either side of, and
	// sleeping out the hold would be a pure delay in front of the door opening.
	if !serving.Load() {
		return true
	}
	deadline := time.Now().Add(bound)
	for {
		if !waitTuneQuiet(time.Until(deadline)) {
			return false
		}
		quietSince := time.Now()
		for {
			if time.Since(quietSince) >= hold {
				return true
			}
			if quiet, _ := tuneQuiet(); !quiet {
				break // the run was broken; start counting again
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(time.Second)
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// awaitQuiet is waitTuneQuietHeld with a voice.
//
// Every gate in this file is now bounded, and a bounded gate that fails is a
// decision — proceed carefully, or come back later — which the log has to be
// able to explain afterwards. The night this was written, three of them were
// spinning in loops that waited for a quiet stretch a three-tuner machine was
// never going to produce, and said nothing at all about it: the driver did not
// install, captions did not start, and from outside it looked like a build that
// had simply stopped working.
//
// So the rule is that no wait is silent and no wait is unbounded. A caller that
// gets false decides what to do about it; what it may not do is loop back here
// for ever.
func awaitQuiet(what string, hold, bound time.Duration) bool {
	began := time.Now()
	if waitTuneQuietHeld(hold, bound) {
		return true
	}
	logger("[CC] %s waited %s for a quiet moment and the machine has been tuning the whole time", what, time.Since(began).Round(time.Second))
	return false
}

// tuneSettleReader marks the tune settled on the first byte it delivers, or
// on close for a stream that never delivers one — whichever comes first.
type tuneSettleReader struct {
	inner io.ReadCloser
	once  sync.Once
}

func newTuneSettleReader(r io.ReadCloser) *tuneSettleReader { return &tuneSettleReader{inner: r} }

func (t *tuneSettleReader) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 {
		t.once.Do(captionTuneSettled)
	}
	return n, err
}

func (t *tuneSettleReader) Close() error {
	t.once.Do(captionTuneSettled)
	return t.inner.Close()
}

// tuneQuiet reports whether every tune on the machine is out of its fragile
// stretch, and if not, roughly how long to wait before asking again.
func tuneQuiet() (bool, time.Duration) {
	// Before the port is bound there is nothing to be quiet about: a request
	// cannot arrive, so no tune can be in its fragile stretch and none can start
	// during whatever the caller is about to do. This is the only time the
	// answer is certain rather than inferred.
	if !serving.Load() {
		return true, 0
	}
	now := time.Now()
	tuneMu.Lock()
	live := tunePending[:0]
	for _, t0 := range tunePending {
		if now.Sub(t0) < tunePendingCap {
			live = append(live, t0)
		}
	}
	tunePending = live
	pending := len(live) > 0
	tuneMu.Unlock()
	if pending {
		// Settling is an event, not a schedule: poll shortly.
		return false, 2 * time.Second
	}
	last := atomic.LoadInt64(&lastTuneStart)
	if last == 0 {
		return true, 0
	}
	age := time.Since(time.Unix(0, last))
	if age >= tuneSettledGrace {
		return true, 0
	}
	return false, tuneSettledGrace - age
}

// tunesPending reports whether any tune has yet to deliver its first video,
// with the same expiry as tuneQuiet. The gentle work — page-cache reading —
// yields on this alone: it need not sit out the settled grace, which exists
// for the heavy, un-pausable steps.
func tunesPending() bool {
	now := time.Now()
	tuneMu.Lock()
	live := tunePending[:0]
	for _, t0 := range tunePending {
		if now.Sub(t0) < tunePendingCap {
			live = append(live, t0)
		}
	}
	tunePending = live
	pending := len(live) > 0
	tuneMu.Unlock()
	return pending
}

// feed offers stream bytes to the recognizer without blocking.
func (e *captionEngine) feed(b []byte) {
	// The tune has to finish before anything expensive starts: bytes have to
	// have been flowing for captionSettle, not merely have begun. Arrival of
	// this call is itself the proof the stream is still alive at the deadline.
	if atomic.LoadInt64(&e.begun) == 0 {
		first := atomic.LoadInt64(&e.firstFeed)
		now := time.Now().UnixNano()
		switch {
		case first == 0:
			atomic.CompareAndSwapInt64(&e.firstFeed, 0, now)
			return
		case now-first < int64(captionSettle):
			return
		}
		e.begin()
	}

	// Nothing is kept while the model loads. The decoder does not exist yet, so
	// anything queued here would sit for several seconds and then be handed to
	// ffmpeg as a block of stale transport stream followed by the gap where the
	// queue overflowed — which is not a delay, it is a corrupt stream, and
	// ffmpeg spends its time reporting broken audio frames instead of decoding.
	// A transport stream can be joined at any point, so starting on live data
	// costs the opening seconds of captions and nothing else.
	if atomic.LoadInt64(&e.ready) == 0 {
		return
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case e.audioCh <- cp:
	default:
		// Dropped rather than stalling the stream, which is the right trade —
		// but said out loud, because this is transport stream bytes going
		// missing from the middle of a decode and the words in them go with
		// them. It was silent, so it could never be the answer to "where did
		// that word go".
		n := atomic.AddInt64(&e.audioLost, 1)
		if n == 1 || n%50 == 0 {
			logger("[CC] %s dropped audio the decoder could not keep up with (%d times)", e.label, n)
		}
	}
}

func (e *captionEngine) Close() {
	e.once.Do(func() {
		close(e.closed)
		// Stopping ffmpeg closes the pipe the listener is blocked on, which is
		// what lets it notice the shutdown and return.
		//
		// Taken under the lock and cleared, because the reader goroutine
		// replaces the decoder when it restarts one. Killing a copy that has
		// already been replaced would leave the new ffmpeg running with nobody
		// feeding it and the reader blocked on it forever.
		e.mu.Lock()
		cmd := e.ffmpeg
		e.ffmpeg = nil
		e.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		// Wait for the listener to finish before releasing anything it might be
		// inside. If it somehow does not stop, leaking the session is far
		// better than freeing it from under a call in flight.
		// If nothing has begun yet, win the race for the start slot: whoever
		// runs this Do first decides, so either the closure below marks the
		// engine finished, or begin() already claimed the slot and the started
		// work owns finishing. Checking a flag and then closing was a window in
		// which both happened, and closing a channel twice is a panic that
		// takes every tuner down with it.
		e.startOnce.Do(func() { e.finish() })
		select {
		case <-e.done:
		case <-time.After(10 * time.Second):
			// The recognizer is usually seconds from returning — a phrase in
			// flight, a reply queued behind a tune hold — so its resources
			// are handed back when it actually finishes, however late that
			// is, rather than dropped. Dropping them here leaked a reference
			// per busy channel change until the shared model could never be
			// freed at all.
			logger("[CC] %s recognizer is still finishing; its memory will be returned when it does", e.label)
			go func() {
				<-e.done
				e.mu.Lock()
				model := e.model
				e.mu.Unlock()
				if model != nil {
					model.Close()
				}
				logger("[CC] %s captions stopped late, memory returned", e.label)
			}()
			return
		}
		e.mu.Lock()
		model := e.model
		e.mu.Unlock()
		if model != nil {
			// Closing the recognizer releases its streaming session as well,
			// so the order the two engines want is theirs to decide.
			model.Close()
		}
		logger("[CC] %s captions stopped", e.label)
	})
}

// ---------------------------------------------------------------------------
