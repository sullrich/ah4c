package main

// Real-time closed captions for ah4c.
//
// Audio is pulled out of the encoder's transport stream, transcribed by a
// speech model, and the resulting text is written back into the same transport
// stream as CEA-608 caption bytes carried in ATSC A/53 user data. That is the
// way an HDHomeRun delivers captions off the air, so Channels DVR and every
// downstream player pick them up as CC1 with no sidecar file and no re-encode
// of the video.
//
// Nothing here is gated on an environment variable. State lives in
// captions/config.json and is driven from the Closed Captions page.
//
// Where things are:
//
//	captionmodels.go   the catalog and the saved configuration
//	captiondriver.go   downloading the weights, the engine and the driver
//	captionengine.go   loading transcribe.cpp and sharing a model between tuners
//	captionlisten.go   pulling out audio, deciding where a phrase ends
//	cea608.go          turning text into caption bytes, and pacing them
//	captioninject.go   putting those bytes into the transport stream
//	captionsapi.go     the HTTP surface behind the Closed Captions page
//	spelling.go        which English the captions are written in
//
//	cohere.go canary.go nemotron.go parakeet.go
//	                   one file per model: its catalog entry and its quirks
//
// This file is what is left: the wrapper that decides whether a stream is
// captioned at all, and the pieces the rest of it hangs from.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ebitengine/purego"
)

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

// Everything the feature needs at run time lives in one directory, bound to the
// host so it survives a container rebuild. Nothing is added to the image: the
// speech engine, the model and any GPU driver are downloaded on demand from the
// Closed Captions page and land here.
const (
	captionDir     = "captions"
	captionCfgFile = "captions/config.json"
	captionModels  = "captions/models"
	captionRuntime = "captions/engine"
	captionDrivers = "captions/drivers"
)

// parakeetRelease is the parakeet.cpp build fetched on demand. It is a ggml
// implementation of NVIDIA's Parakeet models with a flat C entry point, about a
// megabyte, opened at run time with purego. ah4c itself stays pure Go: there is
// no cgo, no ONNX Runtime and nothing linked into the binary.
// transcribeRelease is the transcribe.cpp build, the second engine. It is the
// same idea as parakeet.cpp — ggml, a flat C entry point, downloaded rather
// than bundled — but it runs a much wider set of model families, which is what
// makes the newer streaming checkpoints reachable at all.
// parakeet.cpp only ever implements NVIDIA's Parakeet architectures.
//
// The tag is v0.1.3; the asset file names carry the version without the v.
const transcribeRelease = "0.1.3"

// A model names the engine that can run it. Nothing else in the feature cares
// which is which: the engines are downloaded the same way, kept in the same
// directory, and hidden behind the same recognizer interface.
const rtTranscribe = "transcribe"

// speechRuntime describes an engine for the page. Both are fetched on demand
// and neither is in the image.
type speechRuntime struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Desc    string `json:"desc"`
}

// The descriptions do not compare the two engines, on purpose. Which one a
// model uses is not a choice anybody makes, and presenting them side by side as
// though it were turns an implementation detail into homework. They are named
// only so it is clear what is being downloaded.
var speechRuntimes = []speechRuntime{
	{
		Key: rtTranscribe, Name: "transcribe.cpp", Version: "v" + transcribeRelease,
		Desc: "the helper program this model listens with",
	},
}

// runtimeDescriptions is the engine blurbs keyed by engine, for the page.
func runtimeDescriptions() map[string]string {
	out := make(map[string]string, len(speechRuntimes))
	for _, r := range speechRuntimes {
		out[r.Key] = r.Name + " " + r.Version + ". " + r.Desc
	}
	return out
}

func findSpeechRuntime(key string) speechRuntime {
	for _, r := range speechRuntimes {
		if r.Key == key {
			return r
		}
	}
	return speechRuntimes[0]
}

// engineVariant is a build of the engine: which processor it runs the model on.
//
// All of them are downloaded at run time, so choosing one costs nothing in the
// image. What they differ in is what has to already be present in the container
// for the library to load at all, which is why each one names its requirement
// and is offered only when that requirement is actually there.
type engineVariant struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	// Suffix picks the release archive; SizeMB is roughly what that costs.
	Suffix string `json:"-"`
	SizeMB int    `json:"sizeMB"`
	// Needs is the shared library that has to be loadable for this build to
	// work. Empty means nothing beyond the base image.
	Needs string `json:"needs"`
	// Why explains, in the page's words, what provides that library.
	Why string `json:"why"`
}

// engineVariants is ordered cheapest-first; the page offers whichever ones this
// container can actually load.
var engineVariants = []engineVariant{
	{
		Key: "auto", Name: "Choose automatically (recommended)", Suffix: "cpu",
		Desc:   "Uses a graphics card if this container can reach one, and the processor if it cannot. The right answer unless you have a reason to pin it.",
		SizeMB: 1,
	},
	{
		Key: "cpu", Name: "CPU", Suffix: "cpu",
		Desc:   "Runs on the processor. Needs nothing beyond what is already in the image and is fast enough for several tuners at once.",
		SizeMB: 1,
	},
	{
		Key: "vulkan", Name: "GPU via Vulkan", Suffix: "vulkan",
		Desc:   "Runs the model on an Intel, AMD or NVIDIA GPU through Vulkan.",
		SizeMB: 59,
		Needs:  "libvulkan.so.1",
		Why:    "Needs a Vulkan loader and driver in the container, plus /dev/dri passed through. The base image ships neither, so this only appears if you have added them.",
	},
	{
		Key: "cuda", Name: "GPU via CUDA", Suffix: "cuda",
		Desc:   "Runs the model on an NVIDIA GPU. The download carries its own CUDA runtime, so only the driver has to come from outside.",
		SizeMB: 537,
		Needs:  "libcuda.so.1",
		Why:    "Needs the NVIDIA container runtime, which injects the driver. Add the GPU to your compose file; nothing changes in the image.",
	},
}

func findEngineVariant(key string) (engineVariant, bool) {
	for _, v := range engineVariants {
		if v.Key == key {
			return v, true
		}
	}
	return engineVariant{}, false
}

// engineUsable reports whether this container can load a variant, by trying the
// library it depends on. Asking the loader is the only honest test: a driver
// can be present without the device, or injected by a container runtime that
// left no other trace.
//
// The answer is cached, and has to be. Asking means dlopen, which takes the
// dynamic loader's global lock and bumps a reference this code never drops. The
// Closed Captions page asks about every build of both engines on every poll,
// and it polls while a download runs, so an open page was making a few thousand
// dlopen calls an hour against CUDA and Vulkan. That is how a page that only
// reports on captions ends up stalling the recognizer it is reporting on: the
// engine's own calls into native code contend for the same loader lock.
var (
	usableLock  sync.Mutex
	usableCache = map[string]bool{}
)

func engineUsable(v engineVariant) bool {
	if v.Needs == "" {
		return true
	}
	// The lock covers the answer, never the asking.
	//
	// This used to hold the mutex across the dlopen, and that put a load of the
	// whole Vulkan or CUDA dependency chain — every symbol resolved eagerly,
	// hundreds of megabytes off a cold disk — inside a lock the tune path takes.
	// maybeWrapCaptions asks this question on every captioned tune, and it asks
	// it while holding the global tuner lock, so one background probe could
	// stall the tune that started underneath it and every tune queued behind
	// that one. The cache was added to stop the page making thousands of these
	// calls an hour; it did not stop the first one blocking a recording.
	//
	// So the answer is read under the lock, the library is opened outside it,
	// and the result is stored under it again. Two probes racing may both open
	// the same library, which costs nothing: dlopen is reference counted and
	// idempotent, and the second gets the handle the first already mapped.
	usableLock.Lock()
	ok, seen := usableCache[v.Needs]
	usableLock.Unlock()
	if seen {
		return ok
	}
	h, err := purego.Dlopen(v.Needs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	ok = err == nil && h != 0
	usableLock.Lock()
	usableCache[v.Needs] = ok
	usableLock.Unlock()
	return ok
}

// forgetEngineUsable clears that cache. A driver can be installed while ah4c is
// running, which is the one moment the answer legitimately changes.
func forgetEngineUsable() {
	usableLock.Lock()
	usableCache = map[string]bool{}
	usableLock.Unlock()
}

// engineAsset names the archive for the engine the selected model needs, and
// the library inside it. The architecture is decided at compile time, so an
// arm64 image fetches the arm64 build without being told which it is.
func engineAsset() (url, local string, ok bool) {
	url, _, local, ok = runtimeAssetFor(neededRuntime(), runtime.GOOS, runtime.GOARCH, currentEngineVariant())
	return url, local, ok
}

// neededRuntime is the engine the selected model runs on. A model that names
// no engine is a Parakeet one, which is what the catalog held before there was
// a second engine to name.
func neededRuntime() string {
	return rtTranscribe
}

// runtimeAssetFor is the platform table for both engines. It returns where the
// archive is, the directory its contents are unpacked into, and the library
// whose presence means the engine is installed.
//
// The unpack directory is named by the engine's own build rather than by the
// processor choice, because the two engines do not divide their builds the same
// way: parakeet.cpp publishes one archive per backend, while transcribe.cpp
// puts the processor and Vulkan backends in a single archive and picks between
// them when the model is loaded.
func runtimeAssetFor(rt, goos, goarch, variant string) (url, dir, lib string, ok bool) {
	url, lane, ok := transcribeAssetFor(goos, goarch, variant)
	if !ok {
		return "", "", "", false
	}
	return url, filepath.Join(rtTranscribe, lane), transcribeLib(goos), true
}

// transcribeAssetFor names the transcribe.cpp archive and the build inside it.
//
// Two things differ from parakeet.cpp and both simplify what has to be
// downloaded. The processor and Vulkan backends ship together, so switching
// between them costs nothing once either is here. And libtranscribe.so does not
// link Vulkan itself — the backend is a separate library the engine loads if it
// finds one — so the combined build runs on a machine with no GPU at all rather
// than failing to open.
func transcribeAssetFor(goos, goarch, variant string) (url, lane string, ok bool) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		lane = "linux-x86_64-cpu-vulkan"
		if variant == "cuda" || variant == "cuda12" {
			// There is one CUDA build rather than the two parakeet.cpp offers,
			// so both of those choices land on it.
			lane = "linux-x86_64-cuda"
		}
	case "linux/arm64":
		lane = "linux-aarch64-cpu-vulkan"
	case "darwin/arm64":
		lane = "macos-arm64-metal"
	case "darwin/amd64":
		lane = "macos-x86_64-cpu"
	default:
		return "", "", false
	}
	return "https://github.com/handy-computer/transcribe.cpp/releases/download/v" + transcribeRelease +
		"/transcribe-native-" + transcribeRelease + "-" + lane + ".tar.gz", lane, true
}

// runtimeSizeMB is roughly what a build costs to download. The two engines are
// not remotely the same size: parakeet.cpp is a megabyte because ggml is
// compiled into it, while transcribe.cpp ships its backends separately and the
// Vulkan one alone is most of the archive.
func runtimeSizeMB(rt string, v engineVariant) int {
	if rt != rtTranscribe {
		return v.SizeMB
	}
	_, lane, ok := transcribeAssetFor(runtime.GOOS, runtime.GOARCH, v.Key)
	if !ok {
		return 0
	}
	switch lane {
	case "linux-x86_64-cuda":
		return 216
	case "linux-x86_64-cpu-vulkan":
		return 28
	case "linux-aarch64-cpu-vulkan":
		return 25
	}
	return 2
}

func transcribeLib(goos string) string {
	if goos == "darwin" {
		return "libtranscribe.dylib"
	}
	return "libtranscribe.so"
}

// runtimeVariantOffered reports whether a processor choice is a real choice for
// an engine on this platform, so the page does not offer a build that is either
// unpublished or identical to one already listed.
func runtimeVariantOffered(rt, goos, goarch, variant string) bool {
	if variant == "auto" {
		return true
	}
	if rt == rtTranscribe {
		_, lane, ok := transcribeAssetFor(goos, goarch, variant)
		if !ok {
			return false
		}
		switch variant {
		case "cpu":
			return true
		case "vulkan":
			return strings.Contains(lane, "vulkan")
		case "cuda":
			return strings.Contains(lane, "cuda")
		case "cuda12":
			// transcribe.cpp publishes a single CUDA build, already offered.
			return false
		}
		return false
	}
	return false
}

// currentEngineVariant is the configured build, falling back to the processor
// if the chosen one cannot load here.
func currentEngineVariant() string {
	cfg := currentCaptionConfig()
	// "auto", and anything unset, means use the best build this machine can
	// actually run. The old default was "cpu", which is indistinguishable from
	// somebody deliberately choosing the processor — so a machine with a
	// perfectly good graphics card sat there using none of it, and every
	// measurement taken on it was a measurement of the processor.
	if cfg.Engine == "" || cfg.Engine == "auto" {
		if g := gpuVariant(neededRuntime()); g != "" {
			return g
		}
		return "cpu"
	}
	v, found := findEngineVariant(cfg.Engine)
	if !found || !engineUsable(v) {
		return "cpu"
	}
	return v.Key
}

// engineLibPath is where the engine the selected model needs would land.
func engineLibPath() string {
	return runtimeLibPath(neededRuntime(), currentEngineVariant())
}

// runtimeLibPath is where a given engine's library lives once downloaded.
func runtimeLibPath(rt, variant string) string {
	if variant == "auto" {
		variant = currentEngineVariant()
	}
	_, dir, lib, ok := runtimeAssetFor(rt, runtime.GOOS, runtime.GOARCH, variant)
	if !ok {
		return ""
	}
	return filepath.Join(captionRuntime, dir, lib)
}

// runtimeDirFor is the directory an engine unpacks into. transcribe.cpp needs
// it by name at run time as well as at download time: its ggml backends are
// separate libraries next to the engine, and it is told where to find them.
func runtimeDirFor(rt, variant string) string {
	if variant == "auto" {
		variant = currentEngineVariant()
	}
	_, dir, _, ok := runtimeAssetFor(rt, runtime.GOOS, runtime.GOARCH, variant)
	if !ok {
		return ""
	}
	return filepath.Join(captionRuntime, dir)
}

func engineInstalled() bool {
	return runtimeInstalled(neededRuntime(), currentEngineVariant())
}

func runtimeInstalled(rt, variant string) bool {
	if variant == "auto" {
		variant = currentEngineVariant()
	}
	p := runtimeLibPath(rt, variant)
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// ---------------------------------------------------------------------------
// Model residency
// ---------------------------------------------------------------------------

// The model is not held in memory between streams, and nothing loads it before
// it is wanted.
//
// A standing copy was tried: loaded at container start and kept, so no tune
// ever found it anywhere but ready. It works, and it costs a gigabyte and a
// half of the machine's memory for the whole life of the container whether
// anybody is watching anything or not, which is a great deal to pay for the
// first tune after a restart. Worse, loading it early meant opening the engine
// early, and the engine scans for its backends exactly once — so a graphics
// driver installed from the page afterwards could never be seen, and the page
// had to start asking people to restart the container.
//
// So it loads when a channel is tuned, once that tune has actually settled,
// and the weights are given back when the last stream using them ends. The
// load is in acquireTxModel, behind the same two gates everything heavy here
// goes behind: the file is warmed into the page cache in a loop that yields to
// tunes, and the native load runs only on a quiet machine, failing fast to be
// retried at the next lull rather than ever pushing against a tune in flight.

// ---------------------------------------------------------------------------
// Where the models attach
// ---------------------------------------------------------------------------

// Every model has knobs, and every model's knobs live in that model's own file.
// This is the joint they meet at, and it is deliberately the only one.
//
// captions.go is the common part: the audio splitter, the voice detector, the
// recognizer, both caption encoders, the injector, the tune gate. None of it
// knows which model it is serving. cohere.go, canary.go, nemotron.go and parakeet.go each
// hold one model — its catalog entry, and what it asks of the code around it —
// and they are reachable from here and nowhere else.
//
// The two references that tie a model in are its line in captionModelCatalog
// and its case in quirksFor below. Delete a model's file and the compiler names
// both, by line number, which is how this stays true without anybody
// remembering to keep it true.
//
// What this replaces was bolting each fix on where it was noticed: a phrase
// length inside the audio splitter, a noise gate inside the voice detector, a
// suppression list inside the result handler. Every one of them correct, and
// none of them saying which model it was for.
//
// A model with no opinions gets modelDefaults, which asks for nothing. That is
// where a new one starts; it earns an entry by misbehaving.

// modelQuirks is what one model asks of the code around it.
// phraseWindowFor is how long a phrase may run: the page's choice when the model
// offers one and the choice is among the values it offers, the model's own
// figure otherwise.
//
// Checked against the model's list rather than clamped to a range, because a
// value the page never offered is a saved config from another model or another
// version, and the model's own default is a better answer than the nearest
// number to somebody else's setting.
func phraseWindowFor(q modelQuirks, cfg captionConfig) float64 {
	for _, w := range q.Windows {
		if cfg.PhraseSec == w {
			return w
		}
	}
	if q.PhraseWindow > 0 {
		return q.PhraseWindow
	}
	return vadMaxPhrase
}

type modelQuirks struct {
	// Windows is the set of phrase lengths this model may be run with, for the
	// page to offer. Empty means the length is not a choice for this model.
	Windows []float64
	// PhraseWindow is how long a phrase may run before it is cut, in seconds.
	// Phrase-at-a-time models have an operating point; streaming models run
	// their family's own and ignore this.
	PhraseWindow float64
	// NoiseGate says this model will confidently transcribe things that are
	// not speech, and wants stretches of steady noise held back from it.
	NoiseGate bool
	// ContextSec is how much already-transcribed audio to carry in front of a
	// phrase so this model has something to read into. Zero carries nothing,
	// which is how every model behaved before it existed.
	//
	// Per model because what it buys and what it costs are both per model. It
	// buys accuracy on short phrases, which matters to a model being cut short;
	// it costs a longer encoder run, which is close to free only on a family
	// whose encoder time barely moves with the length — measured for one
	// family and assumed for none.
	ContextSec float64
	// StreamChunkSec is how much audio a streaming model wants per feed.
	//
	// Not a buffer size and not a tuning knob: a cache-aware streaming encoder
	// runs one forward pass per feed over its whole attention context, so the
	// work is per call and not per second of audio. Feeding it a tenth of what
	// it is built for does not make it answer sooner — it cannot commit ahead
	// of its own lookahead whatever it is handed — it simply does the same pass
	// ten times as often. Zero means the shared default.
	StreamChunkSec float64
	// Suppress reports text this model produces when nothing was said. It is
	// given a whole phrase and answers about the whole phrase.
	Suppress func(string) bool
	// PNC and ITN are the punctuation and number-formatting toggles the engine
	// takes per call. Zero is the family's own default for both, which is what
	// a model gets until somebody has a reason.
	PNC int32
	ITN int32
	// KVType is the precision of the attention cache, from transcribe.h.
	// Zero is the engine's own choice and is right for almost everything; a
	// model asks for something else only when its accuracy has been measured
	// to want it.
	KVType int32
}

// modelDefaults is what is asked of a model nobody has written notes about:
// nothing. A new model starts here and earns its entry by misbehaving.
var modelDefaults = modelQuirks{PhraseWindow: 4.0}

// mustFindModel is findCaptionModel for callers that hold a configuration
// rather than a model and only want its handling. An unknown key gets the
// zero model, which quirksFor answers for with the defaults.
func mustFindModel(key string) captionModel {
	m, _ := findCaptionModel(key)
	return m
}

// quirksFor is the single place a model's name is turned into its handling.
func quirksFor(m captionModel) modelQuirks {
	switch m.Key {
	case cohereTranscribe.Key:
		return cohereQuirks
	case canaryFlash.Key:
		return canaryQuirks
	case nemotronStreaming.Key:
		return nemotronQuirks
	case parakeetTDT.Key:
		return parakeetQuirks
	}
	return modelDefaults
}

// isSoundEventTag reports whether a phrase is a model describing a noise
// rather than repeating a word. Shape rather than words, because the words are
// endless and a list of them would be out of date by the next advertisement: a
// whole phrase wrapped in brackets is the captioning convention for a sound,
// and nothing anybody says out loud is wrapped in brackets from beginning to
// end. Musical notes are the same idea with no words in it at all.
//
// Not Cohere-specific — any model trained on captioned video does this — so it
// sits outside the block above and is called from it.
func isSoundEventTag(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	if (strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")")) ||
		(strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")) ||
		(strings.HasPrefix(t, "*") && strings.HasSuffix(t, "*")) {
		// Only when the brackets enclose the whole thing, rather than opening
		// and closing again inside a longer sentence.
		inner := t[1 : len(t)-1]
		return !strings.ContainsAny(inner, "()[]")
	}
	notes := false
	for _, r := range t {
		switch {
		case r == '♪' || r == '♫' || r == '♬' || r == '♩':
			notes = true
		case r == ' ' || r == '.' || r == ',' || r == '-' || r == '—':
		default:
			return false
		}
	}
	return notes
}

// ---------------------------------------------------------------------------
// Stream wrapper
// ---------------------------------------------------------------------------

// captionStream sits between the encoder and the DVR, copying the transport
// stream through the injector while feeding a copy of it to the recognizer.
type captionStream struct {
	src    io.ReadCloser
	engine *captionEngine
	pr     *io.PipeReader
	pw     *io.PipeWriter
	once   sync.Once
	// pump starts the reading loop, and not before the gate has let a byte
	// through. first holds that byte's chunk until the loop can take it. See
	// Read.
	pump    sync.Once
	started bool
	first   []byte
}

// maybeWrapCaptions returns src unchanged unless captions are switched on and
// the selected model is installed, so a tune costs nothing when captions are
// off or half configured.
//
// It is also where the tune gate learns about tunes: this is called for every
// tuner's stream as it comes up, captioned or not, so it marks the tune as
// beginning here — through its most fragile stretch, playback confirmation —
// and the returned reader marks it settled at the first delivered byte. All
// of it inside this file; the caller just wraps a reader like always.
// captionReady is whether captions can run and, if not, why — answered off the
// tune path and refreshed wherever any of its inputs change. maybeWrapCaptions
// reads it and asks nothing.
type captionReadyState struct {
	ok    bool
	why   string
	model captionModel
}

var captionReadyVal atomic.Value

func captionReadiness() captionReadyState {
	if v, ok := captionReadyVal.Load().(captionReadyState); ok {
		return v
	}
	// Nothing has looked yet. Look now rather than refuse: this can only happen
	// before the first refresh, which runs before the port is bound.
	refreshCaptionReady()
	v, _ := captionReadyVal.Load().(captionReadyState)
	return v
}

// refreshCaptionReady re-answers it. Never call from the tune path: it stats.
func refreshCaptionReady() {
	cfg := currentCaptionConfig()
	m, found := findCaptionModel(cfg.Model)
	st := captionReadyState{ok: true, model: m}
	switch {
	case !found:
		st = captionReadyState{why: fmt.Sprintf("unknown model %q", cfg.Model)}
	case !modelInstalled(m):
		st = captionReadyState{why: "model " + m.Key + " is not downloaded"}
	case m.NeedsGPU && !gpuReady.Load():
		// Captioning anyway would be worse than not captioning: this model on a
		// processor loses ground against live audio until most of the speech is
		// missed, and half a transcript is harder to watch than none.
		st = captionReadyState{why: "model " + m.Key + " needs a GPU and none is usable here"}
	case !engineInstalled():
		st = captionReadyState{why: "the speech runtime is not downloaded"}
	}
	captionReadyVal.Store(st)
}

func maybeWrapCaptions(src io.ReadCloser, tunerIndex int, label string) io.ReadCloser {
	captionTuneStarting()
	src = newTuneSettleReader(src)
	cfg := currentCaptionConfig()
	if !cfg.Enabled {
		return src
	}
	if len(cfg.Tuners) > 0 {
		found := false
		for _, t := range cfg.Tuners {
			if t == tunerIndex {
				found = true
				break
			}
		}
		if !found {
			return src
		}
	}
	// Everything about whether captions can run has been worked out already,
	// off this path, and is read here as one value.
	//
	// It used to be worked out here: a lookup, two os.Stat calls and a question
	// that could dlopen the whole Vulkan chain, all inside the global tuner
	// lock, on every captioned tune. None of it is expensive on a quiet
	// machine, which is exactly why it survived — and the one time it matters
	// is three tunes arriving together, which is the one time nothing may be in
	// the way. A stat is a disk touch, and the disk is what a tune is competing
	// for.
	r := captionReadiness()
	if !r.ok {
		logger("[CC] %s %s, captions disabled for this tune", label, r.why)
		return src
	}
	m := r.model
	engine, err := newCaptionEngine(cfg, m, label)
	if err != nil {
		logger("[CC] %s could not start captions: %v", label, err)
		return src
	}

	cs := &captionStream{src: src, engine: engine}
	cs.pr, cs.pw = io.Pipe()
	return cs
}

func (cs *captionStream) run() {
	// Captions are a convenience; the stream is not. If anything in the
	// injector goes wrong, the picture has to keep flowing, so a panic here is
	// caught and the rest of the stream is copied straight through untouched
	// rather than being allowed to kill the process and every tuner with it.
	raw := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				raw = true
				logger("[CC] %s captions failed (%v); passing the stream through untouched from here", cs.engine.label, r)
			}
		}()
		cs.inject()
	}()
	if raw {
		// Bytes first: the DVR is mid-read, and a panic early in a tune is
		// exactly when a stall would kill it. The engine winds down in the
		// background; it no longer owns anything on the byte path.
		go cs.engine.Close()
		_, err := io.Copy(cs.pw, cs.src)
		cs.pw.CloseWithError(err)
	}
}

// inject is the captioning path proper.
func (cs *captionStream) inject() {
	// The injector emits packet by packet, and an io.Pipe write is a
	// synchronous rendezvous with the reader — per 188-byte packet that was
	// thousands of goroutine handoffs a second per stream. Buffering between
	// them turns that into one handoff per read chunk; the flush after each
	// chunk keeps latency at exactly one chunk, which the pipe already had.
	bw := bufio.NewWriterSize(cs.pw, 64*1024)
	inj := newCaptionInjector(bw, cs.engine.enc, cs.engine.label)
	buf := make([]byte, 64*1024)
	// The chunk Read already took directly, before this loop existed. It is the
	// first of the stream and carries the program tables the injector needs, so
	// it goes through the injector like every other chunk rather than being
	// handed to the DVR raw.
	if len(cs.first) > 0 {
		cs.engine.feed(cs.first)
		if _, werr := inj.Write(cs.first); werr != nil {
			cs.pw.CloseWithError(werr)
			return
		}
		if werr := bw.Flush(); werr != nil {
			cs.pw.CloseWithError(werr)
			return
		}
		cs.first = nil
	}
	for {
		n, err := cs.src.Read(buf)
		if n > 0 {
			cs.engine.feed(buf[:n])
			if _, werr := inj.Write(buf[:n]); werr != nil {
				cs.pw.CloseWithError(werr)
				return
			}
			if werr := bw.Flush(); werr != nil {
				cs.pw.CloseWithError(werr)
				return
			}
		}
		if err != nil {
			inj.Flush()
			bw.Flush()
			cs.pw.CloseWithError(err)
			return
		}
	}
}

// Read is a plain pass-through until the first byte arrives, and only then
// becomes the captioned path.
//
// Nothing about captions may be in the way while ah4c is deciding whether the
// box is playing. That window is the gate holding every byte back, and
// waitForPlayback polling the box over adb beside it; the captioned path adds a
// pipe, a pump goroutine, an injector and a second buffer to the stream during
// exactly that stretch. Measured, that difference is the whole failure:
// confirmation succeeds with captions off and times out with them on, on the
// same box and the same channel.
//
// So until a byte actually comes through, this calls straight into src, the way
// the un-captioned reader does — same call, same goroutine, same buffer. A byte
// arriving means the gate has opened and confirmation is over, and only then is
// the pump started, the first chunk handed to it, and the injector put in the
// path.
//
// Nothing is lost by the delay: the gate emits the program tables at release,
// so the first chunk carries what the injector needs to identify the stream.
func (cs *captionStream) Read(p []byte) (int, error) {
	if !cs.started {
		n, err := cs.src.Read(p)
		if n > 0 {
			cs.started = true
			cs.first = append([]byte(nil), p[:n]...)
			cs.pump.Do(func() { go cs.run() })
			return 0, nil
		}
		return 0, err
	}
	return cs.pr.Read(p)
}

func (cs *captionStream) Close() error {
	// The encoder connection is released first and immediately: on a channel
	// change the next tune needs this tuner's encoder, and holding it while
	// the recognizer winds down costs up to ten seconds of the new tune's
	// window — the engine teardown even waits on work that the new tune's
	// own quiet gate is holding, a circle only this ordering breaks.
	// The engine cleans itself up in the background; nothing about it can
	// touch the stream that no longer exists.
	err := cs.src.Close()
	cs.once.Do(func() {
		cs.pr.Close()
		go cs.engine.Close()
	})
	return err
}

// ---------------------------------------------------------------------------
