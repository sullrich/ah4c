package main

// Real-time closed captions for ah4c.
//
// Audio is pulled out of the encoder's transport stream, transcribed on the CPU
// by an NVIDIA Parakeet TDT model, and the resulting text is written back into
// the same transport stream as CEA-608 caption bytes carried in ATSC A/53 user
// data. That is the way an HDHomeRun delivers captions off the air, so Channels
// DVR and every downstream player pick them up as CC1 with no sidecar file and
// no re-encode of the video.
//
// Nothing here is gated on an environment variable. State lives in
// captions/config.json and is driven from the Closed Captions page.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

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
const parakeetRelease = "v0.5.0"

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
const (
	rtParakeet   = "parakeet"
	rtTranscribe = "transcribe"
)

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
		Key: rtParakeet, Name: "parakeet.cpp", Version: parakeetRelease,
		Desc: "the helper program this model listens with",
	},
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
	{
		Key: "cuda12", Name: "GPU via CUDA 12", Suffix: "cuda12",
		Desc:   "The same, built against CUDA 12 for older drivers. Try this if the CUDA build will not load.",
		SizeMB: 722,
		Needs:  "libcuda.so.1",
		Why:    "Needs the NVIDIA container runtime, which injects the driver. Add the GPU to your compose file; nothing changes in the image.",
	},
}

// variantSuffix maps a variant key to its archive suffix.
func variantSuffix(key string) string {
	if v, ok := findEngineVariant(key); ok {
		return v.Suffix
	}
	return "cpu"
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
	usableLock.Lock()
	defer usableLock.Unlock()
	if ok, seen := usableCache[v.Needs]; seen {
		return ok
	}
	h, err := purego.Dlopen(v.Needs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	ok := err == nil && h != 0
	usableCache[v.Needs] = ok
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
	m, ok := findCaptionModel(currentCaptionConfig().Model)
	if !ok || m.Runtime == "" {
		return rtParakeet
	}
	return m.Runtime
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
	if rt == rtTranscribe {
		url, lane, ok := transcribeAssetFor(goos, goarch, variant)
		if !ok {
			return "", "", "", false
		}
		return url, filepath.Join(rtTranscribe, lane), transcribeLib(goos), true
	}
	url, lib, ok = engineAssetFor(goos, goarch, variantSuffix(variant))
	if !ok {
		return "", "", "", false
	}
	return url, variant, lib, true
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
	url, _, ok := engineAssetFor(goos, goarch, variantSuffix(variant))
	if !ok {
		return false
	}
	if variant == "cpu" {
		return true
	}
	// A variant with no build of its own for this platform is not a choice.
	// Apple silicon is the clear case: Metal is in the one build there, and
	// arm64 Linux has no CUDA build at all.
	cpuURL, _, _ := engineAssetFor(goos, goarch, "cpu")
	return url != cpuURL
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

// engineAssetFor is the platform table, separated so every entry can be checked
// rather than only the one this machine happens to be.
func engineAssetFor(goos, goarch, variant string) (url, local string, ok bool) {
	base := "https://github.com/mudler/parakeet.cpp/releases/download/" + parakeetRelease + "/parakeet-" + parakeetRelease + "-lib-"
	if variant == "" {
		variant = "cpu"
	}
	switch goos + "/" + goarch {
	case "linux/amd64":
		return base + "linux-" + variant + "-x64.tar.gz", "libparakeet.so", true
	case "linux/arm64":
		// Only the processor and Vulkan builds are published for arm64.
		if variant != "cpu" && variant != "vulkan" {
			variant = "cpu"
		}
		return base + "linux-" + variant + "-arm64.tar.gz", "libparakeet.so", true
	case "darwin/arm64":
		// Metal is built in on Apple silicon; there is no separate choice.
		return base + "macos-metal-arm64.tar.gz", "libparakeet.dylib", true
	case "darwin/amd64":
		return base + "macos-cpu-x64.tar.gz", "libparakeet.dylib", true
	}
	return "", "", false
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
// Model catalog
// ---------------------------------------------------------------------------

// captionModel describes a downloadable ASR model. Models are never bundled in
// the image; the user pulls the one they want from the Closed Captions page.
type captionModel struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Desc says what the model does, Latency how soon captions appear, and
	// Hardware what it is comfortable running on. All of it is shown on the
	// page, because the choice between these is a trade rather than a ranking:
	// the most accurate model is the slowest, and the fastest writes no
	// punctuation.
	Desc string `json:"desc"`
	// Role is the slot this model fills. Four models, four answers — a list
	// where everything has a reason to exist reads as a menu, not homework.
	Role     string `json:"role"`
	Latency  string `json:"latency"`
	Hardware string `json:"hardware"`
	// Accuracy is the plain-English tier and Benchmark the measurement behind
	// it. Benchmark is empty where there is no published figure, rather than
	// carrying a guess: a made-up number would outrank a real one on the page.
	Accuracy  string `json:"accuracy"`
	Benchmark string `json:"benchmark"`
	// Runtime names the engine that can load these weights, and Repo the
	// Hugging Face repository they come from.
	Runtime string `json:"runtime"`
	Repo    string `json:"repo"`
	File    string `json:"file"`
	SizeMB  int    `json:"sizeMB"`
	// Streaming models transcribe as the audio arrives. Punctuation says
	// whether the model writes any: the ones that do not cannot be made to, and
	// a wall of unpunctuated capitals is worth knowing about in advance.
	Streaming   bool `json:"streaming"`
	Punctuation bool `json:"punctuation"`
	// NeedsGPU marks a model that is not usable without graphics acceleration.
	// It is not a preference: these are offered only where a GPU build can
	// actually run, because the alternative is a model that loads, falls
	// steadily behind live audio and drops most of what is said. Captions that
	// bad are worse than none, and finding out costs a multi-gigabyte download.
	NeedsGPU  bool     `json:"needsGPU"`
	Languages []string `json:"languages"`
}

const captionModelRepo = "mudler/parakeet-cpp-gguf"

// The 25 languages the multilingual checkpoints cover. The streaming
// multilingual model advertises more, but these are the ones both agree on, and
// pinning a locale a model does not know is an error rather than a fallback.
var euroLanguages = []string{"auto", "bg", "cs", "da", "de", "el", "en", "es", "et", "fi", "fr", "hr", "hu",
	"it", "lt", "lv", "mt", "nl", "pl", "pt", "ro", "ru", "sk", "sl", "sv", "uk"}

// Quantized weights: on CPU they are what make these run faster than real time,
// and they keep the download manageable.
//
// The streaming models are listed first because they are what most people
// want: they transcribe as the audio arrives instead of waiting for a phrase to
// finish, which is the difference between captions a second behind and captions
// three or four seconds behind.
var captionModelCatalog = []captionModel{
	{
		Key:         "realtime-multilingual",
		Name:        "Nemotron 3.5 Streaming 0.6B",
		Role:        "The all-round choice",
		Desc:        "Transcribes continuously as the audio arrives and writes proper punctuation and sentence case, in any of its languages. The one to use for anything other than English, and a little quicker than the Unified at some cost in accuracy.",
		Latency:     "Under a second",
		Accuracy:    "Good",
		Benchmark:   "3.0% of words come out wrong",
		Hardware:    "A modern multi-core CPU. Roughly five times the work of the 120M.",
		Runtime:     rtParakeet,
		File:        "nemotron-3.5-asr-streaming-0.6b-q8_0.gguf",
		SizeMB:      938,
		Streaming:   true,
		Punctuation: true,
		Languages:   euroLanguages,
	},
	{
		Key:       "realtime-120m",
		Name:      "Parakeet Realtime 120M",
		Role:      "The low-end choice",
		Desc:      "Just as quick and a fifth of the size, for hardware that cannot spare the cores. Writes no punctuation at all: the model produces none, and no setting changes that.",
		Latency:   "Under a second",
		Accuracy:  "Basic",
		Hardware:  "Runs on almost anything. A low-power NAS, a mini PC or a Raspberry Pi class board is plenty.",
		Runtime:   rtParakeet,
		File:      "realtime_eou_120m-v1-q8_0.gguf",
		SizeMB:    168,
		Streaming: true,
		Languages: []string{"en"},
	},
	{
		Key:  "cohere-transcribe",
		Name: "Cohere Transcribe 03-2026",
		Role: "The high-end choice",
		Desc: "The most accurate open speech model there is, and the top of the public leaderboard. It reads a whole phrase and writes it out rather than transcribing as the audio arrives, and what it writes does not read like machine transcription — it reads like the closed captions on a broadcast channel. It asks for more of a machine than anything else here, and repays it.",
		// It is quick when it is given room. On a laptop APU over Vulkan it
		// does eleven seconds of audio in about one and a half, eight times
		// faster than real time — but that figure is for one long call, and
		// almost all of its cost is paid at the start of a call rather than per
		// second of audio. Fed two-second phrases it runs slower than real
		// time. The delay setting is what governs that, and on anything but
		// the lowest it is given phrases long enough to be worth its while.
		Latency:     "A few seconds, set by the delay setting below",
		Accuracy:    "Best available",
		Benchmark:   "1.3% of words come out wrong",
		Hardware:    "A high-end system. A GPU is strongly recommended — an integrated one is enough — along with the memory for a copy per captioned tuner. It is the heaviest model here by some way, and on a NAS or a low-power box it will not keep up.",
		Runtime:     rtTranscribe,
		Repo:        "handy-computer/cohere-transcribe-03-2026-gguf",
		File:        "cohere-transcribe-03-2026-Q5_K_M.gguf",
		SizeMB:      1760,
		Punctuation: true,
		Languages:   []string{"auto", "de", "en", "es", "fr", "it", "nl", "pl", "pt"},
	},
}

// captionLanguageNames turns the catalog's ISO codes into something readable in
// the language picker.
var captionLanguageNames = map[string]string{
	"auto": "Detect automatically", "bg": "Bulgarian", "cs": "Czech", "da": "Danish",
	"de": "German", "el": "Greek", "en": "English", "es": "Spanish", "et": "Estonian",
	"fi": "Finnish", "fr": "French", "hr": "Croatian", "hu": "Hungarian", "it": "Italian",
	"lt": "Lithuanian", "lv": "Latvian", "mt": "Maltese", "nl": "Dutch", "pl": "Polish",
	"pt": "Portuguese", "ro": "Romanian", "ru": "Russian", "sk": "Slovak", "sl": "Slovenian",
	"sv": "Swedish", "uk": "Ukrainian",
}

func findCaptionModel(key string) (captionModel, bool) {
	for _, m := range captionModelCatalog {
		if m.Key == key {
			return m, true
		}
	}
	return captionModel{}, false
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type captionConfig struct {
	Enabled bool   `json:"enabled"`
	Model   string `json:"model"`
	// Language is an ISO code from the model's catalog entry, or "auto".
	Language string `json:"language"`
	// Style selects the CEA-608 presentation mode.
	Style string `json:"style"`
	// Uppercase renders captions in capitals, which is the long-standing
	// convention for broadcast captioning and is easier to read at a distance.
	// It also squares up the streaming models, which write in lower case.
	Uppercase bool `json:"uppercase"`
	// OffsetSec delays caption display, for trimming sync by hand. It never
	// delays the video: the stream is passed through untouched apart from the
	// caption bytes, so a tune is exactly as fast with captions on as off.
	OffsetSec int `json:"offsetSec"`
	// GPURuntime names driver packages to keep installed in the container, so a
	// GPU build of the engine has something to talk to.
	GPURuntime string `json:"gpuRuntime"`
	// Engine selects which build of the recognizer to run: the processor, or a
	// GPU through Vulkan or CUDA.
	Engine string `json:"engine"`
	// Latency picks how far ahead a streaming model looks before it commits a
	// word. It is the difference between captions a fifth of a second behind
	// and two seconds behind, and between one unit of work per chunk and
	// several, so it matters most when more than one tuner is captioned.
	Latency string `json:"latency"`
	// Tuners restricts captioning to specific tuner indexes. Empty means all.
	Tuners []int `json:"tuners"`
}

func defaultCaptionConfig() captionConfig {
	return captionConfig{
		Enabled:   false,
		Model:     "realtime-multilingual",
		Language:  "en",
		Style:     "rollup3",
		Uppercase: true,
		Engine:    "auto",
		Latency:   "balanced",
		OffsetSec: 0,
	}
}

var (
	captionCfgLock sync.RWMutex
	captionCfg     = defaultCaptionConfig()
)

// warnIfNotPersistent says once, at startup, when downloads will not survive a
// restart. Finding that out after fetching a nine hundred megabyte model is a
// poor way to learn it.
func warnIfNotPersistent() {
	if ok, dir := captionDirPersistent(); !ok {
		logger("[CC] WARNING: %s is not a bind mount. The speech model, engine and any GPU driver downloaded here are lost when the container is recreated.", dir)
		logger("[CC] WARNING: add this volume to your compose file and recreate the container:  ${HOST_DIR}/ah4c/captions:/opt/captions")
	}
}

func loadCaptionConfig() {
	b, err := os.ReadFile(captionCfgFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logger("[CC] Could not read %s: %v", captionCfgFile, err)
		}
		return
	}
	cfg := defaultCaptionConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		logger("[CC] Ignoring malformed %s: %v", captionCfgFile, err)
		return
	}
	captionCfgLock.Lock()
	captionCfg = cfg
	captionCfgLock.Unlock()
	logger("[CC] Loaded config: enabled=%v model=%s language=%s style=%s", cfg.Enabled, cfg.Model, cfg.Language, cfg.Style)
}

func saveCaptionConfig(cfg captionConfig) error {
	// Choosing a GPU build also means keeping its driver in place. Selecting the
	// processor deliberately does not clear that: a downloaded driver is kept
	// working across restarts whatever the engine happens to be set to, so
	// switching to the processor for an evening does not throw it away.
	if v, ok := findEngineVariant(cfg.Engine); ok && v.Key == "vulkan" {
		cfg.GPURuntime = "vulkan"
	}
	if err := os.MkdirAll(captionDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(captionCfgFile, append(b, '\n'), 0o644); err != nil {
		return err
	}
	captionCfgLock.Lock()
	captionCfg = cfg
	captionCfgLock.Unlock()
	return nil
}

func currentCaptionConfig() captionConfig {
	captionCfgLock.RLock()
	defer captionCfgLock.RUnlock()
	return captionCfg
}

// modelURL is where a model's weights are fetched from. It is shown on the page
// as well as used here, so it is always obvious what is being downloaded and
// from whom.
func modelURL(m captionModel) string {
	repo := m.Repo
	if repo == "" {
		repo = captionModelRepo
	}
	return fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", repo, m.File)
}

// modelPath is where a model's weights live once downloaded.
func modelPath(m captionModel) string {
	return filepath.Join(captionModels, m.File)
}

// modelInstalled reports whether the model's weights are on disk.
func modelInstalled(m captionModel) bool {
	st, err := os.Stat(modelPath(m))
	return err == nil && st.Size() > 0
}

// ---------------------------------------------------------------------------
// Model download
// ---------------------------------------------------------------------------

type captionDownload struct {
	Model    string `json:"model"`
	Active   bool   `json:"active"`
	File     string `json:"file"`
	Done     int64  `json:"done"`
	Total    int64  `json:"total"`
	Index    int    `json:"index"`
	Count    int    `json:"count"`
	Err      string `json:"err"`
	Finished bool   `json:"finished"`
}

var (
	dlLock  sync.Mutex
	dlState captionDownload
)

func downloadStatus() captionDownload {
	dlLock.Lock()
	defer dlLock.Unlock()
	return dlState
}

// startModelDownload pulls a model from Hugging Face in the background. It is a
// no-op if a download is already running.
func startModelDownload(m captionModel) error {
	// The page hides a model it cannot run, but the endpoint is reachable
	// without it, and this is the one download where a mistake costs gigabytes.
	if m.NeedsGPU && !gpuAvailable() {
		return fmt.Errorf("%s needs a GPU, and no GPU build can run in this container", m.Name)
	}
	dlLock.Lock()
	if dlState.Active {
		dlLock.Unlock()
		return fmt.Errorf("a download is already running")
	}
	dlState = captionDownload{Model: m.Key, Active: true, Count: 1}
	dlLock.Unlock()

	go func() {
		err := fetchModel(m)
		dlLock.Lock()
		dlState.Active = false
		dlState.Finished = true
		if err != nil {
			dlState.Err = err.Error()
			logger("[CC] Model download failed: %v", err)
		} else {
			logger("[CC] Model %s is ready", m.Key)
		}
		dlLock.Unlock()
	}()
	return nil
}

func fetchModel(m captionModel) error {
	if err := os.MkdirAll(captionModels, 0o755); err != nil {
		return err
	}
	// The download gets its own client: the package-level default in main.go
	// carries a 5 second response header timeout, which is right for an encoder
	// and far too tight for a several hundred megabyte model off a CDN.
	client := &http.Client{Timeout: 2 * time.Hour}
	url := modelURL(m) + "?download=true"
	dlLock.Lock()
	dlState.File, dlState.Index = m.File, 1
	dlLock.Unlock()
	logger("[CC] Downloading %s from %s", m.File, url)
	if err := streamToFile(client, url, modelPath(m)); err != nil {
		return fmt.Errorf("%s: %w", m.File, err)
	}
	return nil
}

// streamToFile downloads url to dst via a temporary name, so an interrupted
// download is never mistaken for an installed file on the next start.
func streamToFile(client *http.Client, url, dst string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	dlLock.Lock()
	dlState.Total = resp.ContentLength
	dlLock.Unlock()

	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	buf := make([]byte, 512*1024)
	var done int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return werr
			}
			done += int64(n)
			dlLock.Lock()
			dlState.Done = done
			dlLock.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// startRuntimeDownload fetches the engine the selected model needs, in the
// background. It is the one piece of native code captions need, and it is
// pulled on demand rather than shipped in the image.
//
// Which engine that is follows from the model rather than from a separate
// choice on the page: picking a model and then being asked which of
// two C++ libraries should run it is not a question anyone wants.
// modelKey may name a model that has not been saved yet, so the page can fetch
// the engine for something it is only considering.
func startRuntimeDownload(variant, modelKey string) error {
	if _, found := findEngineVariant(variant); !found || variant == "auto" {
		variant = currentEngineVariant()
	}
	rt := neededRuntime()
	if m, ok := findCaptionModel(modelKey); ok {
		rt = runtimeOf(m)
	}
	eng := findSpeechRuntime(rt)
	url, dir, lib, ok := runtimeAssetFor(rt, runtime.GOOS, runtime.GOARCH, variant)
	if !ok {
		return fmt.Errorf("no %s build is published for %s/%s", eng.Name, runtime.GOOS, runtime.GOARCH)
	}
	dlLock.Lock()
	if dlState.Active {
		dlLock.Unlock()
		return fmt.Errorf("a download is already running")
	}
	dlState = captionDownload{Model: "engine", Active: true, Count: 1, Index: 1,
		File: eng.Name + " " + eng.Version + " (" + variant + ")"}
	dlLock.Unlock()

	logger("[CC] Downloading %s from %s", eng.Name, url)
	go func() {
		// parakeet.cpp is a single library; transcribe.cpp is a library plus
		// the ggml backends it loads from alongside itself, so that one takes
		// the whole archive.
		err := fetchRuntime(url, dir, lib, rt == rtTranscribe)
		dlLock.Lock()
		dlState.Active = false
		dlState.Finished = true
		if err != nil {
			dlState.Err = err.Error()
			logger("[CC] %s download failed: %v", eng.Name, err)
		} else {
			logger("[CC] %s %s is ready", eng.Name, eng.Version)
		}
		dlLock.Unlock()
	}()
	return nil
}

// fetchRuntime downloads the release archive and unpacks it into dir.
//
// With all set it takes every file, which is what an engine that loads sibling
// libraries needs; otherwise it takes the one named library, matching on file
// name so a change to the directory prefix inside the archive does not break
// the download.
func fetchRuntime(url, dir, lib string, all bool) error {
	dst := filepath.Join(captionRuntime, dir)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	dlLock.Lock()
	dlState.Total = resp.ContentLength
	dlLock.Unlock()

	gz, err := gzip.NewReader(&countingReader{r: resp.Body})
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		isLib := path.Base(h.Name) == lib
		if !all {
			if !isLib {
				continue
			}
			// The one file wanted is here, so stop reading rather than pulling
			// the rest of a half gigabyte archive through the connection.
			if err := writeRuntimeFile(dst, lib, tr); err != nil {
				return err
			}
			return nil
		}
		rel, ok := archiveRelPath(h.Name)
		if !ok {
			continue
		}
		if err := writeRuntimeFile(dst, rel, tr); err != nil {
			return err
		}
		found = found || isLib
	}
	if !found {
		return fmt.Errorf("%s was not found in the archive", lib)
	}
	return nil
}

// archiveRelPath drops the archive's own top-level directory, so what lands on
// disk is not named after the release it came out of, and refuses anything that
// would climb out of the destination.
func archiveRelPath(name string) (string, bool) {
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	i := strings.IndexByte(clean, '/')
	if i < 0 {
		return "", false
	}
	rel := clean[i+1:]
	if rel == "" || rel == "." {
		return "", false
	}
	return rel, true
}

// writeRuntimeFile writes one archive entry through a temporary name, so an
// interrupted download never leaves a half library looking installed.
func writeRuntimeFile(dir, rel string, r io.Reader) error {
	dst := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// countingReader reports download progress for a stream that is being
// decompressed on the fly.
type countingReader struct {
	r    io.Reader
	done int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		dlLock.Lock()
		dlState.Done = c.done
		dlLock.Unlock()
	}
	return n, err
}

// removeCaptionModel deletes a downloaded model, freeing the several hundred
// megabytes it occupies.
func removeCaptionModel(m captionModel) error {
	dlLock.Lock()
	active := dlState.Active && dlState.Model == m.Key
	dlLock.Unlock()
	if active {
		return fmt.Errorf("that model is still downloading")
	}
	if err := os.Remove(modelPath(m)); err != nil && !os.IsNotExist(err) {
		return err
	}
	logger("[CC] Removed model %s", m.Key)
	return nil
}

// ---------------------------------------------------------------------------
// GPU driver support
// ---------------------------------------------------------------------------

// A GPU build of the engine needs a driver in the container, and the base image
// ships none. That is handled the same way the model is: the packages are
// downloaded once into the bind mount, where they survive a rebuild, and put
// back in place at startup from that copy without touching the network again.
// Nobody who leaves captions off pays anything for it.
//
// The packages are the distribution's own rather than files picked by hand. A
// Vulkan driver pulls in a dozen libraries, and letting the package manager
// work out which is the difference between something that runs on other
// people's machines and something that runs on mine.

type gpuRuntime struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Desc     string   `json:"desc"`
	Packages []string `json:"packages"`
	// Needs is the library whose presence proves the driver is in place.
	Needs string `json:"needs"`
	Note  string `json:"note"`
}

var gpuRuntimes = []gpuRuntime{
	{
		Key:      "vulkan",
		Name:     "Vulkan driver",
		Desc:     "The Vulkan loader and the open source drivers, which cover Intel and AMD graphics. On NVIDIA the container runtime brings its own driver and only the loader is used.",
		Packages: []string{"libvulkan1", "mesa-vulkan-drivers"},
		Needs:    "libvulkan.so.1",
		Note:     "Your compose file also has to pass the graphics device through, with a devices entry for /dev/dri.",
	},
}

func findGPURuntime(key string) (gpuRuntime, bool) {
	for _, g := range gpuRuntimes {
		if g.Key == key {
			return g, true
		}
	}
	return gpuRuntime{}, false
}

func driverDir(g gpuRuntime) string { return filepath.Join(captionDrivers, g.Key) }

// driverDownloaded reports whether the packages are sitting in the bind mount,
// which is what survives a rebuild.
func driverDownloaded(g gpuRuntime) bool {
	ents, err := os.ReadDir(driverDir(g))
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".deb") {
			return true
		}
	}
	return false
}

// driverActive reports whether the driver is loadable right now.
func driverActive(g gpuRuntime) bool {
	h, err := purego.Dlopen(g.Needs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	return err == nil && h != 0
}

type gpuInstallState struct {
	Kind     string `json:"kind"`
	Active   bool   `json:"active"`
	Finished bool   `json:"finished"`
	Err      string `json:"err"`
	Log      string `json:"log"`
}

var (
	gpuLock  sync.Mutex
	gpuState gpuInstallState
)

func gpuInstallStatus() gpuInstallState {
	gpuLock.Lock()
	defer gpuLock.Unlock()
	return gpuState
}

// startDriverDownload fetches the packages into the bind mount and puts them in
// place, in the background.
func startDriverDownload(kind string) error {
	g, ok := findGPURuntime(kind)
	if !ok {
		return fmt.Errorf("unknown driver %q", kind)
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("drivers are only installable inside the container")
	}
	gpuLock.Lock()
	if gpuState.Active {
		gpuLock.Unlock()
		return fmt.Errorf("a driver download is already running")
	}
	gpuState = gpuInstallState{Kind: kind, Active: true}
	gpuLock.Unlock()

	go func() {
		log, err := fetchDriver(g)
		if err == nil {
			var l2 string
			l2, err = applyDriver(g)
			log += l2
		}
		gpuLock.Lock()
		gpuState.Active = false
		gpuState.Finished = true
		gpuState.Log = tailLines(log, 12)
		if err != nil {
			gpuState.Err = err.Error()
			logger("[CC] %s could not be set up: %v", g.Name, err)
		} else {
			logger("[CC] %s is ready", g.Name)
			// Whether a GPU build can load is cached, and installing a
			// driver is the one moment that answer changes.
			forgetEngineUsable()
			// Record that this driver is wanted. Downloading it is the only
			// point at which the intent is expressed: the engine picker will
			// not offer a GPU build until the driver already loads, so waiting
			// for that selection to save the choice means it is never saved and
			// the driver disappears on the next restart.
			cfg := currentCaptionConfig()
			if cfg.GPURuntime != g.Key {
				cfg.GPURuntime = g.Key
				if e := saveCaptionConfig(cfg); e != nil {
					logger("[CC] Could not record the driver choice: %v", e)
				}
			}
		}
		gpuLock.Unlock()
	}()
	return nil
}

// fetchDriver downloads the packages and everything they depend on into the
// bind mount. Only this step needs the network.
func fetchDriver(g gpuRuntime) (string, error) {
	if _, err := exec.LookPath("apt-get"); err != nil {
		return "", fmt.Errorf("this image has no apt-get, so the driver cannot be fetched from here")
	}
	dir := driverDir(g)
	// apt downloads into a partial subdirectory of its archive cache and moves
	// the finished files up. Without that directory it refuses to start, which
	// is the difference between the packages landing in the bind mount and not
	// being downloaded at all.
	if err := os.MkdirAll(filepath.Join(dir, "partial"), 0o755); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	var log strings.Builder
	run := func(args ...string) error {
		logger("[CC] %v", args)
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		log.Write(b)
		return e
	}
	if err := run("apt-get", "update"); err != nil {
		return log.String(), fmt.Errorf("apt-get update: %w", err)
	}
	args := append([]string{"apt-get", "install", "-y", "--no-install-recommends",
		"--reinstall", "--download-only", "-o", "Dir::Cache::archives=" + abs}, g.Packages...)
	aptErr := run(args...)

	// Whether or not apt honoured the archive directory, the packages have to
	// end up in the bind mount, because that is the only thing a rebuild does
	// not erase. If they went to the default cache instead, move them.
	if !driverDownloaded(g) {
		if n := harvestDebs(dir); n > 0 {
			logger("[CC] Recovered %d packages from the default apt cache", n)
		}
	}
	if !driverDownloaded(g) {
		if aptErr != nil {
			return log.String(), fmt.Errorf("downloading %s: %w", strings.Join(g.Packages, " "), aptErr)
		}
		return log.String(), fmt.Errorf("no packages ended up in %s", dir)
	}
	n, _ := savedDebs(g)
	logger("[CC] Saved %d packages for %s in %s", len(n), g.Name, dir)
	return log.String(), nil
}

// harvestDebs copies anything apt left in the default cache into the bind
// mount, and returns how many it moved.
func harvestDebs(dir string) int {
	ents, err := os.ReadDir("/var/cache/apt/archives")
	if err != nil {
		return 0
	}
	moved := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		src := filepath.Join("/var/cache/apt/archives", e.Name())
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644) == nil {
			moved++
		}
	}
	return moved
}

// savedDebs lists the packages held for a driver.
func savedDebs(g gpuRuntime) ([]string, error) {
	ents, err := os.ReadDir(driverDir(g))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".deb") {
			out = append(out, filepath.Join(driverDir(g), e.Name()))
		}
	}
	return out, nil
}

// applyDriver unpacks the saved packages into the running container. This is
// what runs after a rebuild, and it needs no network.
func applyDriver(g gpuRuntime) (string, error) {
	debs, err := savedDebs(g)
	if err != nil {
		return "", err
	}
	if len(debs) == 0 {
		return "", fmt.Errorf("no saved packages to install")
	}
	logger("[CC] Installing %d saved packages for %s", len(debs), g.Name)
	cmd := exec.Command("dpkg", append([]string{"-i", "--force-depends"}, debs...)...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	if err != nil && !driverActive(g) {
		return string(out), fmt.Errorf("installing the saved packages: %w", err)
	}
	if !driverActive(g) {
		return string(out), fmt.Errorf("%s still will not load", g.Needs)
	}
	return string(out), nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// captionDirPersistent reports whether the caption directory survives the
// container being recreated.
//
// It is a plain directory inside the image unless the compose file binds it to
// the host, and a container that is brought down and up again starts from the
// image. Downloading several hundred megabytes into somewhere that evaporates
// is a miserable way to find that out, so it is checked and said out loud.
func captionDirPersistent() (bool, string) {
	abs, err := filepath.Abs(captionDir)
	if err != nil {
		return true, ""
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		// Not a Linux container; nothing to warn about.
		return true, ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// The mount point is the fifth field.
		fields := strings.Fields(sc.Text())
		if len(fields) >= 5 && fields[4] == abs {
			return true, ""
		}
	}
	return false, abs
}

// renderNodes lists the graphics devices visible inside the container.
//
// A container gets no device nodes unless the compose file passes them, so this
// is usually the reason a GPU build sees nothing: the driver is installed, the
// card is in the machine, and /dev/dri simply is not here.
func renderNodes() []string {
	ents, err := os.ReadDir("/dev/dri")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "render") || strings.HasPrefix(e.Name(), "card") {
			out = append(out, "/dev/dri/"+e.Name())
		}
	}
	return out
}

// accelReport is what the page shows about hardware acceleration: not just
// whether it was asked for, but whether each thing it depends on is actually
// there.
type accelReport struct {
	Variant  string   `json:"variant"`
	Active   bool     `json:"active"`
	Headline string   `json:"headline"`
	Detail   string   `json:"detail"`
	Devices  []string `json:"devices"`
}

// accelStatus works out whether the GPU is really going to be used, and if not,
// which of the pieces is missing.
func accelStatus() accelReport {
	cfg := currentCaptionConfig()
	v, ok := findEngineVariant(cfg.Engine)
	r := accelReport{Variant: cfg.Engine, Devices: renderNodes()}
	if !ok || v.Key == "cpu" {
		r.Headline = "Running on the processor"
		r.Detail = "No GPU build is selected. This is fine for a few tuners at once; a GPU build spares the cores and the heat when many are running."
		return r
	}
	switch {
	case !engineUsable(v):
		r.Headline = "Not accelerated: the driver is missing"
		r.Detail = fmt.Sprintf("%s is selected but %s will not load. Download the driver below.", v.Name, v.Needs)
	case v.Key == "vulkan" && len(r.Devices) == 0:
		r.Headline = "Not accelerated: no graphics device in the container"
		r.Detail = "The driver is in place but /dev/dri is not here. Add a devices entry for /dev/dri to your compose file and recreate the container."
	case !engineInstalled():
		r.Headline = "Not accelerated: the engine build is not downloaded"
		r.Detail = fmt.Sprintf("Download the %s build above.", v.Name)
	default:
		r.Active = true
		r.Headline = "Hardware acceleration is active: " + v.Name
		if len(r.Devices) > 0 {
			r.Detail = "Using " + strings.Join(r.Devices, ", ") + ". The engine falls back to the processor by itself if the device stops answering, so captions keep working either way."
		} else {
			r.Detail = "The engine falls back to the processor by itself if the device stops answering, so captions keep working either way."
		}
	}
	return r
}

// gpuAvailable reports whether any GPU build could actually run here: its
// driver loads, and for Vulkan a graphics device is present as well. It asks
// the same questions accelStatus does, but about every build rather than the
// selected one, because what it decides is whether a model that cannot work
// without a GPU is offered at all.
//
// It deliberately does not care whether that build has been downloaded yet.
// Refusing to show a model because the engine it would use is still a button
// press away would be a maze rather than a gate.
func gpuAvailable() bool {
	nodes := renderNodes()
	for _, v := range engineVariants {
		if v.Key == "auto" || v.Key == "cpu" || !engineUsable(v) {
			continue
		}
		if v.Key == "vulkan" && len(nodes) == 0 {
			continue
		}
		return true
	}
	return false
}

// restoreGPURuntime puts the driver back after a container rebuild, from the
// copy in the bind mount. Called at startup, so the choice survives without
// anyone pressing anything or the network being reachable.
func restoreGPURuntime() {
	if runtime.GOOS != "linux" {
		return
	}
	cfg := currentCaptionConfig()
	for _, g := range gpuRuntimes {
		// Anything with packages saved in the bind mount gets put back, whether
		// or not the config remembers asking for it. The packages being there
		// is the intent; a rebuild wipes the installed copy but not them.
		if !driverDownloaded(g) || driverActive(g) {
			continue
		}
		logger("[CC] %s is saved but not loaded, restoring it from %s", g.Name, driverDir(g))
		if out, err := applyDriver(g); err != nil {
			logger("[CC] %s could not be restored: %v %s", g.Name, err, tailLines(out, 6))
			continue
		}
		logger("[CC] %s restored", g.Name)
		if cfg.GPURuntime != g.Key {
			cfg.GPURuntime = g.Key
			saveCaptionConfig(cfg)
		}
	}
}

// ---------------------------------------------------------------------------
// CEA-608 encoding
// ---------------------------------------------------------------------------

// CEA-608 carries two bytes per video frame on field 1. At 29.97 fps that is
// about 60 characters a second, comfortably ahead of the ~13 characters a
// second that ordinary speech produces, so the queue below drains faster than
// the recognizer fills it.

const (
	cc608Null = 0x00 // padding, before parity

	ccCtrlCC1 = 0x14 // control code channel prefix for CC1
	ccRU2     = 0x25 // roll up 2 rows
	ccRU3     = 0x26 // roll up 3 rows
	ccRU4     = 0x27 // roll up 4 rows
	ccCR      = 0x2D // carriage return
	ccEDM     = 0x2C // erase displayed memory
	ccEOC     = 0x2F // end of caption (pop-on flip)
	ccRCL     = 0x20 // resume caption loading
	ccENM     = 0x2E // erase non-displayed memory
)

// odd608 sets bit 7 so the byte carries odd parity, which is what a 608 decoder
// expects on the wire.
func odd608(b byte) byte {
	v := b & 0x7F
	ones := 0
	for i := 0; i < 7; i++ {
		if v&(1<<i) != 0 {
			ones++
		}
	}
	if ones%2 == 0 {
		return v | 0x80
	}
	return v
}

// cc608Char maps a rune onto the CEA-608 basic character set.
//
// That set is ASCII with a handful of positions given over to accented letters:
// á é í ó ú ç ñ Ñ are carried natively and are emitted as themselves. The rest
// of Europe's letters have no code point here, and dropping one leaves a hole
// in the middle of a word — "café" arrived as "caf ", which reads as a typo
// rather than as a missing glyph — so those are folded to the nearest letter a
// viewer would recognise.
//
// The ASCII characters occupying the accented positions have to be blanked
// rather than passed through, or an asterisk would be shown as an á.
func cc608Char(r rune) byte {
	if b, ok := cc608Native[r]; ok {
		return b
	}
	switch {
	case r >= 0x20 && r <= 0x7F:
		switch r {
		case '*', '\\', '^', '_', '`', '{', '|', '}', '~', 0x7F:
			// These positions carry á é í ó ú ç ÷ Ñ ñ and a solid block.
			return ' '
		}
		return byte(r)
	}
	if b, ok := cc608Fold[r]; ok {
		return b
	}
	return ' '
}

// cc608Native are the letters the basic character set carries in place of
// certain ASCII codes.
var cc608Native = map[rune]byte{
	'á': 0x2A, 'é': 0x5C, 'í': 0x5E, 'ó': 0x5F, 'ú': 0x60,
	'ç': 0x7B, '÷': 0x7C, 'Ñ': 0x7D, 'ñ': 0x7E,
}

// cc608Fold folds the letters the European languages need onto the basic set.
// It is not a transliteration scheme, just the nearest letter a viewer would
// recognise, which is what a caption needs.
var cc608Fold = map[rune]byte{
	'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'Á': 'A', 'À': 'A', 'Â': 'A', 'Ä': 'A', 'Ã': 'A', 'Å': 'A', 'Ā': 'A', 'Ă': 'A', 'Ą': 'A',
	'è': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'É': 'E', 'È': 'E', 'Ê': 'E', 'Ë': 'E', 'Ē': 'E', 'Ė': 'E', 'Ę': 'E', 'Ě': 'E',
	'ì': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i', 'į': 'i', 'ı': 'i',
	'Í': 'I', 'Ì': 'I', 'Î': 'I', 'Ï': 'I', 'Ī': 'I', 'Į': 'I', 'İ': 'I',
	'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o', 'ø': 'o', 'ō': 'o', 'ő': 'o',
	'Ó': 'O', 'Ò': 'O', 'Ô': 'O', 'Ö': 'O', 'Õ': 'O', 'Ø': 'O', 'Ō': 'O', 'Ő': 'O',
	'ù': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'Ú': 'U', 'Ù': 'U', 'Û': 'U', 'Ü': 'U', 'Ū': 'U', 'Ů': 'U', 'Ű': 'U', 'Ų': 'U',
	'ý': 'y', 'ÿ': 'y', 'Ý': 'Y', 'Ŷ': 'Y', 'ŷ': 'y',
	'ń': 'n', 'ň': 'n', 'ņ': 'n', 'Ń': 'N', 'Ň': 'N', 'Ņ': 'N',
	'ć': 'c', 'č': 'c', 'ĉ': 'c', 'Ç': 'C', 'Ć': 'C', 'Č': 'C', 'Ĉ': 'C',
	'š': 's', 'ś': 's', 'ş': 's', 'ŝ': 's', 'Š': 'S', 'Ś': 'S', 'Ş': 'S', 'Ŝ': 'S',
	'ž': 'z', 'ź': 'z', 'ż': 'z', 'Ž': 'Z', 'Ź': 'Z', 'Ż': 'Z',
	'ł': 'l', 'ľ': 'l', 'ĺ': 'l', 'ļ': 'l', 'Ł': 'L', 'Ľ': 'L', 'Ĺ': 'L', 'Ļ': 'L',
	'ť': 't', 'ţ': 't', 'ŧ': 't', 'Ť': 'T', 'Ţ': 'T', 'Ŧ': 'T',
	'ď': 'd', 'đ': 'd', 'ð': 'd', 'Ď': 'D', 'Đ': 'D', 'Ð': 'D',
	'ř': 'r', 'ŕ': 'r', 'Ř': 'R', 'Ŕ': 'R',
	'ğ': 'g', 'ģ': 'g', 'ġ': 'g', 'Ğ': 'G', 'Ģ': 'G', 'Ġ': 'G',
	'ķ': 'k', 'Ķ': 'K', 'ħ': 'h', 'Ħ': 'H',
	'þ': 'p', 'Þ': 'P', 'ŭ': 'u', 'Ŭ': 'U',
	'‘': '\'', '’': '\'', '‚': '\'', '‹': '<', '›': '>',
	'“': '"', '”': '"', '„': '"', '«': '"', '»': '"',
	'—': '-', '–': '-', '‑': '-', '−': '-',
	'…': '.', '·': '.', '•': '-',
	'\u00a0': ' ', '\u202f': ' ', '\u2009': ' ',
}

// cc608Expand spells out the characters that carry meaning and have no letter
// to fold onto, so a price or a fraction is not silently blanked.
var cc608Expand = map[rune]string{
	'€': "EUR", '£': "GBP", '¥': "YEN", '¢': "c", '¤': "",
	'æ': "ae", 'Æ': "AE", 'œ': "oe", 'Œ': "OE", 'ß': "ss",
	'½': "1/2", '¼': "1/4", '¾': "3/4", '°': " degrees",
	'±': "+/-", '×': "x", '™': "(TM)", '©': "(C)", '®': "(R)",
	'µ': "u", '§': "S", '¶': "P", '†': "+", '‡': "++",
}

// cc608ExpandText spells out the characters that have no single letter to fold
// onto, before the text is laid out on the caption grid.
func cc608ExpandText(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { _, ok := cc608Expand[r]; return ok }) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 8)
	for _, r := range text {
		if s, ok := cc608Expand[r]; ok {
			b.WriteString(s)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cea608 turns lines of recognized text into the byte pairs that ride in the
// caption stream, one pair per video frame.
type cea608 struct {
	mu    sync.Mutex
	queue [][2]byte
	// lastText is when text was last queued. A caption left on screen after
	// everything upstream has stopped is worse than no caption: it is a
	// sentence from four minutes ago presented as if it were current. A
	// broadcast encoder erases after a while and so does this.
	lastText time.Time
	rows     byte // ccRU2 / ccRU3 / ccRU4
	started  bool
	col      int
	maxCol   int
	upper    bool
}

func newCEA608(style string, upper bool) *cea608 {
	rows := byte(ccRU3)
	switch style {
	case "rollup2":
		rows = ccRU2
	case "rollup4":
		rows = ccRU4
	}
	return &cea608{rows: rows, maxCol: 32, upper: upper}
}

func (c *cea608) ctrl(code byte) {
	// Control codes are sent twice; a decoder that catches the pair twice acts
	// on it once, and the repeat is what survives a dropped frame.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(code)}, [2]byte{odd608(ccCtrlCC1), odd608(code)})
}

// begin puts the decoder into roll-up mode on the bottom row.
func (c *cea608) begin() {
	c.mode()
	c.ctrl(ccCR)
	c.started = true
	c.col = 0
}

// mode states the roll-up style and the row to write on.
func (c *cea608) mode() {
	c.ctrl(c.rows)
	// Preamble address code for row 15, column 0, white non-italic: 0x14 0x70.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(0x70)}, [2]byte{odd608(ccCtrlCC1), odd608(0x70)})
}

// newRow ends the current row and restates the mode.
//
// A decoder holds the caption style and the row as state, and it only learns
// them from these commands. Sending them once at the start of a stream is
// enough for somebody watching from the start and no use to anybody else: seek
// into a recording, or switch captions on an hour in, and the decoder has
// nothing to go on until the next one arrives. Restating them on every row
// gives a receiver joining at any point somewhere to latch on within a second,
// which is what a broadcast encoder does and why its captions survive a
// channel change.
func (c *cea608) newRow() {
	c.ctrl(ccCR)
	c.mode()
	c.col = 0
}

// ccMaxBacklog is the most unshown caption data we will hold, in byte pairs.
// The channel moves two bytes per picture, so at 30 fps this is about five
// seconds of text. Reaching it means recognition has outrun the display.
const ccMaxBacklog = 150

// push queues a complete phrase and closes the line after it.
func (c *cea608) push(text string) { c.pushText(text, true) }

// pushText queues text, wrapping it to the 32 column caption grid, and ends the
// line only if breakAfter is set.
//
// A streaming model finalizes a few words at a time, and each of those is a
// continuation of the sentence being spoken rather than a line of its own, so
// the line is closed on the end of an utterance instead of on every arrival.
func (c *cea608) pushText(text string, breakAfter bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	text = cc608ExpandText(text)
	if c.upper {
		text = strings.ToUpper(text)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Captions have to track what is being said now. If a burst of recognition
	// has queued more than the channel can carry, showing all of it would put
	// the text permanently behind the picture, so drop what has not aired and
	// start the roll-up again on the current phrase.
	if len(c.queue) > ccMaxBacklog {
		c.queue = c.queue[:0]
		c.started = false
		c.col = 0
	}
	if !c.started {
		c.begin()
	}
	c.lastText = time.Now()
	for _, w := range strings.Fields(text) {
		runes := []rune(w)
		if c.col > 0 && c.col+1+len(runes) > c.maxCol {
			c.newRow()
		}
		if c.col > 0 {
			c.writeRune(' ')
		}
		for _, r := range runes {
			if c.col >= c.maxCol {
				c.newRow()
			}
			c.writeRune(r)
		}
	}
	if breakAfter {
		// Finish the phrase on its own line so the next one rolls up under it.
		c.newRow()
	}
}

// writeRune appends a character, pairing it with the previous one where it can.
// 608 always moves two bytes at a time, so a lone character is padded.
func (c *cea608) writeRune(r rune) {
	b := cc608Char(r)
	if n := len(c.queue); n > 0 && c.queue[n-1][1] == odd608(cc608Null) && c.queue[n-1][0] != odd608(ccCtrlCC1) {
		c.queue[n-1][1] = odd608(b)
	} else {
		c.queue = append(c.queue, [2]byte{odd608(b), odd608(cc608Null)})
	}
	c.col++
}

// clear wipes the display, used when the stream goes quiet for a while.
// ccStaleAfter is how long a caption stays on screen with nothing behind it.
//
// Long enough to sit through a musical interlude or a quiet scene without the
// text flickering away, short enough that a viewer is never reading a sentence
// that stopped being true minutes ago. It also means a failure upstream now
// looks like a failure — a blank line — instead of looking like a caption.
const ccStaleAfter = 20 * time.Second

// next returns the pair of bytes to attach to the next video frame.
func (c *cea608) next() [2]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		if c.started && !c.lastText.IsZero() && time.Since(c.lastText) > ccStaleAfter {
			c.ctrl(ccEDM)
			c.started = false
			c.col = 0
			c.lastText = time.Time{}
			if len(c.queue) > 0 {
				p := c.queue[0]
				c.queue = c.queue[1:]
				return p
			}
		}
		return [2]byte{odd608(cc608Null), odd608(cc608Null)}
	}
	p := c.queue[0]
	c.queue = c.queue[1:]
	return p
}

func (c *cea608) backlog() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// ---------------------------------------------------------------------------
// ATSC A/53 caption user data and SEI
// ---------------------------------------------------------------------------

// buildCCData assembles the cc_data() structure from A/53 Part 4. ccCount is
// fixed by the frame rate: the caption channel runs at 9600 bits per second, so
// each frame carries 600/fps constructs of two bytes each.
func buildCCData(pair [2]byte, ccCount int) []byte {
	if ccCount < 2 {
		ccCount = 2
	}
	if ccCount > 31 {
		ccCount = 31
	}
	b := make([]byte, 0, 5+3*ccCount+1)
	b = append(b,
		0xB5,       // itu_t_t35_country_code, United States
		0x00, 0x31, // itu_t_t35_provider_code, ATSC
		'G', 'A', '9', '4', // user_identifier
		0x03, // user_data_type_code, cc_data
	)
	// process_em_data_flag=1, process_cc_data_flag=1, additional_data_flag=0
	b = append(b, 0xC0|byte(ccCount))
	b = append(b, 0xFF) // em_data

	for i := 0; i < ccCount; i++ {
		// marker_bits(5)=11111, cc_valid(1), cc_type(2)
		switch i {
		case 0: // field 1: the 608 bytes we actually care about
			b = append(b, 0xFC, pair[0], pair[1])
		case 1:
			// Field 2 is marked not valid. Nothing is ever written to it, and
			// claiming otherwise makes a player advertise a second caption
			// service, and the 708 services derived from the pair, so a viewer
			// is offered four tracks where only the first has anything in it.
			b = append(b, 0xF9, 0x00, 0x00)
		default: // 708 channel padding, marked invalid
			b = append(b, 0xFA, 0x00, 0x00)
		}
	}
	b = append(b, 0xFF) // marker_bits
	return b
}

// emulationPrevention inserts the 0x03 escape bytes that keep a NAL payload
// from accidentally containing a start code.
func emulationPrevention(in []byte) []byte {
	out := make([]byte, 0, len(in)+len(in)/64+8)
	zeros := 0
	for _, b := range in {
		if zeros >= 2 && b <= 0x03 {
			out = append(out, 0x03)
			zeros = 0
		}
		out = append(out, b)
		if b == 0x00 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// seiPayloadSize writes an SEI size using the spec's 255-at-a-time encoding.
func seiPayloadSize(n int) []byte {
	var out []byte
	for n >= 255 {
		out = append(out, 0xFF)
		n -= 255
	}
	return append(out, byte(n))
}

// buildCaptionSEI produces a complete Annex-B NAL, start code included, that
// carries the frame's caption bytes as registered ITU-T T.35 user data.
func buildCaptionSEI(pair [2]byte, ccCount int, hevc bool) []byte {
	payload := buildCCData(pair, ccCount)

	rbsp := make([]byte, 0, len(payload)+8)
	rbsp = append(rbsp, 0x04) // payloadType 4, user_data_registered_itu_t_t35
	rbsp = append(rbsp, seiPayloadSize(len(payload))...)
	rbsp = append(rbsp, payload...)
	rbsp = append(rbsp, 0x80) // rbsp_trailing_bits

	nal := make([]byte, 0, len(rbsp)+6)
	nal = append(nal, 0x00, 0x00, 0x00, 0x01)
	if hevc {
		// nal_unit_type 39 (PREFIX_SEI_NUT), layer 0, temporal id 0.
		nal = append(nal, 0x4E, 0x01)
	} else {
		// nal_ref_idc 0, nal_unit_type 6 (SEI).
		nal = append(nal, 0x06)
	}
	return append(nal, emulationPrevention(rbsp)...)
}

// ---------------------------------------------------------------------------
// Transport stream parsing and injection
// ---------------------------------------------------------------------------

const tsPacketSize = 188

const (
	streamTypeH264 = 0x1B
	streamTypeHEVC = 0x24
)

// nalStarts finds every Annex-B start code offset in an elementary stream.
func nalStarts(es []byte) []int {
	var out []int
	for i := 0; i+3 < len(es); i++ {
		if es[i] == 0 && es[i+1] == 0 {
			if es[i+2] == 1 {
				out = append(out, i)
				i += 2
			} else if es[i+2] == 0 && i+3 < len(es) && es[i+3] == 1 {
				out = append(out, i)
				i += 3
			}
		}
	}
	return out
}

// nalHeaderAt returns the payload offset and NAL type for the start code at p.
func nalHeaderAt(es []byte, p int, hevc bool) (int, int, bool) {
	off := p + 3
	if es[p+2] == 0 {
		off = p + 4
	}
	if off >= len(es) {
		return 0, 0, false
	}
	if hevc {
		return off, int((es[off] >> 1) & 0x3F), true
	}
	return off, int(es[off] & 0x1F), true
}

// isVCL reports whether a NAL type carries coded picture data.
func isVCL(t int, hevc bool) bool {
	if hevc {
		return t <= 31
	}
	return t >= 1 && t <= 5
}

// injectSEI places a caption SEI immediately before the first coded slice of
// the access unit, which is where A/53 requires it and where every decoder
// looks. Returns the elementary stream unchanged if no slice is found.
func injectSEI(es []byte, sei []byte, hevc bool) []byte {
	for _, p := range nalStarts(es) {
		off, t, ok := nalHeaderAt(es, p, hevc)
		if !ok {
			continue
		}
		_ = off
		if isVCL(t, hevc) {
			out := make([]byte, 0, len(es)+len(sei))
			out = append(out, es[:p]...)
			out = append(out, sei...)
			out = append(out, es[p:]...)
			return out
		}
	}
	return es
}

// hasCaptionSEI reports whether the access unit already carries A/53 captions,
// in which case we leave the stream alone rather than fighting the source.
func hasCaptionSEI(es []byte, hevc bool) bool {
	for _, p := range nalStarts(es) {
		off, t, ok := nalHeaderAt(es, p, hevc)
		if !ok {
			continue
		}
		seiType := 6
		if hevc {
			seiType = 39
		}
		if t != seiType {
			continue
		}
		hdr := 1
		if hevc {
			hdr = 2
		}
		// A start code can sit close enough to the end that the NAL header runs
		// past it. That only happens on a stream truncated mid-packet, but it
		// is a slice out of range when it does.
		if off+hdr > len(es) {
			continue
		}
		body := es[off+hdr:]
		if len(body) > 12 && body[0] == 0x04 {
			for i := 1; i < len(body)-8 && i < 12; i++ {
				if body[i] == 0xB5 && body[i+1] == 0x00 && body[i+2] == 0x31 &&
					body[i+3] == 'G' && body[i+4] == 'A' && body[i+5] == '9' && body[i+6] == '4' {
					return true
				}
			}
		}
	}
	return false
}

// tsPacket is one 188 byte packet held while an access unit is assembled.
type tsPacket struct {
	buf   [tsPacketSize]byte
	video bool
	// payload is false for a packet that carries only an adaptation field.
	// Those hold the clock rather than the picture, and must be passed through
	// where they are rather than rebuilt or dropped.
	payload bool
}

// captionInjector rewrites a transport stream in place, adding caption bytes to
// each video access unit and passing every other packet through untouched.
type captionInjector struct {
	out      io.Writer
	enc      *cea608
	log      string
	videoPID int
	pmtPID   int
	hevc     bool

	window   []tsPacket // packets held for the access unit being assembled
	pes      []byte     // the video PES currently being reassembled
	inPES    bool
	videoCC  byte
	ccSeeded bool // whether videoCC has picked up the source's count

	carry []byte // bytes of a packet split across two Write calls
	// pmtPatch is the programme table rewritten to announce the caption
	// service; pmtDone records that the attempt has been made.
	pmtPatch []byte
	pmtDone  bool

	ccCount  int
	lastPTS  int64
	haveRate bool

	frames   int64
	injected int64
	warned   bool
}

func newCaptionInjector(out io.Writer, enc *cea608, label string) *captionInjector {
	return &captionInjector{
		out:      out,
		enc:      enc,
		log:      label,
		videoPID: -1,
		pmtPID:   -1,
		ccCount:  20, // 29.97 fps until the stream tells us otherwise
	}
}

// Write consumes transport stream bytes.
//
// Reads off a socket land wherever they land, so a packet routinely straddles
// two calls. The tail of a short call is carried over and completed by the next
// one; letting a split packet through unparsed would leave it out of the access
// unit while its bytes still reached the output, which corrupts the picture.
func (ci *captionInjector) Write(b []byte) (int, error) {
	consumed := len(b)

	if len(ci.carry) > 0 {
		need := tsPacketSize - len(ci.carry)
		if len(b) < need {
			ci.carry = append(ci.carry, b...)
			return consumed, nil
		}
		ci.carry = append(ci.carry, b[:need]...)
		b = b[need:]
		if err := ci.packet(ci.carry); err != nil {
			return consumed, err
		}
		ci.carry = ci.carry[:0]
	}

	for len(b) > 0 {
		if b[0] != 0x47 {
			// Resynchronize on the next sync byte rather than corrupting
			// output. A lone 0x47 is not enough: the byte 188 further on has to
			// be one too, or this locks onto a coincidence inside the picture
			// data and feeds it to the reassembler as if it were a packet.
			i := 1
			for i < len(b) {
				if b[i] == 0x47 && (i+tsPacketSize >= len(b) || b[i+tsPacketSize] == 0x47) {
					break
				}
				i++
			}
			if _, err := ci.out.Write(b[:i]); err != nil {
				return consumed, err
			}
			b = b[i:]
			continue
		}
		if len(b) < tsPacketSize {
			ci.carry = append(ci.carry[:0], b...)
			break
		}
		if err := ci.packet(b[:tsPacketSize]); err != nil {
			return consumed, err
		}
		b = b[tsPacketSize:]
	}
	return consumed, nil
}

// Flush emits whatever is still held for the access unit in flight. Without it
// the tail of the stream, including the audio packets interleaved into the last
// window, would be dropped when the source ends.
func (ci *captionInjector) Flush() error {
	if err := ci.passthroughWindow(); err != nil {
		return err
	}
	if len(ci.carry) > 0 {
		if _, err := ci.out.Write(ci.carry); err != nil {
			return err
		}
		ci.carry = ci.carry[:0]
	}
	return nil
}

func (ci *captionInjector) packet(p []byte) error {
	pid := int(p[1]&0x1F)<<8 | int(p[2])
	pusi := p[1]&0x40 != 0

	switch {
	case pid == 0:
		ci.parsePAT(p)
	case pid == ci.pmtPID:
		ci.parsePMT(p)
	}

	// Announce the caption service in the programme table, so a player that
	// does not decode the video to look for caption messages still knows they
	// are there.
	if pid == ci.pmtPID && ci.videoPID >= 0 && !ci.pmtDone {
		if q := addCaptionDescriptor(p, ci.videoPID); q != nil {
			ci.pmtPatch = q
			logger("[CC] %s announced the caption service in the programme table", ci.log)
		}
		ci.pmtDone = true
	}
	if pid == ci.pmtPID && ci.pmtPatch != nil {
		if ci.inPES {
			var t tsPacket
			copy(t.buf[:], ci.pmtPatch)
			ci.window = append(ci.window, t)
			return nil
		}
		_, err := ci.out.Write(ci.pmtPatch)
		return err
	}

	if ci.videoPID < 0 || pid != ci.videoPID {
		// Not video: hold it in the window so ordering survives, or write it
		// straight out when no access unit is in flight.
		if ci.inPES {
			var t tsPacket
			copy(t.buf[:], p)
			ci.window = append(ci.window, t)
			return nil
		}
		_, err := ci.out.Write(p)
		return err
	}

	// Video packets that arrived before the PMT identified the video PID went
	// out untouched, carrying the source's own count. Pick that count up so the
	// handover leaves no gap.
	//
	// Only a packet that carries payload may be used for it. The counter names
	// the value the next payload packet should take, and a packet without
	// payload repeats the previous one, so seeding from such a packet lands one
	// short and stamps a value that has already been used.
	if !ci.ccSeeded {
		if afc := (p[3] >> 4) & 0x03; afc != 0x01 && afc != 0x03 {
			_, err := ci.out.Write(p)
			return err
		}
		ci.videoCC = p[3] & 0x0F
		ci.ccSeeded = true
	}

	if pusi && ci.inPES {
		if err := ci.flush(); err != nil {
			return err
		}
	}
	if pusi {
		ci.inPES = true
		ci.pes = ci.pes[:0]
	}
	if !ci.inPES {
		ci.stampVideoCC(p)
		_, err := ci.out.Write(p)
		return err
	}

	var t tsPacket
	copy(t.buf[:], p)
	t.video = true
	if afc := (p[3] >> 4) & 0x03; afc == 0x01 || afc == 0x03 {
		t.payload = true
	}
	ci.window = append(ci.window, t)
	ci.pes = append(ci.pes, tsPayload(p)...)

	// A pathological stream with no further PUSI must not grow without bound.
	if len(ci.window) > 4096 {
		if !ci.warned {
			logger("[CC] %s access unit exceeded the reassembly limit, passing through", ci.log)
			ci.warned = true
		}
		return ci.passthroughWindow()
	}
	return nil
}

// tsPayload returns the payload bytes of a transport packet, skipping any
// adaptation field.
func tsPayload(p []byte) []byte {
	afc := (p[3] >> 4) & 0x03
	switch afc {
	case 0x01:
		return p[4:]
	case 0x03:
		l := int(p[4])
		if 5+l > tsPacketSize {
			return nil
		}
		return p[5+l:]
	}
	return nil
}

// mpegCRC is the CRC-32 the PSI tables carry, MSB first with no final inversion.
func mpegCRC(b []byte) uint32 {
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

// captionDescriptor is the ATSC caption service descriptor, announcing one
// line 21 field 1 service in English. A player that does not decode the video
// looking for caption messages finds out captions exist from this and nothing
// else, which is why some show none without it.
var captionDescriptor = []byte{
	0x86, 0x07, // tag, length
	0xE1,          // reserved, one service
	'e', 'n', 'g', // language
	0x7F, // analogue service on field 1
	0x7F, // not easy reader, wide aspect
	0xFF, // reserved
}

// addCaptionDescriptor rewrites a PMT so the video stream announces a caption
// service. It returns nil when the table cannot be rewritten safely, in which
// case the original is passed through untouched.
func addCaptionDescriptor(p []byte, videoPID int) []byte {
	if p[1]&0x40 == 0 {
		return nil // not the start of a section
	}
	pl := tsPayload(p)
	off := len(p) - len(pl) // where the payload begins in the packet
	sec := psiSection(pl)
	if len(sec) < 16 || sec[0] != 0x02 {
		return nil
	}
	slen := int(sec[1]&0x0F)<<8 | int(sec[2])
	end := 3 + slen
	if end > len(sec) || slen < 13 {
		return nil
	}
	body := sec[:end-4] // section without its CRC

	il := int(body[10]&0x0F)<<8 | int(body[11])
	i := 12 + il
	out := append([]byte(nil), body[:i]...)
	found := false
	for i+4 < len(body) {
		st := int(body[i])
		pid := int(body[i+1]&0x1F)<<8 | int(body[i+2])
		esil := int(body[i+3]&0x0F)<<8 | int(body[i+4])
		if i+5+esil > len(body) {
			return nil
		}
		entry := append([]byte(nil), body[i:i+5+esil]...)
		if pid == videoPID && (st == streamTypeH264 || st == streamTypeHEVC) {
			if bytes.Contains(entry[5:], []byte{0x86}) {
				return nil // the source already announces captions
			}
			entry = append(entry, captionDescriptor...)
			n := esil + len(captionDescriptor)
			entry[3] = byte(n>>8) | 0xF0
			entry[4] = byte(n)
			found = true
		}
		out = append(out, entry...)
		i += 5 + esil
	}
	if !found {
		return nil
	}
	// Restate the section length, then the CRC over everything before it.
	n := len(out) + 4 - 3
	if n > 0x3FD {
		return nil
	}
	out[1] = byte(n>>8) | 0xB0
	out[2] = byte(n)
	crc := mpegCRC(out)
	out = append(out, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))

	// Rebuild the packet: pointer field, section, then stuffing.
	if off+1+len(out) > tsPacketSize {
		return nil
	}
	var q [tsPacketSize]byte
	copy(q[:], p[:off])
	q[off] = 0x00 // pointer_field
	copy(q[off+1:], out)
	for j := off + 1 + len(out); j < tsPacketSize; j++ {
		q[j] = 0xFF
	}
	r := make([]byte, tsPacketSize)
	copy(r, q[:])
	return r
}

// psiSection skips the pointer_field at the head of a PSI payload.
//
// That field is a whole byte and can therefore claim up to 255, while the
// payload it indexes into is at most 184. A packet saying so is malformed, but
// malformed packets arrive: an encoder losing its HDMI signal emits rubbish,
// and the resync path can hand over a run of bytes that merely begins with the
// sync byte. Trusting it panicked, and a panic here takes the whole proxy down
// with every tuner on it, not just the captions.
func psiSection(pl []byte) []byte {
	if len(pl) < 1 {
		return nil
	}
	skip := 1 + int(pl[0])
	if skip > len(pl) {
		return nil
	}
	return pl[skip:]
}

func (ci *captionInjector) parsePAT(p []byte) {
	if p[1]&0x40 == 0 || ci.pmtPID >= 0 {
		return
	}
	pl := psiSection(tsPayload(p))
	if len(pl) < 12 || pl[0] != 0x00 {
		return
	}
	sl := int(pl[1]&0x0F)<<8 | int(pl[2])
	end := 3 + sl - 4
	if end > len(pl) {
		return
	}
	for i := 8; i+3 < end; i += 4 {
		prog := int(pl[i])<<8 | int(pl[i+1])
		pid := int(pl[i+2]&0x1F)<<8 | int(pl[i+3])
		if prog != 0 {
			ci.pmtPID = pid
			return
		}
	}
}

func (ci *captionInjector) parsePMT(p []byte) {
	if p[1]&0x40 == 0 || ci.videoPID >= 0 {
		return
	}
	pl := psiSection(tsPayload(p))
	if len(pl) < 16 || pl[0] != 0x02 {
		return
	}
	sl := int(pl[1]&0x0F)<<8 | int(pl[2])
	end := 3 + sl - 4
	if end > len(pl) {
		return
	}
	il := int(pl[10]&0x0F)<<8 | int(pl[11])
	i := 12 + il
	for i+4 < end {
		st := int(pl[i])
		pid := int(pl[i+1]&0x1F)<<8 | int(pl[i+2])
		esil := int(pl[i+3]&0x0F)<<8 | int(pl[i+4])
		switch st {
		case streamTypeH264:
			ci.videoPID, ci.hevc = pid, false
			logger("[CC] %s video is H.264 on PID %d", ci.log, pid)
		case streamTypeHEVC:
			ci.videoPID, ci.hevc = pid, true
			logger("[CC] %s video is HEVC on PID %d", ci.log, pid)
		}
		if ci.videoPID >= 0 {
			return
		}
		i += 5 + esil
	}
}

// stampVideoCC rewrites a video packet's continuity counter from our own
// sequence.
//
// Adding captions makes an access unit larger, so our packet count for the
// video PID drifts away from the source's. Once that has happened, a packet
// carrying the source's original counter reads as a lost packet to the
// demuxer, which throws away the picture around it. Every packet on the video
// PID therefore gets its counter from us, whether we rebuilt it or not.
func (ci *captionInjector) stampVideoCC(p []byte) {
	if afc := (p[3] >> 4) & 0x03; afc == 0x00 || afc == 0x02 {
		// A packet with no payload does not advance the count; it repeats the
		// one the last payload packet used. Giving it the next value instead
		// reads as a lost packet either side of it.
		p[3] = (p[3] &^ 0x0F) | ((ci.videoCC - 1) & 0x0F)
		return
	}
	p[3] = (p[3] &^ 0x0F) | (ci.videoCC & 0x0F)
	ci.videoCC = (ci.videoCC + 1) & 0x0F
}

// passthroughWindow writes the held packets out unmodified apart from the video
// continuity counter, then resets state.
func (ci *captionInjector) passthroughWindow() error {
	for i := range ci.window {
		if ci.window[i].video {
			ci.stampVideoCC(ci.window[i].buf[:])
		}
		if _, err := ci.out.Write(ci.window[i].buf[:]); err != nil {
			return err
		}
	}
	ci.window = ci.window[:0]
	ci.pes = ci.pes[:0]
	ci.inPES = false
	return nil
}

// flush rebuilds the completed access unit with captions attached and writes
// the window out in its original packet order.
func (ci *captionInjector) flush() error {
	if len(ci.pes) == 0 {
		return ci.passthroughWindow()
	}
	es, hdrLen, ptsVal, ok := splitPES(ci.pes)
	if !ok || len(es) == 0 {
		return ci.passthroughWindow()
	}
	if hasCaptionSEI(es, ci.hevc) {
		// The source already carries captions. Do not add a second set.
		return ci.passthroughWindow()
	}
	ci.trackFrameRate(ptsVal)

	sei := buildCaptionSEI(ci.enc.next(), ci.ccCount, ci.hevc)
	newES := injectSEI(es, sei, ci.hevc)
	if len(newES) == len(es) {
		// No slice NAL found; leave this access unit alone.
		return ci.passthroughWindow()
	}
	ci.frames++
	ci.injected++

	newPES := rebuildPES(ci.pes, hdrLen, newES)
	pkts := ci.packetize(newPES)
	return ci.emit(pkts)
}

// splitPES separates the PES header from the elementary stream and returns the
// PTS when one is present.
func splitPES(pes []byte) (es []byte, hdrLen int, pts int64, ok bool) {
	if len(pes) < 9 || pes[0] != 0x00 || pes[1] != 0x00 || pes[2] != 0x01 {
		return nil, 0, 0, false
	}
	optLen := int(pes[8])
	start := 9 + optLen
	if start > len(pes) {
		return nil, 0, 0, false
	}
	pts = -1
	if pes[7]&0x80 != 0 && optLen >= 5 {
		p := pes[9:]
		pts = int64(p[0]&0x0E)<<29 | int64(p[1])<<22 | int64(p[2]&0xFE)<<14 | int64(p[3])<<7 | int64(p[4])>>1
	}
	return pes[start:], start, pts, true
}

// rebuildPES reassembles a PES packet around a modified elementary stream.
func rebuildPES(orig []byte, hdrLen int, es []byte) []byte {
	unbounded := orig[4] == 0 && orig[5] == 0
	out := make([]byte, 0, hdrLen+len(es))
	out = append(out, orig[:hdrLen]...)
	out = append(out, es...)
	n := len(out) - 6
	if unbounded || n > 0xFFFF {
		// Zero means "runs to the next start", which is legal for video and is
		// what an encoder emits for a picture larger than 64 KB. Keep whichever
		// form the source chose.
		out[4], out[5] = 0, 0
	} else {
		out[4], out[5] = byte(n>>8), byte(n)
	}
	return out
}

// trackFrameRate derives the caption construct count from the picture rate, so
// the caption channel runs at the 9600 bits per second the spec calls for.
func (ci *captionInjector) trackFrameRate(pts int64) {
	if pts < 0 {
		return
	}
	if ci.lastPTS > 0 && !ci.haveRate {
		if d := pts - ci.lastPTS; d > 0 && d < 90000 {
			fps := 90000.0 / float64(d)
			n := int(600.0/fps + 0.5)
			if n >= 2 && n <= 31 {
				ci.ccCount = n
				ci.haveRate = true
				logger("[CC] %s picture rate is %.2f fps, cc_count %d", ci.log, fps, n)
			}
		}
	}
	ci.lastPTS = pts
}

// packetize turns a PES packet back into transport packets on the video PID.
// Continuity counters are left blank and assigned by emit, which is the only
// place that knows the order packets actually leave in: the clock-bearing
// packets kept from the source are interleaved with these.
//
// carrying the original adaptation field of the first packet so the PCR and any
// random access indicator survive.
func (ci *captionInjector) packetize(pes []byte) [][tsPacketSize]byte {
	// The clock can sit on any packet of the access unit, not only the first.
	// Collect what each source packet carried, in order, and give the same to
	// the rebuilt packet in that position; anything beyond gets none. The PCR
	// shifts by at most a packet, which is a fraction of a millisecond, where
	// dropping it costs the receiver its clock.
	var afs [][]byte
	for i := range ci.window {
		if ci.window[i].video && ci.window[i].payload {
			afs = append(afs, meaningfulAF(ci.window[i].buf[:]))
		}
	}

	var out [][tsPacketSize]byte
	first := true
	for len(pes) > 0 {
		var af []byte
		if k := len(out); k < len(afs) {
			af = afs[k]
		}
		var pkt [tsPacketSize]byte
		pkt[0] = 0x47
		pkt[1] = byte(ci.videoPID >> 8)
		pkt[2] = byte(ci.videoPID)
		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
		}

		body := 4
		useAF := len(af) > 0
		if useAF {
			// adaptation_field_control = 11
			pkt[3] = 0x30
			pkt[4] = byte(len(af))
			copy(pkt[5:], af)
			body = 5 + len(af)
		} else {
			pkt[3] = 0x10
		}
		space := tsPacketSize - body
		if len(pes) < space {
			// Pad the tail with an adaptation field so the packet stays 188
			// bytes without inventing elementary stream data.
			stuff := space - len(pes)
			if useAF {
				// Extend the existing adaptation field.
				pkt[4] = byte(int(pkt[4]) + stuff)
				copy(pkt[body+stuff:], pes)
				for i := body; i < body+stuff; i++ {
					pkt[i] = 0xFF
				}
			} else {
				pkt[3] = 0x30
				pkt[4] = byte(stuff - 1)
				if stuff >= 2 {
					pkt[5] = 0x00
					for i := 6; i < 4+stuff; i++ {
						pkt[i] = 0xFF
					}
				}
				copy(pkt[4+stuff:], pes)
			}
			out = append(out, pkt)
			break
		}
		copy(pkt[body:], pes[:space])
		pes = pes[space:]
		out = append(out, pkt)
		first = false
	}
	return out
}

// meaningfulAF returns a packet's adaptation field only when it carries
// something the receiver needs: the programme clock, or a flag marking a
// discontinuity, a random access point or a splice. An adaptation field that is
// nothing but stuffing is dropped, since the repacketizer adds its own where it
// needs to pad.
func meaningfulAF(p []byte) []byte {
	af := adaptationField(p)
	if len(af) == 0 {
		return nil
	}
	// discontinuity, random access, ES priority, PCR, OPCR, splicing point.
	if af[0]&0xFC == 0 {
		return nil
	}
	return af
}

// adaptationField returns the meaningful part of a packet's adaptation field so
// the PCR and the random access indicator survive repacketization.
//
// The length is computed from the flags rather than by trimming trailing 0xFF,
// because a PCR or OPCR can legitimately end in 0xFF and trimming those bytes
// leaves a field that claims more than it carries, which shifts the payload and
// corrupts the picture.
func adaptationField(p []byte) []byte {
	if (p[3]>>4)&0x02 == 0 {
		return nil
	}
	l := int(p[4])
	if l == 0 || 5+l > tsPacketSize {
		return nil
	}
	af := p[5 : 5+l]
	flags := af[0]
	n := 1
	if flags&0x10 != 0 { // PCR
		n += 6
	}
	if flags&0x08 != 0 { // OPCR
		n += 6
	}
	if flags&0x04 != 0 { // splicing point
		n++
	}
	if flags&0x02 != 0 && n < len(af) { // transport private data
		n += 1 + int(af[n])
	}
	if flags&0x01 != 0 && n < len(af) { // adaptation field extension
		n += 1 + int(af[n])
	}
	if n > len(af) {
		return af
	}
	return af[:n]
}

// emit writes the rebuilt video packets back into the window's original slots
// so the interleaving with audio and PSI stays close to the source.
func (ci *captionInjector) emit(pkts [][tsPacketSize]byte) error {
	n := 0
	for i := range ci.window {
		if ci.window[i].video {
			// A video packet with no payload carries the programme clock, not
			// the picture. Overwriting it with rebuilt payload, or skipping it
			// once the rebuilt packets run out, deletes a PCR the receiver
			// needs; on a constant rate mux that is most of them.
			if !ci.window[i].payload {
				ci.stampVideoCC(ci.window[i].buf[:])
				if _, err := ci.out.Write(ci.window[i].buf[:]); err != nil {
					return err
				}
				continue
			}
			if n < len(pkts) {
				ci.stampVideoCC(pkts[n][:])
				if _, err := ci.out.Write(pkts[n][:]); err != nil {
					return err
				}
				n++
			}
			continue
		}
		if _, err := ci.out.Write(ci.window[i].buf[:]); err != nil {
			return err
		}
	}
	// Captions make the access unit slightly larger, so anything left over goes
	// out right after the window it belongs to.
	for ; n < len(pkts); n++ {
		ci.stampVideoCC(pkts[n][:])
		if _, err := ci.out.Write(pkts[n][:]); err != nil {
			return err
		}
	}
	ci.window = ci.window[:0]
	ci.pes = ci.pes[:0]
	ci.inPES = false
	return nil
}

// ---------------------------------------------------------------------------
// Speech recognition
// ---------------------------------------------------------------------------

// Recognition runs in this process against parakeet.cpp, a ggml implementation
// of NVIDIA's Parakeet models. Its flat C entry points are opened with purego,
// so there is no cgo: ah4c still builds as pure Go, and the engine is a file the
// user downloaded rather than anything linked into the binary or baked into the
// image.

const (
	asrSampleRate = 16000
	// decoder 0 lets the library pick by architecture: the transducer head for
	// TDT and RNNT models, CTC for CTC models.
	asrDecoderDefault = 0
)

var (
	pkOnce sync.Once
	pkErr  error

	pkABIVersion func() int32
	pkLoad       func(path string) uintptr
	pkFreeCtx    func(ctx uintptr)
	pkTranscribe func(ctx uintptr, samples unsafe.Pointer, n int32, rate int32, decoder int32, lang string) unsafe.Pointer
	pkFreeString func(s unsafe.Pointer)
	pkLastError  func(ctx uintptr) string

	// Cache-aware streaming: the session buffers audio and returns text as
	// encoder chunks complete, instead of waiting for a whole phrase.
	pkStreamBegin    func(ctx uintptr, lang string) uintptr
	pkStreamFeed     func(s uintptr, pcm unsafe.Pointer, n int32) unsafe.Pointer
	pkStreamFinalize func(s uintptr) unsafe.Pointer
	pkStreamFree     func(s uintptr)
)

// Event bits reported by a streaming feed.
const (
	pkEventEOU = 1 // the speaker finished an utterance
	pkEventEOB = 2 // a backchannel, a short "uh-huh" while someone else talks
)

// initParakeet opens the downloaded engine exactly once per process.
func initParakeet() error {
	pkOnce.Do(func() {
		lib := engineLibPath()
		if lib == "" {
			pkErr = fmt.Errorf("no speech engine is published for %s/%s", runtime.GOOS, runtime.GOARCH)
			return
		}
		if !engineInstalled() {
			pkErr = fmt.Errorf("the speech engine has not been downloaded yet")
			return
		}
		abs, err := filepath.Abs(lib)
		if err != nil {
			pkErr = err
			return
		}
		// Opened privately. libparakeet.so embeds its own ggml and exports eight
		// hundred ggml_* symbols; transcribe.cpp imports the same names for its
		// own build. Published process-wide, this one would capture the other's
		// compute layer, which is a class of bug nobody should have to debug.
		// It loses nothing by being private: it is a single self-contained
		// library with no modules to load afterwards, which is precisely what
		// transcribe.cpp is not.
		handle, err := purego.Dlopen(abs, purego.RTLD_NOW)
		if err != nil {
			pkErr = fmt.Errorf("opening %s: %w", abs, err)
			return
		}
		defer func() {
			// A missing symbol panics inside purego; report it as an error
			// rather than taking the process down mid-tune.
			if r := recover(); r != nil {
				pkErr = fmt.Errorf("speech engine is missing an entry point: %v", r)
			}
		}()
		purego.RegisterLibFunc(&pkABIVersion, handle, "parakeet_capi_abi_version")
		purego.RegisterLibFunc(&pkLoad, handle, "parakeet_capi_load")
		purego.RegisterLibFunc(&pkFreeCtx, handle, "parakeet_capi_free")
		purego.RegisterLibFunc(&pkTranscribe, handle, "parakeet_capi_transcribe_pcm_lang")
		purego.RegisterLibFunc(&pkFreeString, handle, "parakeet_capi_free_string")
		purego.RegisterLibFunc(&pkLastError, handle, "parakeet_capi_last_error")
		purego.RegisterLibFunc(&pkStreamBegin, handle, "parakeet_capi_stream_begin_lang")
		purego.RegisterLibFunc(&pkStreamFeed, handle, "parakeet_capi_stream_feed_json")
		purego.RegisterLibFunc(&pkStreamFinalize, handle, "parakeet_capi_stream_finalize_json")
		purego.RegisterLibFunc(&pkStreamFree, handle, "parakeet_capi_stream_free")
		logger("[CC] Speech engine loaded, ABI %d", pkABIVersion())
	})
	return pkErr
}

// recognizer is a loaded speech model. There are two implementations, one per
// engine, and nothing downstream of this knows which it has: the phrase
// segmenter, the CEA-608 encoder and the injector are the same either way.
//
// beginStream returns an error on a model that cannot transcribe continuously,
// which is the caller's cue to fall back to a phrase at a time.
type recognizer interface {
	transcribe(pcm []float32) (string, error)
	beginStream(language string) error
	feedStream(pcm []float32) *streamResult
	finishStream() *streamResult
	// idleFlush releases the tail of a sentence after the talking stops.
	//
	// A streaming engine holds the last few words back until more audio arrives
	// to confirm them, which is right in the middle of speech and wrong at the
	// end of it: the closing words of a sentence would sit unsaid until the next
	// person started talking, so a pause left the caption hanging mid-sentence
	// for as long as the room was quiet. A model that reports the end of an
	// utterance itself has nothing to do here.
	idleFlush() *streamResult
	Close()
}

// parakeet is a loaded model, reused across utterances.
type parakeet struct {
	ctx  uintptr
	lang string
	// stream is the continuous session, opened by beginStream and non-zero
	// only for a model that supports one.
	stream uintptr
	// eou is the out-parameter the streaming feed writes its event mask into.
	// It lives on the heap for the life of the model: handing C a pointer into
	// a goroutine stack is not safe, because the stack can move.
	eou []int32
	mu  sync.Mutex // the context holds decoder state, so one utterance at a time
}

// loadParakeet opens the weights the user downloaded.
func loadParakeet(gguf, language string) (*parakeet, error) {
	if err := initParakeet(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(gguf)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	ctx := pkLoad(abs)
	if ctx == 0 {
		return nil, fmt.Errorf("could not load %s", filepath.Base(gguf))
	}
	if language == "" {
		language = "auto"
	}
	return &parakeet{ctx: ctx, lang: language, eou: make([]int32, 1)}, nil
}

func (p *parakeet) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stream != 0 {
		pkStreamFree(p.stream)
		p.stream = 0
	}
	if p.ctx != 0 {
		pkFreeCtx(p.ctx)
		p.ctx = 0
	}
}

// transcribe runs one utterance of 16 kHz mono audio through the model.
func (p *parakeet) transcribe(pcm []float32) (string, error) {
	if len(pcm) < asrSampleRate/4 {
		return "", nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == 0 {
		return "", fmt.Errorf("model is closed")
	}

	out := pkTranscribe(p.ctx, unsafe.Pointer(&pcm[0]), int32(len(pcm)), asrSampleRate, asrDecoderDefault, p.lang)
	// The engine reads the samples during the call, so keep them reachable for
	// its duration rather than trusting the argument alone to pin them.
	runtime.KeepAlive(pcm)
	if out == nil {
		if msg := pkLastError(p.ctx); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("recognition failed")
	}
	return cleanRecognized(cStringFree(out)), nil
}

// beginStream opens a cache-aware streaming session. Only a streaming
// checkpoint supports one; anything else returns an error and the caller falls
// back to recognizing a phrase at a time.
func (p *parakeet) beginStream(language string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == 0 {
		return fmt.Errorf("model is closed")
	}
	s := pkStreamBegin(p.ctx, language)
	if s == 0 && language != "auto" && language != "" {
		// A prompt-conditioned model rejects a locale it does not know rather
		// than falling back, so try again letting it detect.
		if msg := pkLastError(p.ctx); msg != "" {
			logger("[CC] streaming rejected language %q (%s), letting the model detect", language, msg)
		}
		s = pkStreamBegin(p.ctx, "auto")
	}
	if s == 0 {
		if msg := pkLastError(p.ctx); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("this model does not support streaming")
	}
	if p.stream != 0 {
		// A session can be opened more than once now that the decoder is
		// restarted after a failure, and each one owns an encoder cache. Losing
		// the handle would leak that cache on every restart of a long capture.
		pkStreamFree(p.stream)
	}
	p.stream = s
	return nil
}

// streamResult is what a streaming feed reports.
//
// The plain text entry point hands back whatever tokens finalized in that call,
// which is sub-word: "broadcast" arrives as "broad" then "cast" with nothing to
// say whether the two join or are separate words. The JSON entry point groups
// them properly and timestamps each word, which is both what the display needs
// and what makes a decent subtitle cue.
type streamResult struct {
	Text  string       `json:"text"`
	EOU   int          `json:"eou"`
	Words []streamWord `json:"words"`
}

// streamWord is one finalized word and where it fell in the audio. It is named
// rather than anonymous because both engines build these: parakeet.cpp hands
// them over as JSON, transcribe.cpp through an accessor, and the caption side
// should not be able to tell which it is looking at.
type streamWord struct {
	W     string  `json:"w"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// words joins the grouped words of this result.
//
// Only the grouped words are used. The text field carries the same speech as
// the raw sub-word tokens it finalized, so taking both would print every word
// twice: once in pieces and once whole.
func (r *streamResult) words() string {
	if len(r.Words) == 0 {
		return ""
	}
	_ = r.Text
	parts := make([]string, 0, len(r.Words))
	for _, w := range r.Words {
		if t := cleanRecognized(w.W); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// feedStream hands the session more audio and returns whatever it just
// finalized.
func (p *parakeet) feedStream(pcm []float32) *streamResult {
	if p.stream == 0 || len(pcm) == 0 {
		return nil
	}
	out := pkStreamFeed(p.stream, unsafe.Pointer(&pcm[0]), int32(len(pcm)))
	runtime.KeepAlive(pcm)
	return parseStreamResult(cStringFree(out))
}

// idleFlush does nothing here. These models mark the end of an utterance
// themselves and the feed carries that flag out, so a pause already closes the
// sentence without being asked.
func (p *parakeet) idleFlush() *streamResult { return nil }

// finishStream flushes the tail when the stream ends.
func (p *parakeet) finishStream() *streamResult {
	if p.stream == 0 {
		return nil
	}
	return parseStreamResult(cStringFree(pkStreamFinalize(p.stream)))
}

func parseStreamResult(doc string) *streamResult {
	if doc == "" {
		return nil
	}
	var r streamResult
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return nil
	}
	return &r
}

// specialToken matches the control tokens a model emits alongside the words:
// the language tag a prompt-conditioned model stamps on each utterance, and the
// end-of-utterance and backchannel markers. They belong to the model, not to
// the caption, and would otherwise be read out on screen as "<en-US>".
var specialToken = regexp.MustCompile(`<[^<>]*>`)

// cleanRecognized removes those tokens and tidies the spacing they leave.
func cleanRecognized(text string) string {
	if !strings.ContainsRune(text, '<') {
		return strings.TrimSpace(text)
	}
	text = specialToken.ReplaceAllString(text, " ")
	return strings.TrimSpace(strings.Join(strings.Fields(text), " "))
}

// cStringFree copies a NUL-terminated string out of engine memory and releases
// the original, which the C API hands over to the caller.
func cStringFree(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	defer pkFreeString(p)
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	if n == 0 {
		return ""
	}
	return strings.TrimSpace(string(unsafe.Slice((*byte)(p), n)))
}

// ---------------------------------------------------------------------------
// Speech recognition: transcribe.cpp
// ---------------------------------------------------------------------------

// The second engine, bound the same way as the first: purego, no cgo, nothing
// linked in. What it adds is reach. parakeet.cpp implements NVIDIA's Parakeet
// architectures and only those; transcribe.cpp implements a couple of dozen
// families, which is what lets this catalog reach past the Parakeet ones.
//
// Three things differ from parakeet.cpp and all three are handled below.
// It is a library plus sibling ggml backends rather than a single file, so it
// is told where they are. Work happens on a session rather than a bare context.
// And results are read back through accessors instead of a returned string, so
// nothing here owns engine memory or frees it.

const (
	// Backends, from transcribe.h. The processor is asked for by name rather
	// than left to AUTO so that choosing it on the page actually means it.
	txBackendAuto   = 0
	txBackendCPU    = 1
	txBackendVulkan = 3
	txBackendCUDA   = 5
	txBackendMetal  = 2

	txOK = 0

	// No alignment data is wanted; see runParams.
	txTimestampsNone = 0

	// Struct ids for the ABI check, from transcribe_abi_struct.
	txABILoadParams    = 0
	txABISessionParams = 1
	txABIRunParams     = 2
	txABIStreamParams  = 3
	txABIStreamUpdate  = 9
	txABIStreamText    = 10
)

// The parameter and result structs, laid out to match transcribe.h. Every one
// of them is checked against the library's own sizeof at load rather than
// trusted: this is a pre-1.0 ABI that says outright it may move between minor
// releases, and a silently mismatched struct is memory corruption rather than
// an error message.
type (
	txLoadParams struct {
		structSize uint64
		backend    int32
		gpuDevice  int32
	}
	txSessionParams struct {
		structSize uint64
		nThreads   int32
		kvType     int32
		nCtx       int32
	}
	txRunParams struct {
		structSize      uint64
		task            int32
		timestamps      int32
		pnc             int32
		itn             int32
		language        *byte
		targetLanguage  *byte
		keepSpecialTags bool
		family          uintptr
		specKDrafts     int32
	}
	txStreamParams struct {
		structSize             uint64
		family                 uintptr
		commitPolicy           int32
		stablePrefixAgreementN uint32
	}
	txStreamUpdate struct {
		structSize       uint64
		resultChanged    bool
		isFinal          bool
		revision         int32
		inputReceivedMS  int64
		audioCommittedMS int64
		bufferedMS       int64
		committedChanged bool
		tentativeChanged bool
	}
	// The engine reports byte lengths alongside every view of the transcript,
	// which is what is used below: a length is exact where scanning for a
	// terminator is a guess about encoding.
	txStreamText struct {
		structSize             uint64
		fullText               *byte
		fullTextBytes          uint64
		committedText          *byte
		committedTextBytes     uint64
		tentativeText          *byte
		tentativeTextBytes     uint64
		rawTentativeStartBytes uint64
	}
)

var (
	txOnce sync.Once
	txErr  error

	txVersion       func() string
	txStatusString  func(status int32) string
	txInitBackends  func(dir string) int32
	txABIStructSize func(which int32) uint64

	txModelLoadFile func(path string, params, out unsafe.Pointer) int32
	txModelFree     func(model uintptr)
	txSessionInit   func(model uintptr, params, out unsafe.Pointer) int32
	txSessionFree   func(session uintptr)

	txRun      func(session uintptr, pcm unsafe.Pointer, n int32, params unsafe.Pointer) int32
	txFullText func(session uintptr) string

	txStreamBegin    func(session uintptr, runParams, streamParams unsafe.Pointer) int32
	txStreamFeed     func(session uintptr, pcm unsafe.Pointer, n int32, update unsafe.Pointer) int32
	txStreamFinalize func(session uintptr, update unsafe.Pointer) int32
	txStreamGetText  func(session uintptr, out unsafe.Pointer) int32

	txSetAbortCallback   func(session uintptr, cb uintptr, userData unsafe.Pointer)
	txAcceptsExtKind     func(model uintptr, slot int32, kind uint32) bool
	txBackendAvailable   func(kind int32) bool
	txModelBackend       func(model uintptr) int32
	txRunBatch           func(session uintptr, pcm, nSamples unsafe.Pointer, n int32, params unsafe.Pointer) int32
	txBatchNResults      func(session uintptr) int32
	txBatchStatus        func(session uintptr, i int32) int32
	txBatchFullText      func(session uintptr, i int32) string
	txPkStreamExtInit    func(p unsafe.Pointer)
	txPkBufStreamExtInit func(p unsafe.Pointer)
	txLogSet             func(cb uintptr, userData unsafe.Pointer)
	txWasAborted         func(session uintptr) bool
	txLoadParamsInit     func(p unsafe.Pointer)
	txSessionParamsInit  func(p unsafe.Pointer)
	txRunParamsInit      func(p unsafe.Pointer)
	txStreamParamsInit   func(p unsafe.Pointer)
	txStreamUpdateInit   func(p unsafe.Pointer)
	txStreamTextInit     func(p unsafe.Pointer)
)

// initTranscribe opens the engine once per process and registers its backends.
//
// dir is the directory the archive was unpacked into. transcribe.cpp keeps its
// ggml backends in separate libraries next to itself and is handed that path,
// which is also how a machine with no GPU ends up on the processor without
// anything failing to open: the Vulkan backend is simply one of the libraries
// that does not load, and the engine carries on with the ones that did.
func initTranscribe(variant string) error {
	txOnce.Do(func() {
		lib := runtimeLibPath(rtTranscribe, variant)
		if lib == "" {
			txErr = fmt.Errorf("no transcribe.cpp build is published for %s/%s", runtime.GOOS, runtime.GOARCH)
			return
		}
		if !runtimeInstalled(rtTranscribe, variant) {
			txErr = fmt.Errorf("transcribe.cpp has not been downloaded yet")
			return
		}
		// The backends live beside the library, so both come from the same
		// build. Reading one from the configured variant and the other from the
		// variant actually in use is how a GPU upgrade ends up pointing at a
		// directory that holds a different build.
		dir, err := filepath.Abs(runtimeDirFor(rtTranscribe, variant))
		if err != nil {
			txErr = err
			return
		}
		abs, err := filepath.Abs(lib)
		if err != nil {
			txErr = err
			return
		}
		// Global, unlike parakeet.cpp, and the asymmetry is deliberate.
		//
		// This engine's compute backends are separate modules that it dlopens
		// itself after this call, and they resolve their ggml symbols at that
		// point. Loading the engine privately is exactly the condition under
		// which a module can fail to find them and be skipped — which is what a
		// missing Vulkan backend looks like from outside, and is
		// indistinguishable from a missing driver. Publishing these symbols is
		// safe now that the other engine no longer publishes its own: the
		// collision that made both private was parakeet.cpp capturing this
		// one's ggml, and keeping parakeet.cpp private prevents it just as well
		// while leaving this engine's own modules able to link against it.
		handle, err := purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil || handle == 0 {
			txErr = fmt.Errorf("opening %s: %w", abs, err)
			return
		}
		defer func() {
			// A missing symbol panics inside purego; report it as an error
			// rather than taking the process down mid-tune.
			if r := recover(); r != nil {
				txErr = fmt.Errorf("transcribe.cpp is missing an entry point: %v", r)
			}
		}()
		purego.RegisterLibFunc(&txVersion, handle, "transcribe_version")
		purego.RegisterLibFunc(&txStatusString, handle, "transcribe_status_string")
		purego.RegisterLibFunc(&txInitBackends, handle, "transcribe_init_backends")
		purego.RegisterLibFunc(&txABIStructSize, handle, "transcribe_abi_struct_size")
		purego.RegisterLibFunc(&txModelLoadFile, handle, "transcribe_model_load_file")
		purego.RegisterLibFunc(&txModelFree, handle, "transcribe_model_free")
		purego.RegisterLibFunc(&txSessionInit, handle, "transcribe_session_init")
		purego.RegisterLibFunc(&txSessionFree, handle, "transcribe_session_free")
		purego.RegisterLibFunc(&txRun, handle, "transcribe_run")
		purego.RegisterLibFunc(&txFullText, handle, "transcribe_full_text")
		purego.RegisterLibFunc(&txSetAbortCallback, handle, "transcribe_set_abort_callback")
		purego.RegisterLibFunc(&txLogSet, handle, "transcribe_log_set")
		purego.RegisterLibFunc(&txAcceptsExtKind, handle, "transcribe_model_accepts_ext_kind")
		purego.RegisterLibFunc(&txBackendAvailable, handle, "transcribe_backend_available")
		purego.RegisterLibFunc(&txModelBackend, handle, "transcribe_model_backend")
		purego.RegisterLibFunc(&txRunBatch, handle, "transcribe_run_batch")
		purego.RegisterLibFunc(&txBatchNResults, handle, "transcribe_batch_n_results")
		purego.RegisterLibFunc(&txBatchStatus, handle, "transcribe_batch_status")
		purego.RegisterLibFunc(&txBatchFullText, handle, "transcribe_batch_full_text")
		purego.RegisterLibFunc(&txPkStreamExtInit, handle, "transcribe_parakeet_stream_ext_init")
		purego.RegisterLibFunc(&txPkBufStreamExtInit, handle, "transcribe_parakeet_buffered_stream_ext_init")
		purego.RegisterLibFunc(&txWasAborted, handle, "transcribe_was_aborted")
		purego.RegisterLibFunc(&txStreamBegin, handle, "transcribe_stream_begin")
		purego.RegisterLibFunc(&txStreamFeed, handle, "transcribe_stream_feed")
		purego.RegisterLibFunc(&txStreamFinalize, handle, "transcribe_stream_finalize")
		purego.RegisterLibFunc(&txStreamGetText, handle, "transcribe_stream_get_text")
		purego.RegisterLibFunc(&txLoadParamsInit, handle, "transcribe_model_load_params_init")
		purego.RegisterLibFunc(&txSessionParamsInit, handle, "transcribe_session_params_init")
		purego.RegisterLibFunc(&txRunParamsInit, handle, "transcribe_run_params_init")
		purego.RegisterLibFunc(&txStreamParamsInit, handle, "transcribe_stream_params_init")
		purego.RegisterLibFunc(&txStreamUpdateInit, handle, "transcribe_stream_update_init")
		purego.RegisterLibFunc(&txStreamTextInit, handle, "transcribe_stream_text_init")

		if err := txCheckABI(); err != nil {
			txErr = err
			return
		}
		// Take the engine's logging before the backend scan, or its running
		// commentary goes to stderr and buries everything else. It writes a
		// line about its key/value cache for every phrase it transcribes, which
		// on a busy channel is a line a second, for ever.
		txLogSet(txLogCallback(), nil)
		if st := txInitBackends(dir); st != txOK {
			txErr = fmt.Errorf("registering the transcribe.cpp backends: %s", txStatusString(st))
			return
		}
		// Say which backends actually registered.
		//
		// A backend module whose system dependencies are missing — the Vulkan
		// one on a machine with a loader but no working driver, say — fails to
		// load quietly and is skipped by design, leaving the processor to pick
		// up the work. Quietly is the problem: somebody who has selected the
		// Vulkan build, passed the device through and installed the driver has
		// every reason to think it is being used, and nothing anywhere said
		// otherwise. It is one line at startup and it settles the question.
		var have []string
		for _, b := range []struct {
			name string
			kind int32
		}{{"processor", txBackendCPU}, {"Vulkan", txBackendVulkan}, {"CUDA", txBackendCUDA}, {"Metal", txBackendMetal}} {
			if txBackendAvailable(b.kind) {
				have = append(have, b.name)
			}
		}
		if len(have) == 0 {
			have = []string{"none"}
		}
		logger("[CC] transcribe.cpp backends available here: %s", strings.Join(have, ", "))
		logger("[CC] transcribe.cpp %s loaded from %s", txVersion(), dir)
	})
	return txErr
}

// txCheckABI compares every struct this file declares against the library's own
// idea of its size, and refuses to go further if any of them disagree.
func txCheckABI() error {
	for _, s := range []struct {
		name string
		id   int32
		size uintptr
	}{
		{"model load params", txABILoadParams, unsafe.Sizeof(txLoadParams{})},
		{"session params", txABISessionParams, unsafe.Sizeof(txSessionParams{})},
		{"run params", txABIRunParams, unsafe.Sizeof(txRunParams{})},
		{"stream params", txABIStreamParams, unsafe.Sizeof(txStreamParams{})},
		{"stream update", txABIStreamUpdate, unsafe.Sizeof(txStreamUpdate{})},
		{"stream text", txABIStreamText, unsafe.Sizeof(txStreamText{})},
	} {
		if got := txABIStructSize(s.id); got != uint64(s.size) {
			return fmt.Errorf("transcribe.cpp %s is %d bytes here and %d bytes in the engine; this build of ah4c expects transcribe.cpp %s",
				s.name, s.size, got, transcribeRelease)
		}
	}
	return nil
}

func txBackendName(k int32) string {
	switch k {
	case txBackendCPU:
		return "the processor"
	case txBackendVulkan:
		return "Vulkan"
	case txBackendCUDA:
		return "CUDA"
	case txBackendMetal:
		return "Metal"
	case txBackendAuto:
		return "whatever it chose"
	}
	return fmt.Sprintf("backend %d", k)
}

// txBackend maps the processor choice on the page onto the backend the engine
// is asked for.
// captionVariantFor is the build a model will actually run on, and the single
// place that decides it.
//
// A model that cannot work on a processor must not be given the processor
// because that is what the engine picker happens to say. The picker defaults to
// CPU and most people never touch it, so a GPU-only model otherwise passed the
// gate — which only asks whether a GPU exists — and then loaded strictly on the
// processor, where it falls behind live audio and drops most of what is said.
//
// Refusing is the right answer when there is no GPU build to run it on. Having
// a graphics card is not the same as having downloaded the build that uses it:
// the CUDA build is a separate download, so a machine with an NVIDIA card and
// no CUDA build would otherwise pass the gate and then quietly run on the
// processor anyway, which is the exact failure the gate exists to prevent.
func captionVariantFor(m captionModel) (string, error) {
	variant := currentEngineVariant()
	if !m.NeedsGPU {
		return variant, nil
	}
	rt := runtimeOf(m)
	if variant != "cpu" && runtimeInstalled(rt, variant) {
		return variant, nil
	}
	if g := gpuVariant(rt); g != "" {
		logger("[CC] %s needs a GPU, so it runs on the %s build rather than the processor", m.Name, g)
		return g, nil
	}
	return "", fmt.Errorf("%s needs a GPU build of %s and none is downloaded; fetch one from the Closed Captions page",
		m.Name, findSpeechRuntime(rt).Name)
}

// gpuVariant is the best GPU build this container can actually load, or "" if
// there is none.
func gpuVariant(rt string) string {
	nodes := renderNodes()
	for _, v := range engineVariants {
		// "auto" is the question, not an answer: resolving it by asking about
		// itself is how this recursed until the stack ran out.
		if v.Key == "auto" || v.Key == "cpu" || !engineUsable(v) {
			continue
		}
		if v.Key == "vulkan" && len(nodes) == 0 {
			continue
		}
		// Only a build that is actually on disk. Upgrading to one that was
		// never downloaded points the engine at a directory holding a different
		// build's backends, and the failure that produces is cached for the
		// life of the process: the backend scan cannot be retried, so captions
		// would then fail on every tune until a restart.
		if rt != "" && !runtimeInstalled(rt, v.Key) {
			continue
		}
		return v.Key
	}
	return ""
}

func txBackend(variant string) int32 {
	switch variant {
	case "vulkan":
		return txBackendVulkan
	case "cuda", "cuda12":
		return txBackendCUDA
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// The only Apple silicon build is the Metal one, and there is no
		// separate choice on that platform, so let the engine pick.
		return txBackendAuto
	}
	return txBackendCPU
}

// A recognition that never returns used to take the captions with it: the
// recognizer goroutine is the only thing calling the model, so one call that
// does not come back means no captions for the rest of the tune, with the
// picture running on perfectly and nothing on screen changing. The engine
// offers a way out — a callback it polls during a run, which aborts it when it
// returns true — so every run is given a deadline.
//
// The deadline is generous. It is not there to hurry a slow machine along; it
// is there so that "slow" can never become "stopped".
const txRunDeadline = 12 * time.Second

var (
	txAbortOnce sync.Once
	txAbortPtr  uintptr
)

// txAbortHandle is what the callback is handed. It holds no Go pointers, so its
// address can be given to C and read back from the run thread safely.
type txAbortHandle struct{ deadlineUnixNano int64 }

// txAbortCallback returns the C function pointer the engine polls, creating it
// once: purego callbacks come from a small fixed pool and must not be made per
// session.
func txAbortCallback() uintptr {
	txAbortOnce.Do(func() {
		txAbortPtr = purego.NewCallback(func(userData unsafe.Pointer) bool {
			if userData == nil {
				return false
			}
			d := atomic.LoadInt64((*int64)(userData))
			return d != 0 && time.Now().UnixNano() > d
		})
	})
	return txAbortPtr
}

// Loaded weights are per-stream and are freed the moment the stream ends.
//
// Two rules shape this. The engine permits one decode in flight per loaded copy
// across every session built on it, so concurrent streams cannot share one copy
// without taking turns — and a stream that waits its turn falls behind live
// audio and drops speech. So each concurrent stream gets its own copy.
//
// And nothing is kept once it is not being used. These are gigabytes: holding
// them against a stream that might start later means a machine that captioned
// three tuners an hour ago is still carrying three copies now, on a box that
// also has to run everything else. Memory is borrowed for as long as a stream
// needs it and given straight back.
//
// The cost of that is the load time on the next tune, which is why nothing on
// the tune path waits for it: the picture starts immediately and captions join
// when the weights are ready.
type sharedTxModel struct {
	handle uintptr
	refs   int
	// compute guards this copy. Nothing shares one today, so it is never
	// contended; it is here so that the engine's one-decode-at-a-time rule
	// cannot be broken by accident if anything ever does.
	compute sync.Mutex
}

var (
	txModelLock sync.Mutex
	txLive      = map[string]int{}
)

// gpuGate limits how many streams decode on the accelerator at once.
//
// A machine has one graphics card and may have seven tuners. Seven decodes
// issued at it together do not run seven times faster than two — the card runs
// them one at a time regardless, and the interleaving costs command buffer
// churn and working memory on top. Letting a couple through at a time keeps the
// card busy without the pile-up; the rest wait briefly, and if they wait too
// long the phrase queue drops one, which is the same back-pressure the rest of
// this file already uses.
//
// The processor path is not gated: threads are shared out there instead, which
// is the right tool for a device that really does run things in parallel.
var gpuGate = make(chan struct{}, 2)

func (t *transcribeModel) enterGPU() bool {
	if !t.onGPU {
		return false
	}
	gpuGate <- struct{}{}
	return true
}

func (t *transcribeModel) leaveGPU(held bool) {
	if held {
		<-gpuGate
	}
}

// Phrase-at-a-time models are served from one shared copy of the weights.
//
// The engine's rule is one compute in flight per model, and its batch entry
// point is what makes that rule cheap to obey: transcribe_run_batch takes any
// number of clips of any lengths in a single call, pads and masks them
// internally, returns a result per clip, and counts as one compute. So instead
// of a copy of the weights per tuner taking turns — or worse, a copy each —
// every tuner hands its phrase to one service goroutine, which runs whatever
// has accumulated as a single batch and hands the texts back. Five tuners on a
// 2 GB model cost 2 GB, not 10, and a batch of five costs one dispatch, which
// on a GPU is far closer to the cost of one clip than of five.
//
// Streaming models are not served this way: an active stream holds engine
// state between feeds, so those keep a copy per stream. They are a third of
// the size, and it is the phrase-at-a-time models — one in particular — that
// made memory the problem.
type txBatchRequest struct {
	pcm   []float32
	reply chan txBatchReply
}

type txBatchReply struct {
	text string
	err  error
}

type txBatchService struct {
	shared  *sharedTxModel
	key     string
	session uintptr
	// lang mirrors transcribeModel.lang: NUL-terminated, heap-resident.
	lang     []byte
	abort    *txAbortHandle
	onGPU    bool
	requests chan txBatchRequest
	refs     int
	closed   chan struct{}
	// ready is closed once the service is usable (or failed, with err set).
	// Streams that arrive while the weights are still loading wait on it
	// rather than loading their own copy, which is the entire point.
	ready chan struct{}
	err   error
}

var (
	txServiceLock sync.Mutex
	txServices    = map[string]*txBatchService{}
)

// acquireTxBatchService returns the shared service for a model, starting it on
// first use.
func acquireTxBatchService(path string, backend int32, cfg captionConfig, alive func() bool) (*txBatchService, error) {
	key := fmt.Sprintf("%s|%d", path, backend)
	txServiceLock.Lock()
	if svc, ok := txServices[key]; ok {
		svc.refs++
		n := svc.refs
		txServiceLock.Unlock()
		select {
		case <-svc.ready:
		case <-time.After(150 * time.Second):
			txServiceLock.Lock()
			svc.refs--
			txServiceLock.Unlock()
			return nil, fmt.Errorf("the shared model is still loading, or stuck; this tune runs without captions")
		}
		if svc.err != nil {
			return nil, svc.err
		}
		logger("[CC] Sharing the copy of %s already in memory (%d streams on it)", filepath.Base(path), n)
		return svc, nil
	}
	// Claim the key before the slow work, so every stream that arrives during
	// the load waits for this copy instead of starting another.
	svc := &txBatchService{
		abort:    &txAbortHandle{},
		onGPU:    backend != txBackendCPU,
		requests: make(chan txBatchRequest, 32),
		refs:     1,
		closed:   make(chan struct{}),
		ready:    make(chan struct{}),
	}
	txServices[key] = svc
	txServiceLock.Unlock()

	fail := func(err error) (*txBatchService, error) {
		txServiceLock.Lock()
		delete(txServices, key)
		txServiceLock.Unlock()
		svc.err = err
		close(svc.ready)
		return nil, err
	}
	shared, mkey, err := acquireTxModel(path, backend, alive)
	if err != nil {
		return fail(err)
	}
	sp := txSessionParams{}
	txSessionParamsInit(unsafe.Pointer(&sp))
	sp.nThreads = int32(captionThreads(cfg))
	sp.nCtx = captionDecoderCtx
	var session uintptr
	if st := txSessionInit(shared.handle, unsafe.Pointer(&sp), unsafe.Pointer(&session)); st != txOK || session == 0 {
		releaseTxModel(mkey, shared)
		return fail(fmt.Errorf("opening the shared session: %s", txStatusString(st)))
	}
	svc.shared, svc.key, svc.session = shared, mkey, session
	if l := cfg.Language; l != "" && l != "auto" {
		svc.lang = append([]byte(l), 0)
	}
	txSetAbortCallback(session, txAbortCallback(), unsafe.Pointer(&svc.abort.deadlineUnixNano))
	close(svc.ready)
	go svc.run()
	return svc, nil
}

func (svc *txBatchService) release() {
	txServiceLock.Lock()
	svc.refs--
	if svc.refs > 0 {
		txServiceLock.Unlock()
		return
	}
	delete(txServices, svc.key)
	txServiceLock.Unlock()
	close(svc.closed)
}

// run is the service loop: wait for one phrase, sweep up everything else that
// arrived meanwhile, run the lot as one batch, answer everyone.
//
// The sweep is what makes the latency work. Nothing waits to fill a batch: a
// lone phrase runs alone, immediately. Batching happens by itself exactly when
// it is needed — while one batch runs, the other tuners' phrases queue, and
// the next dispatch takes them all at once.
func (svc *txBatchService) run() {
	defer func() {
		if svc.session != 0 {
			txSessionFree(svc.session)
		}
		releaseTxModel(svc.key, svc.shared)
		logger("[CC] Shared model released")
	}()
	for {
		var first txBatchRequest
		select {
		case <-svc.closed:
			return
		case first = <-svc.requests:
		}
		batch := []txBatchRequest{first}
		for len(batch) < 16 {
			select {
			case r := <-svc.requests:
				batch = append(batch, r)
			default:
				goto ready
			}
		}
	ready:
		svc.dispatch(batch)
	}
}

func (svc *txBatchService) dispatch(batch []txBatchRequest) {
	ptrs := make([]unsafe.Pointer, len(batch))
	lens := make([]int32, len(batch))
	for i, r := range batch {
		ptrs[i] = unsafe.Pointer(&r.pcm[0])
		lens[i] = int32(len(r.pcm))
	}
	p := txRunParams{}
	txRunParamsInit(unsafe.Pointer(&p))
	p.timestamps = txTimestampsNone
	if len(svc.lang) > 0 {
		p.language = &svc.lang[0]
	}

	if svc.shared != nil {
		svc.shared.compute.Lock()
	}
	held := false
	if svc.onGPU {
		gpuGate <- struct{}{}
		held = true
	}
	atomic.StoreInt64(&svc.abort.deadlineUnixNano, time.Now().Add(txRunDeadline).UnixNano())
	st := txRunBatch(svc.session, unsafe.Pointer(&ptrs[0]), unsafe.Pointer(&lens[0]), int32(len(batch)), unsafe.Pointer(&p))
	atomic.StoreInt64(&svc.abort.deadlineUnixNano, 0)
	if held {
		<-gpuGate
	}
	if svc.shared != nil {
		svc.shared.compute.Unlock()
	}
	for _, r := range batch {
		runtime.KeepAlive(r.pcm)
	}
	runtime.KeepAlive(svc.lang)
	runtime.KeepAlive(svc.abort)

	if st != txOK {
		err := fmt.Errorf("%s", txStatusString(st))
		for _, r := range batch {
			r.reply <- txBatchReply{err: err}
		}
		return
	}
	nres := int(txBatchNResults(svc.session))
	for i, r := range batch {
		if i >= nres {
			r.reply <- txBatchReply{err: fmt.Errorf("no result for this phrase")}
			continue
		}
		if pst := txBatchStatus(svc.session, int32(i)); pst != txOK {
			r.reply <- txBatchReply{err: fmt.Errorf("%s", txStatusString(pst))}
			continue
		}
		r.reply <- txBatchReply{text: cleanRecognized(txBatchFullText(svc.session, int32(i)))}
	}
}

// batchClient is the per-stream face of the shared service. It satisfies
// recognizer; the streaming entry points refuse, which sends the caption
// engine down the phrase-at-a-time path — the only path these models have.
type batchClient struct{ svc *txBatchService }

func (b *batchClient) transcribe(pcm []float32) (string, error) {
	if len(pcm) < asrSampleRate/4 {
		return "", nil
	}
	reply := make(chan txBatchReply, 1)
	select {
	case b.svc.requests <- txBatchRequest{pcm: pcm, reply: reply}:
	default:
		return "", fmt.Errorf("the shared recognizer is full; this phrase is dropped")
	}
	select {
	case r := <-reply:
		return r.text, r.err
	case <-time.After(txRunDeadline + 8*time.Second):
		// The service dispatch is itself bounded by the run deadline, so this
		// only fires if the service has died with the request in hand. Waiting
		// forever here would freeze this stream's recognizer for the tune.
		return "", fmt.Errorf("the shared recognizer did not answer")
	}
}

func (b *batchClient) beginStream(string) error           { return fmt.Errorf("phrase at a time only") }
func (b *batchClient) feedStream([]float32) *streamResult { return nil }
func (b *batchClient) finishStream() *streamResult        { return nil }
func (b *batchClient) idleFlush() *streamResult           { return nil }
func (b *batchClient) Close()                             { b.svc.release() }

// runWithDeadline runs fn and stops waiting for it after d.
//
// It cannot stop fn — native code has no cancellation — so on timeout fn is
// left running and its completion is reported to the returned channel. What
// this buys is containment: a load that has wedged inside a driver costs the
// one stream that wanted it, instead of holding a lock that every other
// stream then queues behind. A model wedging is an inconvenience; a model
// wedging everything is an outage.
func runWithDeadline(d time.Duration, what string, fn func()) (finished bool, done <-chan struct{}) {
	ch := make(chan struct{})
	go func() { defer close(ch); fn() }()
	select {
	case <-ch:
		return true, ch
	case <-time.After(d):
		logger("[CC] %s did not finish within %s; carrying on without it (it may still complete in the background)", what, d)
		return false, ch
	}
}

// initTranscribeDeadline is initTranscribe with the waiting bounded. The Once
// inside initTranscribe means a wedged first call would block every later
// caller forever; this way they get an error and their tunes run uncaptioned.
func initTranscribeDeadline(variant string) error {
	var err error
	ok, _ := runWithDeadline(60*time.Second, "loading the speech engine", func() { err = initTranscribe(variant) })
	if !ok {
		return fmt.Errorf("the speech engine is not responding")
	}
	return err
}

// txLoadGate limits how many sets of weights load at once.
//
// Seven tuners starting together meant seven multi-gigabyte loads at once, each
// fighting the others for memory bandwidth and all of them slower for it. One
// at a time fixed that and introduced a worse problem: the loads queued, and
// the last tuner to start waited for the six in front of it, which is most of a
// minute before its captions appear.
//
// Two at a time is the compromise. Memory bandwidth is not saturated by a pair,
// and the queue is half as long. Nothing waits on this except captions.
var txLoadGate = make(chan struct{}, 2)

// acquireTxModel loads a copy of the weights for one stream. alive is checked
// once the gate is held, so a stream that ended while queued never loads at all.
func acquireTxModel(path string, backend int32, alive func() bool) (*sharedTxModel, string, error) {
	key := fmt.Sprintf("%s|%d", path, backend)
	select {
	case txLoadGate <- struct{}{}:
	case <-time.After(90 * time.Second):
		return nil, "", fmt.Errorf("gave up queueing behind other model loads")
	}
	defer func() { <-txLoadGate }()
	if alive != nil && !alive() {
		return nil, "", fmt.Errorf("the stream ended before its model finished loading")
	}
	txModelLock.Lock()
	live := txLive[key]
	txModelLock.Unlock()
	if live > 0 {
		logger("[CC] Loading a copy of %s for another stream (%d already in memory)", filepath.Base(path), live)
	}

	// Loaded outside the lock: this takes tens of seconds for a large model and
	// holding the lock across it would stall an unrelated stream trying to stop.
	load := txLoadParams{}
	txLoadParamsInit(unsafe.Pointer(&load))
	load.backend = backend
	var h uintptr
	var st int32
	ok, done := runWithDeadline(120*time.Second, "loading "+filepath.Base(path), func() {
		st = txModelLoadFile(path, unsafe.Pointer(&load), unsafe.Pointer(&h))
	})
	if !ok {
		// If the wedged load ever does finish, its weights belong to nobody:
		// give them straight back rather than leaking gigabytes.
		go func() {
			<-done
			if st == txOK && h != 0 {
				txModelFree(h)
				logger("[CC] A model load that had been given up on finished late; its memory was freed")
			}
		}()
		return nil, "", fmt.Errorf("loading %s took too long; it may be wedged in the driver", filepath.Base(path))
	}
	if st != txOK || h == 0 {
		return nil, "", fmt.Errorf("loading %s: %s", filepath.Base(path), txStatusString(st))
	}
	m := &sharedTxModel{handle: h, refs: 1}
	txModelLock.Lock()
	txLive[key]++
	n := txLive[key]
	txModelLock.Unlock()
	logger("[CC] Loaded %s (%d in memory)", filepath.Base(path), n)
	return m, key, nil
}

// releaseTxModel gives the memory back.
func releaseTxModel(key string, m *sharedTxModel) {
	if m == nil {
		return
	}
	txModelLock.Lock()
	if m.refs <= 0 {
		// Already given back. Freeing native memory a second time is a crash
		// rather than an error, so this refuses rather than trusting callers.
		txModelLock.Unlock()
		return
	}
	if m.refs--; m.refs > 0 {
		txModelLock.Unlock()
		return
	}
	handle := m.handle
	m.handle = 0
	if txLive[key] > 0 {
		txLive[key]--
	}
	n := txLive[key]
	txModelLock.Unlock()

	// Freed outside the lock for the same reason it is loaded outside it.
	txModelFree(handle)
	logger("[CC] Freed %s (%d in memory)", filepath.Base(strings.SplitN(key, "|", 2)[0]), n)
}

// txLogCallback filters what the engine has to say down to what a person
// running a TV proxy would want to see: warnings and errors, nothing else.
// Chatter about buffer allocations is the engine's business.
var (
	txLogOnce sync.Once
	txLogPtr  uintptr
)

func txLogCallback() uintptr {
	txLogOnce.Do(func() {
		// Written to from the engine's own worker threads, mid-decode. Writing
		// the log from here would put a disk or pipe write inside a compute
		// graph, where the run deadline cannot interrupt it — a slow log would
		// become a slow decode. So the text is copied and handed to a goroutine,
		// and if even that would wait, the line is dropped. Losing a warning is
		// better than stalling the decode that produced it.
		lines := make(chan string, 64)
		go func() {
			for text := range lines {
				logger("[CC] engine: %s", text)
			}
		}()
		txLogPtr = purego.NewCallback(func(level int32, msg *byte, _ unsafe.Pointer) {
			// 2 is WARN and 3 is ERROR; INFO, DEBUG and continuation lines are
			// dropped where they are made rather than filtered later.
			if level != 2 && level != 3 {
				return
			}
			text := strings.TrimSpace(txGoString(msg))
			if text == "" {
				return
			}
			select {
			case lines <- text:
			default:
			}
		})
	})
	return txLogPtr
}

// txGoString reads a NUL-terminated string the engine owns.
func txGoString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// Streaming models are trained on a menu of context windows and the caller
// picks one when the stream opens. Left unasked, they use the first entry,
// which is the most accurate and the slowest — and for the Unified that is
// 5600 ms of left context, a 1040 ms chunk and 1040 ms of lookahead, so every
// chunk re-runs the encoder over nearly eight seconds of audio and commits a
// second behind. That is a fine default for transcribing a file and the wrong
// one for live television, where it shows up as captions trailing the picture
// and as work that multiplies by the number of tuners.
//
// Two families take two different knobs, and a model accepts exactly one of
// them, so which to use is asked rather than assumed.
const (
	txExtKindParakeetStream   = 0x54534B50 // 'PKST', cache-aware
	txExtKindParakeetBuffered = 0x53424B50 // 'PKBS', chunked attention
	txExtSlotStream           = 0
)

type (
	txExt struct {
		size uint64
		kind uint32
		_    uint32
	}
	txParakeetStreamExt struct {
		ext             txExt
		attContextRight int32
		_               int32
	}
	txParakeetBufferedExt struct {
		ext     txExt
		leftMS  int32
		chunkMS int32
		rightMS int32
		_       int32
	}
)

// captionLatency is the setting on the page, and the windows each one asks for.
// Everything is in milliseconds and must be a multiple of the 80 ms encoder
// frame; a tuple outside the model's training menu is refused by the engine
// rather than silently rounded, which is why an unusable choice falls back to
// the model's own default instead of failing the stream.
type captionLatency struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	// chunk and right are the buffered family's window; right alone is the
	// cache-aware family's lookahead. -1 means the model's default.
	chunkMS, rightMS int32
	cacheRight       int32
	// phraseSec is the same trade for a model that reads a phrase at a time,
	// and it is a latency ceiling more than a throughput knob.
	//
	// A phrase's first word waits the whole phrase before it can appear: cut at
	// eight seconds, the opening words of a sentence reach the screen nine
	// seconds after they were said, which reads as the captions ignoring the
	// programme. The number here is therefore roughly the worst-case caption
	// lag, and it is kept near what live broadcast captioning runs.
	//
	// The throughput argument for long phrases died when batching arrived. Each
	// call once paid its own start-up cost, so short phrases meant paying it
	// many times a minute; now every tuner's phrases go through one shared
	// dispatch, the cost is paid per batch, and when load rises the queue
	// deepens and the batches grow on their own — efficiency now scales with
	// demand instead of with phrase length. What longer phrases still buy is a
	// little acoustic context and fewer phrase boundaries, which is where the
	// mistakes cluster; that is why the accurate setting keeps a longer window,
	// not speed.
	phraseSec float64
}

var captionLatencies = []captionLatency{
	{
		Key: "fast", Name: "Lowest delay",
		Desc:    "Captions follow speech as closely as they can: phrases are cut within two and a half seconds, so even the first word of a sentence lands about three seconds behind at worst. Slightly less accurate — short phrases give the model less to work with, and mistakes cluster at the cuts.",
		chunkMS: 80, rightMS: 80, cacheRight: 1, phraseSec: 2.5,
	},
	{
		Key: "balanced", Name: "Balanced (recommended)",
		Desc:    "Phrases are cut within four seconds, which puts captions two to four seconds behind the picture — about what live broadcast captioning runs. The right answer for most setups.",
		chunkMS: 480, rightMS: 480, cacheRight: 6, phraseSec: 4,
	},
	{
		Key: "accurate", Name: "Most accurate",
		Desc:    "The most accurate, at the cost of arriving latest. Streaming models use their own published settings; a phrase-at-a-time model gets up to six seconds per phrase — more context and fewer cuts, which is where mistakes cluster — so the start of a sentence can trail six or seven seconds.",
		chunkMS: -1, rightMS: -1, cacheRight: -1, phraseSec: 6,
	},
}

func findCaptionLatency(key string) captionLatency {
	for _, l := range captionLatencies {
		if l.Key == key {
			return l
		}
	}
	return captionLatencies[1]
}

// transcribeModel is a loaded model and the session that runs it.
type transcribeModel struct {
	model uintptr
	// shared is the weights this session borrows, and the lock that keeps one
	// tuner's decode from overlapping another's on the same model.
	shared *sharedTxModel
	// modelKey identifies those weights for release.
	modelKey string
	session  uintptr
	// lang is the NUL-terminated language code handed to the engine. It lives
	// on the heap for the life of the model for the same reason parakeet's
	// event mask does: a pointer into a goroutine stack is not safe to give to
	// C, because the stack can move.
	lang []byte
	// committed counts the bytes of the continuous transcript already shown, so
	// each feed contributes only what is new. The engine's committed text is
	// append-only by contract, which is what makes this safe.
	committed int
	streaming bool
	mu        sync.Mutex // the session holds decoder state: one call at a time
	// abort carries the current run's deadline to the engine's callback. It is
	// allocated once and never moves for the life of the model.
	abort *txAbortHandle
	// onGPU records that this copy decodes on the accelerator, which is shared
	// between every stream and therefore rationed.
	onGPU bool
	// latency is the context window asked for when a stream opens.
	latency string
	// heldGPU is set between arm and disarm while this stream holds a place on
	// the accelerator. Only the goroutine inside a call touches it.
	heldGPU bool
}

// loadTranscribe opens the weights the user downloaded.
func loadTranscribe(gguf string, cfg captionConfig, alive func() bool) (*transcribeModel, error) {
	m, _ := findCaptionModel(cfg.Model)
	variant, err := captionVariantFor(m)
	if err != nil {
		return nil, err
	}
	if err := initTranscribeDeadline(variant); err != nil {
		return nil, err
	}
	weights, err := filepath.Abs(gguf)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(weights); err != nil {
		return nil, err
	}

	shared, key, err := acquireTxModel(weights, txBackend(variant), alive)
	if err != nil {
		return nil, err
	}

	sp := txSessionParams{}
	txSessionParamsInit(unsafe.Pointer(&sp))
	sp.nThreads = int32(captionThreads(cfg))
	// The decoder window is sized for long-form transcription by default — the
	// log shows a thousand-token cache allocated for every run — and a caption
	// phrase is a sentence or two. Capping it bounds that allocation without
	// coming near what a phrase actually generates. It only narrows: a model
	// whose own maximum is lower keeps its own.
	sp.nCtx = captionDecoderCtx
	var session uintptr
	if st := txSessionInit(shared.handle, unsafe.Pointer(&sp), unsafe.Pointer(&session)); st != txOK || session == 0 {
		releaseTxModel(key, shared)
		return nil, fmt.Errorf("opening a session: %s", txStatusString(st))
	}

	if txModelBackend != nil {
		got := txModelBackend(shared.handle)
		if asked := txBackend(variant); got != asked && asked != txBackendAuto {
			logger("[CC] WARNING: asked for the %s backend and the engine is using %s instead. The %s module did not load — check the driver inside the container, and /dev/dri for Vulkan.",
				txBackendName(asked), txBackendName(got), txBackendName(asked))
		} else {
			logger("[CC] %s is running on %s", filepath.Base(gguf), txBackendName(got))
		}
	}

	t := &transcribeModel{model: shared.handle, shared: shared, modelKey: key, session: session,
		abort: &txAbortHandle{}, onGPU: variant != "cpu", latency: cfg.Latency}
	txSetAbortCallback(session, txAbortCallback(), unsafe.Pointer(&t.abort.deadlineUnixNano))
	// "auto" is this page's word for detection, not the engine's: it wants a
	// null language for that, and would reject "auto" as a locale.
	if l := cfg.Language; l != "" && l != "auto" {
		t.lang = append([]byte(l), 0)
	}
	return t, nil
}

func (t *transcribeModel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session != 0 {
		txSessionFree(t.session)
		t.session = 0
	}
	if t.model != 0 {
		// Given back now rather than held for a stream that may never come.
		releaseTxModel(t.modelKey, t.shared)
		t.model = 0
		t.shared = nil
	}
}

// runParams is the per-call configuration.
//
// Timestamps are turned off rather than left at the default, which asks each
// family for the finest alignment it can manage. Captions are shown the moment
// the words are ready and never seek, so where a word fell in the audio is work
// that would be done and then thrown away.
func (t *transcribeModel) runParams() txRunParams {
	p := txRunParams{}
	txRunParamsInit(unsafe.Pointer(&p))
	p.timestamps = txTimestampsNone
	if len(t.lang) > 0 {
		p.language = &t.lang[0]
	}
	return p
}

// arm and disarm bracket a call into the engine with the run deadline, so no
// single call can take the captions with it. Every entry point that decodes
// uses them, not just the offline one: a streaming feed that never returns
// stops captions just as completely.
// arm brackets a decode: the model's own lock, a place on the accelerator, and
// the run deadline. armSetup is the same without the accelerator, for the calls
// that open and close a stream.
//
// Opening a session is bookkeeping, not compute, and making it queue behind two
// running decodes was making a new stream wait seconds for a place it did not
// need — with phrases now up to fourteen seconds long, sometimes a great many
// seconds. Starting a stream should never wait on other streams' work.
func (t *transcribeModel) arm() {
	t.armSetup()
	t.heldGPU = t.enterGPU()
}

func (t *transcribeModel) armSetup() {
	if t.shared != nil {
		t.shared.compute.Lock()
	}
	atomic.StoreInt64(&t.abort.deadlineUnixNano, time.Now().Add(txRunDeadline).UnixNano())
}

func (t *transcribeModel) disarmSetup() {
	atomic.StoreInt64(&t.abort.deadlineUnixNano, 0)
	runtime.KeepAlive(t.abort)
	if t.shared != nil {
		t.shared.compute.Unlock()
	}
}

func (t *transcribeModel) disarm() {
	held := t.heldGPU
	t.heldGPU = false
	t.leaveGPU(held)
	t.disarmSetup()
}

// transcribe runs one utterance of 16 kHz mono audio through the model. This is
// the path an offline model takes: it reads a whole phrase and writes it out,
// which is why it cannot be as immediate as a streaming model however fast the
// hardware is.
func (t *transcribeModel) transcribe(pcm []float32) (string, error) {
	if len(pcm) < asrSampleRate/4 {
		return "", nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session == 0 {
		return "", fmt.Errorf("model is closed")
	}
	p := t.runParams()
	t.arm()
	st := txRun(t.session, unsafe.Pointer(&pcm[0]), int32(len(pcm)), unsafe.Pointer(&p))
	t.disarm()
	// The engine reads the samples during the call, so keep them reachable for
	// its duration rather than trusting the argument alone to pin them.
	runtime.KeepAlive(pcm)
	runtime.KeepAlive(t.lang)
	runtime.KeepAlive(t.abort)
	if st != txOK {
		if txWasAborted(t.session) {
			// Whatever went wrong with this phrase, the next one gets a clean
			// try. This is the difference between one lost sentence and a
			// recording with no captions after the first ten seconds.
			return "", fmt.Errorf("gave up on this phrase after %s", txRunDeadline)
		}
		return "", fmt.Errorf("%s", txStatusString(st))
	}
	return cleanRecognized(txFullText(t.session)), nil
}

// beginStream opens a continuous session. A model that cannot transcribe
// continuously fails here and the caller falls back to a phrase at a time.
func (t *transcribeModel) beginStream(language string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.beginStreamLocked()
}

// beginStreamLocked opens the session with the lock already held.
func (t *transcribeModel) beginStreamLocked() error {
	if t.session == 0 {
		return fmt.Errorf("model is closed")
	}
	sp := txStreamParams{}
	txStreamParamsInit(unsafe.Pointer(&sp))
	// Ask for a window if this model takes one. Keeping the struct alive for
	// the duration of the call is the caller's job: the engine reads through
	// the pointer during stream_begin and does not retain it.
	var pkExt txParakeetStreamExt
	var bufExt txParakeetBufferedExt
	lat := findCaptionLatency(t.latency)
	switch {
	case t.model != 0 && txAcceptsExtKind(t.model, txExtSlotStream, txExtKindParakeetBuffered):
		txPkBufStreamExtInit(unsafe.Pointer(&bufExt))
		bufExt.chunkMS, bufExt.rightMS = lat.chunkMS, lat.rightMS
		sp.family = uintptr(unsafe.Pointer(&bufExt))
	case t.model != 0 && txAcceptsExtKind(t.model, txExtSlotStream, txExtKindParakeetStream):
		txPkStreamExtInit(unsafe.Pointer(&pkExt))
		pkExt.attContextRight = lat.cacheRight
		sp.family = uintptr(unsafe.Pointer(&pkExt))
	}
	// The run params carry the language into the stream as well, so a
	// prompt-conditioned model is told which locale to expect rather than
	// having to work it out from the first few seconds of audio.
	rp := t.runParams()
	t.armSetup()
	st := txStreamBegin(t.session, unsafe.Pointer(&rp), unsafe.Pointer(&sp))
	t.disarmSetup()
	runtime.KeepAlive(t.lang)
	runtime.KeepAlive(&pkExt)
	runtime.KeepAlive(&bufExt)
	if st != txOK && sp.family != 0 {
		// The window has to be one the model was trained on, and the engine
		// refuses anything else rather than rounding it. Fall back to the
		// model's own rather than leaving the stream unopened.
		logger("[CC] %s window not available on this model (%s); using its default", t.latency, txStatusString(st))
		sp.family = 0
		t.armSetup()
		st = txStreamBegin(t.session, unsafe.Pointer(&rp), unsafe.Pointer(&sp))
		t.disarmSetup()
	}
	if st != txOK {
		return fmt.Errorf("%s", txStatusString(st))
	}
	t.streaming = true
	t.committed = 0
	return nil
}

func (t *transcribeModel) feedStream(pcm []float32) *streamResult {
	if len(pcm) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session == 0 {
		return nil
	}
	if !t.streaming {
		// The session died and the last attempt to reopen it failed. Try again
		// rather than decoding audio into nothing for the rest of the tune: a
		// stream that cannot be reopened once may well open on the next try,
		// and the alternative is captions that never come back.
		//
		// This used to sit behind a guard that returned on exactly this
		// condition, so it never ran once in its life, and the watchdog then
		// turned a single slow feed into permanent silence — the failure it
		// exists to prevent.
		if err := t.beginStreamLocked(); err != nil {
			return nil
		}
		logger("[CC] continuous recognition recovered after a failed session")
	}
	u := txStreamUpdate{}
	txStreamUpdateInit(unsafe.Pointer(&u))
	t.arm()
	st := txStreamFeed(t.session, unsafe.Pointer(&pcm[0]), int32(len(pcm)), unsafe.Pointer(&u))
	t.disarm()
	runtime.KeepAlive(pcm)
	if st != txOK {
		// A failed feed leaves the session unusable; mark it so the next one
		// reopens rather than feeding a stream that will never answer.
		t.streaming = false
		return nil
	}
	if !u.committedChanged {
		return nil
	}
	return t.takeCommitted(false)
}

// idleFlush ends the utterance and opens a new one, which is how the last words
// of a sentence get said while the room is still quiet.
//
// Finalizing is the only way to make this engine release text it is holding for
// confirmation, and finalizing closes the stream, so a fresh one is started
// straight after. That is the right shape anyway: a pause is an utterance
// boundary, and the next sentence begins with no assumptions carried into it.
func (t *transcribeModel) idleFlush() *streamResult {
	t.mu.Lock()
	if t.session == 0 || !t.streaming {
		t.mu.Unlock()
		return nil
	}
	u := txStreamUpdate{}
	txStreamUpdateInit(unsafe.Pointer(&u))
	t.arm()
	st := txStreamFinalize(t.session, unsafe.Pointer(&u))
	t.disarm()
	var r *streamResult
	if st == txOK {
		r = t.takeCommitted(true)
	}
	t.streaming = false
	if err := t.beginStreamLocked(); err != nil {
		// Not fatal: the next feed tries again rather than giving up on
		// captions for the rest of the tune.
		logger("[CC] could not reopen the continuous session after a pause: %v", err)
	}
	t.mu.Unlock()

	if r != nil {
		// The talking stopped, so whatever just came out finishes a sentence.
		r.EOU = 1
	}
	return r
}

func (t *transcribeModel) finishStream() *streamResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session == 0 || !t.streaming {
		return nil
	}
	u := txStreamUpdate{}
	txStreamUpdateInit(unsafe.Pointer(&u))
	t.arm()
	st := txStreamFinalize(t.session, unsafe.Pointer(&u))
	t.disarm()
	if st != txOK {
		return nil
	}
	r := t.takeCommitted(true)
	if r != nil {
		r.EOU = 1
	}
	return r
}

// takeCommitted returns the part of the transcript that has appeared since the
// last call.
//
// The engine offers two views of a stream in progress: a raw hypothesis it may
// rewrite anywhere, and a committed prefix it promises never to rewrite. This
// takes the committed one. Captions are burned into the transport stream a
// couple of bytes at a time and cannot be taken back, so text that might be
// revised is of no use here — better a word later than a word retracted.
//
// The caller must hold the lock. The returned pointers belong to the session
// and are only valid until the next call, which is why the text is copied out
// before anything else happens.
func (t *transcribeModel) takeCommitted(final bool) *streamResult {
	var view txStreamText
	txStreamTextInit(unsafe.Pointer(&view))
	if st := txStreamGetText(t.session, unsafe.Pointer(&view)); st != txOK {
		return nil
	}
	full := txGoStringN(view.committedText, view.committedTextBytes)
	if len(full) < t.committed {
		// Append-only is best effort rather than a guarantee. If the prefix
		// ever shrinks, start again from what is there instead of slicing off
		// the end of a shorter string.
		t.committed = 0
	}
	tail := full[t.committed:]

	// Commit arrives as bytes, not as words, so the end of it is very often
	// half a word: "broadcast" appears as "broad" and then, a moment later,
	// "cast". Taking whatever has arrived and splitting it on spaces therefore
	// put a caption on screen reading "broad cast", one fragment at a time —
	// which is what the transcript looked like, and why the model appeared to
	// have lost its accuracy when it had not.
	//
	// So only whole words are taken: everything up to the last space, with any
	// trailing fragment left where it is until the rest of it arrives. On the
	// last call of a stream there is no more coming, and the remainder is a
	// whole word by definition.
	fresh := tail
	if !final {
		cut := strings.LastIndexAny(tail, " \t\r\n")
		if cut < 0 {
			// Nothing but a fragment so far. Leave it for next time.
			return nil
		}
		fresh = tail[:cut]
		t.committed += cut
	} else {
		t.committed = len(full)
	}
	fresh = strings.TrimSpace(fresh)
	if fresh == "" {
		return nil
	}

	fields := strings.Fields(fresh)
	r := &streamResult{Text: fresh, Words: make([]streamWord, 0, len(fields))}
	for _, w := range fields {
		if c := cleanRecognized(w); c != "" {
			r.Words = append(r.Words, streamWord{W: c})
		}
	}
	if len(r.Words) == 0 {
		return nil
	}
	// The engines report the end of an utterance differently: parakeet.cpp
	// raises a flag, and this one does not. Sentence-ending punctuation is the
	// signal that is actually available here, and it is a good one, because
	// every model reaching this path writes punctuation.
	if last := r.Words[len(r.Words)-1].W; strings.HasSuffix(last, ".") ||
		strings.HasSuffix(last, "?") || strings.HasSuffix(last, "!") {
		r.EOU = 1
	}
	return r
}

// txGoStringN copies n bytes out of engine memory. Unlike parakeet.cpp's
// strings, these are borrowed rather than handed over, so there is nothing to
// free: the session owns them until its next call.
func txGoStringN(p *byte, n uint64) string {
	if p == nil || n == 0 {
		return ""
	}
	return string(unsafe.Slice(p, int(n)))
}

// ---------------------------------------------------------------------------
// Listening
// ---------------------------------------------------------------------------

// captionEngine turns the transport stream into captions: ffmpeg decodes the
// audio, a voice activity check cuts it into phrases, and each phrase is
// recognized and handed to the CEA-608 encoder.
type captionEngine struct {
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
		enc:     newCEA608(cfg.Style, cfg.Uppercase),
		label:   label,
		cfg:     cfg,
		audioCh: make(chan []byte, 64),
		phrases: make(chan phraseItem, 3),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	// Deliberately not started here. Loading weights moves gigabytes through
	// memory, and doing that while the tuner is still negotiating slows the
	// thing that actually matters — even on its own goroutine, the bandwidth is
	// shared. The load waits until the stream is delivering video, which is the
	// moment the tune is known to have worked and the point at which nothing is
	// waiting on the machine any more.
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
func (e *captionEngine) start(cfg captionConfig, m captionModel) {
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
		model, err = loadRecognizer(m, cfg, alive)
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
		if backoff < 2*time.Minute {
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
		if err := model.beginStream(cfg.Language); err != nil {
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
	where := currentEngineVariant()
	if where == "cpu" {
		where = "processor"
	}
	logger("[CC] %s captions on: %s, %s, %s, on the %s, ready in %s",
		e.label, m.Key, cfg.Language, mode, where, time.Since(began).Round(time.Millisecond))
}

// loadRecognizer opens a model on whichever engine can run it.
func loadRecognizer(m captionModel, cfg captionConfig, alive func() bool) (recognizer, error) {
	if runtimeOf(m) == rtTranscribe {
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
			return &batchClient{svc: svc}, nil
		}
		return loadTranscribe(modelPath(m), cfg, alive)
	}
	return loadParakeet(modelPath(m), cfg.Language)
}

// runtimeOf is the engine a model needs. A catalog entry that names none is a
// Parakeet one, which is what every entry was before there was a second engine.
func runtimeOf(m captionModel) string {
	if m.Runtime == "" {
		return rtParakeet
	}
	return m.Runtime
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
	vadFrame     = asrSampleRate / 50 // 20 ms
	vadMinSpeech = 0.6                // ignore blips shorter than this
	vadMinPhrase = 1.8                // past this, a word gap is enough to cut
	vadWordGap   = 0.15               // the gap between two spoken words
	vadSilence   = 0.45               // a real pause: end the phrase whatever its length
	vadMaxPhrase = 3.5                // backstop, so captions never fall this far behind
	vadLead      = 0.20               // audio kept before speech, so words are not clipped

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
	// It fails towards hearing. A noisy channel gets its noise transcribed,
	// which is untidy and was the behaviour before any of this; the alternative
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
func (e *captionEngine) listenStreaming(pcm io.ReadCloser) {
	defer e.finish()
	defer pcm.Close()

	// 100 ms per feed: short enough that nothing waits on a buffer, long enough
	// that the call overhead is irrelevant next to the work inside.
	const chunk = asrSampleRate / 10
	const chunkSec = 0.1
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

	for {
		select {
		case <-e.closed:
			return
		default:
		}
		if _, err := io.ReadFull(pcm, raw); err != nil {
			take(e.model.finishStream())
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
			quiet, settled = 0, true
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
		take(e.model.feedStream(buf))

		// Watch for the talking stopping, so the end of a sentence is not left
		// waiting for the next one to start. The floor tracks the channel's own
		// noise, because broadcast audio is never actually silent.
		rms := math.Sqrt(sum / float64(len(buf)))
		peak = vadPeak(peak, rms)
		if rms > vadBar(floor, peak) {
			quiet, settled = 0, false
			continue
		}
		floor = math.Min(0.995*floor+0.005*rms, vadFloorMax)
		if settled {
			continue
		}
		if quiet += chunkSec; quiet >= flushSilence {
			take(e.model.idleFlush())
			settled = true
		}
	}
}

// show puts recognized text on screen, honouring the configured offset.
func (e *captionEngine) show(text string, breakAfter bool) {
	if d := e.cfg.OffsetSec; d > 0 {
		time.AfterFunc(time.Duration(d)*time.Second, func() { e.enc.pushText(text, breakAfter) })
		return
	}
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
	floor := 0.005
	peak := 0.0
	decoderTries := 0
	var frames, cutThisMinute int
	maxPhrase := findCaptionLatency(e.cfg.Latency).phraseSec
	if maxPhrase <= 0 {
		maxPhrase = vadMaxPhrase
	}

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
				lead := int(vadLead * asrSampleRate)
				if len(pending) > lead {
					pending = pending[len(pending)-lead:]
				}
			}
			silenceRun = 0
			speechLen += float64(vadFrame) / asrSampleRate
		} else {
			silenceRun += float64(vadFrame) / asrSampleRate
		}
		pending = append(pending, frame...)

		if !speaking {
			// Hold only the lead-in window while the channel is quiet.
			keep := int((vadLead + 0.1) * asrSampleRate)
			if len(pending) > keep {
				pending = pending[len(pending)-keep:]
			}
			continue
		}

		phrase := float64(len(pending)) / asrSampleRate
		ended := silenceRun >= vadSilence
		// Long phrases are cheaper per second of television, so how long to let
		// one run is the same trade as a streaming model's lookahead and is
		// made with the same setting.
		gapped := phrase >= maxPhrase*0.55 && silenceRun >= vadWordGap
		forced := phrase >= maxPhrase
		if !ended && !gapped && !forced {
			continue
		}

		audio := pending
		if forced {
			// Carry a little audio forward so a mid-word cut is not lost.
			lead := int(vadLead * asrSampleRate)
			if len(audio) > lead {
				pending = append([]float32(nil), audio[len(audio)-lead:]...)
			} else {
				pending = nil
			}
			silenceRun = 0
		} else {
			pending = nil
			speaking = false
		}
		if speechLen < vadMinSpeech {
			continue
		}
		speechLen = 0
		cutThisMinute++
		e.queue(audio)
	}
}

// phraseItem is a cut phrase and the moment it was cut. The stamp is what
// keeps the pipeline honest: any stage may compare it against now and refuse
// to spend work on the past.
type phraseItem struct {
	pcm []float32
	cut time.Time
}

// phraseStaleAfter is how old a phrase may be before it is abandoned. Normal
// passage through the pipeline is a second or two; anything past this is a
// backlog, and transcribing a backlog in order is how captions end up narrating
// television from minutes ago.
const phraseStaleAfter = 10 * time.Second

// queue hands a phrase to the recognizer, and never waits for it.
func (e *captionEngine) queue(audio []float32) {
	select {
	case e.phrases <- phraseItem{pcm: audio, cut: time.Now()}:
	default:
		// The recognizer is still working through what it has. Losing this
		// phrase costs a sentence; waiting here would cost the audio stream,
		// which is the trade that made captions stop altogether.
		n := atomic.AddInt64(&e.dropped, 1)
		if n == 1 || n%10 == 0 {
			logger("[CC] %s behind: %d phrases dropped", e.label, n)
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
	for item := range e.phrases {
		select {
		case <-e.closed:
			return
		default:
		}
		// Skip anything that went stale in the queue. Dropping the oldest is
		// what lets the newest stay current: the alternative is every phrase
		// arriving late by however far behind the recognizer once fell.
		if age := time.Since(item.cut); age > phraseStaleAfter {
			n := atomic.AddInt64(&e.skippedStale, 1)
			if n == 1 || n%10 == 0 {
				logger("[CC] %s skipped a phrase %.0fs old to stay current (%d so far)", e.label, age.Seconds(), n)
			}
			continue
		}
		e.caption(item)
	}
}

// caption recognizes one phrase and queues it for display.
func (e *captionEngine) caption(item phraseItem) {
	audio := item.pcm
	secs := float64(len(audio)) / asrSampleRate
	start := time.Now()
	text, err := e.model.transcribe(audio)
	took := time.Since(start)
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
	// Taking longer than the audio lasted is not the same as falling behind,
	// and reporting it as though it were was alarming people whose captions
	// were perfect. Speech has gaps in it: the segmenter only ever hands over
	// the parts somebody was talking, so a phrase holding 2.7 seconds of speech
	// usually has a good deal more than 2.7 seconds of wall clock behind it.
	// A recognizer at just over the length of the speech is comfortably keeping
	// up with the channel.
	//
	// What actually means it is losing is phrases being dropped, which is
	// counted where it happens. So that is what this waits for, and until then
	// a thin margin is reported as a thin margin.
	if took.Seconds() > secs {
		e.slow++
		switch drops := atomic.LoadInt64(&e.dropped); {
		case drops > 0 && (e.slow == 1 || e.slow%20 == 0):
			logger("[CC] %s falling behind: %s for %.1fs of speech, %d dropped", e.label, took.Round(time.Millisecond), secs, drops)
		case e.slow == 1 || e.slow%100 == 0:
			logger("[CC] %s tight on time: %s for %.1fs of speech, keeping up (nothing dropped)", e.label, took.Round(time.Millisecond), secs)
		}
	}
	text = e.trimOverlap(text)
	if text == "" {
		return
	}
	if d := e.cfg.OffsetSec; d > 0 {
		// Hold the phrase back for hand-tuned sync. The video is never delayed,
		// so this only ever pushes text later.
		time.AfterFunc(time.Duration(d)*time.Second, func() { e.enc.push(text) })
		return
	}
	e.enc.push(text)
}

// trimOverlap drops the beginning of a phrase where it repeats the end of the
// one before.
//
// A phrase cut at the ceiling rather than at a pause carries a fraction of a
// second of audio into the next one, so that a word is not sliced in half. That
// audio is recognized in both, which put "and" twice across the join and turned
// "downtown" into "downtown" followed by "town". Up to four words of overlap
// are matched and removed.
func (e *captionEngine) trimOverlap(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 || len(e.tail) == 0 {
		e.rememberTail(words)
		return strings.TrimSpace(text)
	}
	best := 0
	for n := min(len(e.tail), min(len(words), 4)); n > 0; n-- {
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

func (e *captionEngine) rememberTail(words []string) {
	if len(words) == 0 {
		return
	}
	if len(words) > 4 {
		words = words[len(words)-4:]
	}
	e.tail = append([]string(nil), words...)
}

// feed offers stream bytes to the recognizer without blocking.
func (e *captionEngine) feed(b []byte) {
	// The first bytes of video are the signal that the tune succeeded and the
	// machine is free to do something expensive.
	e.begin()

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
	default: // recognizer is behind; drop rather than stall the stream
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
		stopped := true
		select {
		case <-e.done:
		case <-time.After(10 * time.Second):
			stopped = false
			logger("[CC] %s recognizer did not stop in time; leaving its memory alone. Its copy of the model stays loaded until ah4c restarts.", e.label)
		}
		e.mu.Lock()
		model := e.model
		e.mu.Unlock()
		if stopped && model != nil {
			// Closing the recognizer releases its streaming session as well,
			// so the order the two engines want is theirs to decide.
			model.Close()
		}
		logger("[CC] %s captions stopped", e.label)
	})
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
}

// maybeWrapCaptions returns src unchanged unless captions are switched on and
// the selected model is installed, so a tune costs nothing when captions are
// off or half configured.
func maybeWrapCaptions(src io.ReadCloser, tunerIndex int, label string) io.ReadCloser {
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
	m, ok := findCaptionModel(cfg.Model)
	if !ok {
		logger("[CC] %s unknown model %q, captions disabled for this tune", label, cfg.Model)
		return src
	}
	if !modelInstalled(m) {
		logger("[CC] %s model %s is not downloaded, captions disabled for this tune", label, m.Key)
		return src
	}
	if m.NeedsGPU && !gpuAvailable() {
		// Captioning anyway would be worse than not captioning: this model on a
		// processor loses ground against live audio until most of the speech is
		// missed, and half a transcript is harder to watch than none.
		logger("[CC] %s model %s needs a GPU and none is usable here, captions disabled for this tune", label, m.Key)
		return src
	}
	if !engineInstalled() {
		logger("[CC] %s the speech runtime is not downloaded, captions disabled for this tune", label)
		return src
	}
	engine, err := newCaptionEngine(cfg, m, label)
	if err != nil {
		logger("[CC] %s could not start captions: %v", label, err)
		return src
	}

	cs := &captionStream{src: src, engine: engine}
	cs.pr, cs.pw = io.Pipe()
	go cs.run()
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
		cs.engine.Close()
		_, err := io.Copy(cs.pw, cs.src)
		cs.pw.CloseWithError(err)
	}
}

// inject is the captioning path proper.
func (cs *captionStream) inject() {
	inj := newCaptionInjector(cs.pw, cs.engine.enc, cs.engine.label)
	buf := make([]byte, 64*1024)
	for {
		n, err := cs.src.Read(buf)
		if n > 0 {
			cs.engine.feed(buf[:n])
			if _, werr := inj.Write(buf[:n]); werr != nil {
				cs.pw.CloseWithError(werr)
				return
			}
		}
		if err != nil {
			inj.Flush()
			cs.pw.CloseWithError(err)
			return
		}
	}
}

func (cs *captionStream) Read(p []byte) (int, error) { return cs.pr.Read(p) }

func (cs *captionStream) Close() error {
	cs.once.Do(func() {
		cs.engine.Close()
		cs.pr.Close()
	})
	return cs.src.Close()
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

// captionStatus is what the Closed Captions page renders.
type captionStatus struct {
	Config    captionConfig        `json:"config"`
	Models    []captionStatusModel `json:"models"`
	Languages map[string]string    `json:"languageNames"`
	Download  captionDownload      `json:"download"`
	// The Runtime fields describe the engine the selected model needs, which is
	// the only one that has to be downloaded for that model to work.
	Runtime       string `json:"runtime"`
	RuntimeReady  bool   `json:"runtimeReady"`
	RuntimeSizeMB int    `json:"runtimeSizeMB"`
	RuntimeName   string `json:"runtimeName"`
	// Runtimes describes each engine, keyed by engine, for the same reason
	// Engines carries both: the page talks about the engine under the radio
	// button, which is not always the saved one.
	Runtimes map[string]string `json:"runtimes"`
	// RuntimeList is every engine, in order, so the page can show both as the
	// separate programs they are rather than swapping one card's contents and
	// leaving the reader to notice the name changed.
	RuntimeList    []speechRuntime  `json:"runtimeList"`
	Latencies      []captionLatency `json:"latencies"`
	RuntimeVersion string           `json:"runtimeVersion"`
	RuntimeURL     string           `json:"runtimeURL"`
	// Engines carries the builds of both engines, keyed by engine, so the page
	// can show what a model would need before it has been saved. Picking a
	// radio button is browsing, not a decision, and must not change what a tune
	// starting right now will do.
	Engines        map[string][]captionStatusEngine `json:"engines"`
	Drivers        []captionStatusDriver            `json:"drivers"`
	Accel          accelReport                      `json:"accel"`
	DriverInstall  gpuInstallState                  `json:"driverInstall"`
	Persistent     bool                             `json:"persistent"`
	PersistWarning string                           `json:"persistWarning"`
	Tuners         int                              `json:"tuners"`
	// MemoryWarning is spelled out when the current choice could use a lot of
	// memory, worked out for the tuners actually being captioned rather than
	// left as arithmetic for the reader.
	MemoryWarning string `json:"memoryWarning"`
}

type captionStatusDriver struct {
	gpuRuntime
	Downloaded bool `json:"downloaded"`
	Active     bool `json:"active"`
}

type captionStatusEngine struct {
	engineVariant
	Usable    bool   `json:"usable"`
	Installed bool   `json:"installed"`
	Selected  bool   `json:"selected"`
	URL       string `json:"url"`
	// File is the archive that will actually be fetched, and Shared says so
	// when another build on the list fetches the very same one. Two rows both
	// reading "28 MB" with no explanation is how someone ends up believing they
	// have to download the engine twice.
	File string `json:"file"`
	// PartOf names the engine and version this build belongs to, for the line
	// under the heading rather than in it.
	PartOf string `json:"partOf"`
	Shared string `json:"shared"`
}

type captionStatusModel struct {
	captionModel
	Installed bool `json:"installed"`
	// Engine is the engine this model runs on and EngineName the same thing
	// said in full, so the page can be honest that picking some models means a
	// second download.
	Engine      string `json:"engine"`
	EngineName  string `json:"engineName"`
	EngineReady bool   `json:"engineReady"`
	// Runnable is false when this machine cannot give the model what it needs.
	// Blocked says what is missing, in the words the page shows.
	// Recommended is worked out for this machine rather than fixed in the
	// catalog: what to use depends on whether there is a graphics card to use
	// it with, and a label that ignores that is advice for somebody else.
	Recommended bool   `json:"recommended"`
	Why         string `json:"why"`
	Runnable    bool   `json:"runnable"`
	Blocked     string `json:"blocked"`
	// Memory is what one simultaneous stream costs in RAM, and Reuse says what
	// happens to that copy when the stream ends.
	Memory string `json:"memory"`
	Reuse  string `json:"reuse"`
	// MemoryMB is one stream's cost and MemoryTotalMB the ceiling across the
	// tuners actually being captioned, worked out here so the page never asks
	// anyone to multiply anything.
	MemoryMB      int    `json:"memoryMB"`
	MemoryTotalMB int    `json:"memoryTotalMB"`
	MemoryTotal   string `json:"memoryTotal"`
	URL           string `json:"url"`
}

// memoryWarning says, in gigabytes, what the current settings could use, when
// that is enough to matter.
//
// Getting this wrong is expensive in a way the rest of the page is not: a
// machine that runs out of memory does not caption badly, it stops doing
// everything, and the number is not obvious from a model list because it
// multiplies by the tuners being captioned. So it is worked out here and said
// plainly rather than left as a sum for the reader.
func memoryWarning(cfg captionConfig) string {
	if !cfg.Enabled {
		return ""
	}
	m, ok := findCaptionModel(cfg.Model)
	if !ok {
		return ""
	}
	n := captionedStreams(cfg)
	per := streamMemoryMB(m)
	totalMB := per * n
	if runtimeOf(m) == rtTranscribe && !m.Streaming {
		// Shared: the total is one copy no matter how many tuners, and by the
		// same yardstick as everything else that rarely warrants a banner.
		totalMB = per
	}
	// One threshold for every model, judged on the total the current settings
	// would actually use. A shared model is judged on its one copy; a
	// per-stream model on all of its copies together — which is how a
	// middleweight on many tuners outranks a heavyweight that is shared, and
	// the warnings land where the memory actually goes.
	if totalMB < 4000 {
		return ""
	}
	if runtimeOf(m) == rtTranscribe && !m.Streaming {
		return fmt.Sprintf("%s keeps about %s in memory — one copy shared by every tuner, loaded when "+
			"the first stream needs it and freed when the last one ends — on top of everything else "+
			"this machine is doing.", m.Name, humanMB(totalMB))
	}
	each := humanMB(per)
	total := humanMB(totalMB)
	which := fmt.Sprintf("all %d tuners", n)
	if len(cfg.Tuners) > 0 {
		which = fmt.Sprintf("the %d tuners you have selected", n)
	}
	warn := fmt.Sprintf("%s uses about %s of memory per stream, and every stream captioned at "+
		"the same time loads its own copy. With captions on for %s that is up to %s of RAM at "+
		"once, on top of everything else this machine is doing. Copies are not shared, because "+
		"sharing one would make the streams wait for each other and fall behind the picture. "+
		"Memory is released as soon as a stream ends.",
		m.Name, each, which, total)

	warn += " If this is more than you have, caption fewer tuners below or pick a smaller model."
	return warn
}

// recommendedModel is the one to use on this machine.
//
// With a graphics card to run it on, the Unified is the pick: it is roughly
// twice as accurate as anything else that still transcribes as the audio
// arrives, and the extra second it spends looking ahead is the only thing it
// costs. Without one, that accuracy is not reachable at a sensible speed and
// the Nemotron is the better answer — it is quicker off the mark and it is the
// only recommendation that works in a language other than English.
func recommendedModel() (key, why string) {
	// One recommendation, the same on every machine: the model that keeps pace
	// with live speech on a processor or a GPU alike and handles every
	// supported language. The heavier models are for people who already know
	// they want them; a recommendation that changes with the hardware reads as
	// authority the page does not have.
	return "realtime-multilingual", "Recommended: keeps pace with live speech on a processor or a GPU alike, writes punctuation and sentence case, and handles every supported language."
}

// memoryNote describes what a model costs to run.
//
// No model shares one copy between two streams that are both transcribing. The
// engines decode one thing at a time per loaded copy, so sharing would make
// concurrent streams take turns, and a stream that waits its turn falls behind
// live audio and drops speech. Every simultaneous stream therefore loads its
// own copy, and every copy is freed the moment its stream ends.
func memoryNote(m captionModel, streams int) (memory, reuse, total string) {
	per := streamMemoryMB(m)
	if runtimeOf(m) == rtTranscribe && !m.Streaming {
		// One copy serves every tuner on this path; see txBatchService.
		memory = "one copy shared by every stream"
		total = "about " + humanMB(per) + " total, however many tuners caption"
		reuse = "Freed when the last stream ends."
		return memory, reuse, total
	}
	memory = humanMB(per) + " per stream"
	if streams > 1 {
		total = fmt.Sprintf("up to %s with %d tuners captioning at once", humanMB(per*streams), streams)
	} else {
		total = "about " + humanMB(per) + " with one tuner captioning"
	}
	reuse = "Each stream has its own copy, freed the moment it ends."
	return memory, reuse, total
}

// streamMemoryMB is what one captioned stream really costs in memory.
//
// It is not the size of the file. The weights are the bulk of it, but a stream
// also needs its decoder cache, the encoder's activations and ggml's own
// working buffers, and those scale with the model rather than being a fixed
// cost — a second stream of a 2.4 GB model was measured at about three
// gigabytes resident, not 2.4. Reporting the file size
// as the memory cost understates it by roughly a quarter, and understating
// memory is how a machine gets pushed into swap by a setting that looked safe.
//
// The allowance below is fitted to that measurement rather than derived, so it
// is an estimate and is worded as one wherever it is shown. It is deliberately
// not generous in the other direction: it is better to over-warn about memory
// than to have somebody discover the truth when the box stops responding.
const (
	// captionDecoderCtx bounds the decoder's key/value cache. Generous next to
	// a spoken phrase — a couple of hundred words — and a fraction of the
	// default sized for hours of audio.
	captionDecoderCtx = 512
	// streamOverhead covers buffers that grow with the model.
	streamOverhead = 1.2
	// streamWorkingMB covers the decoder cache and the fixed per-session cost.
	// A 0.6B run allocates tens of megabytes of key/value cache alone.
	streamWorkingMB = 100
)

func streamMemoryMB(m captionModel) int {
	sizeMB := m.SizeMB
	// The installed file is the truth when it is present.
	if st, err := os.Stat(modelPath(m)); err == nil && st.Size() > 0 {
		sizeMB = int(st.Size() / (1024 * 1024))
	}
	return int(float64(sizeMB)*streamOverhead) + streamWorkingMB
}

// humanMB writes a size the way a person would say it.
func humanMB(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// captionThreads divides the machine between the streams that may caption at
// once, rather than promising all of it to each of them.
//
// Left unset, the engine takes a sensible default for one session, which is
// most of the cores. That is right for one session and badly wrong for seven:
// seven sessions each starting twenty threads on a twenty thread machine is a
// hundred and forty threads competing for it, and the throughput lost to
// context switching swamps the work. Captions then fall behind on hardware that
// was never short of capacity — the machine was busy fighting itself.
//
// A share each, floored at one. The sum stays roughly the size of the machine
// however many tuners are captioned.
func captionThreads(cfg captionConfig) int {
	cpus := availableCPUs()
	streams := captionedStreams(cfg)
	// Rounded up, so the shares cover the machine rather than leaving cores
	// idle, and floored at two so a stream is never reduced to a single thread
	// on a machine with cores to spare. Seven tuners on a twenty thread
	// processor get three threads each: twenty-one in total, which is the size
	// of the machine, instead of the hundred and forty they were getting.
	per := (cpus + streams - 1) / streams
	if floor := 2; per < floor && cpus >= floor {
		per = floor
	}
	if per < 1 {
		per = 1
	}
	if per > cpus {
		per = cpus
	}
	return per
}

// availableCPUs is how much processor this container may actually use.
//
// runtime.NumCPU is not that number. It honours the affinity mask but knows
// nothing about a cgroup quota, so ah4c in Docker with --cpus=4 on a twenty
// thread host is told twenty, and would hand out threads on that basis while
// the kernel throttles it to four. The quota is where the real answer is, and
// it is worth reading rather than assuming: guessing high here is how a machine
// ends up thrashing, which is the fault this whole function exists to fix.
//
// Both cgroup layouts are checked. Anything unreadable or unlimited falls back
// to the affinity count, which is the best available answer on a bare host.
func availableCPUs() int {
	n := runtime.NumCPU()
	if q := cgroupCPUQuota(); q > 0 && q < n {
		return q
	}
	if n < 1 {
		return 1
	}
	return n
}

// cgroupCPUQuota reads the quota as a whole number of processors, or 0 when
// there is no limit.
func cgroupCPUQuota() int {
	// cgroup v2: "max 100000" or "400000 100000" in one file.
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(b))
		if len(f) == 2 && f[0] != "max" {
			quota, err1 := strconv.Atoi(f[0])
			period, err2 := strconv.Atoi(f[1])
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				return atLeastOne(quota / period)
			}
		}
		return 0
	}
	// cgroup v1: quota and period in separate files, -1 meaning unlimited.
	qb, err1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, err2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 != nil || err2 != nil {
		return 0
	}
	quota, err1 := strconv.Atoi(strings.TrimSpace(string(qb)))
	period, err2 := strconv.Atoi(strings.TrimSpace(string(pb)))
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return atLeastOne(quota / period)
}

func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// captionedStreams is how many tuners could be captioning at the same time.
func captionedStreams(cfg captionConfig) int {
	// Only tuners that exist. A selection left over from a larger setup would
	// otherwise inflate every memory figure and shrink every thread share for
	// streams that can never run.
	n := 0
	for _, t := range cfg.Tuners {
		if t >= 0 && t < len(tuners) {
			n++
		}
	}
	if n == 0 {
		n = len(tuners)
	}
	if n == 0 {
		n = 1
	}
	return n
}

func captionStatusPayload() captionStatus {
	cfg := currentCaptionConfig()
	cur := currentEngineVariant()
	hasGPU := gpuAvailable()
	pick, why := recommendedModel()
	streams := captionedStreams(cfg)
	models := make([]captionStatusModel, 0, len(captionModelCatalog))
	for _, m := range captionModelCatalog {
		rt := runtimeOf(m)
		mem, reuse, total := memoryNote(m, streams)
		perMB := streamMemoryMB(m)
		totalMB := perMB * streams
		if runtimeOf(m) == rtTranscribe && !m.Streaming {
			totalMB = perMB // one shared copy, whatever the tuner count
		}
		blocked := ""
		switch {
		case m.NeedsGPU && !hasGPU:
			blocked = "This model needs a GPU and no GPU build can run in this container yet. " +
				"On a processor it cannot keep pace with live audio: it falls further behind every " +
				"minute and drops most of what is said, so it is not offered rather than left to " +
				"disappoint after a multi-gigabyte download. The bar is low — integrated graphics " +
				"clear it comfortably — so set up Vulkan or CUDA above and this appears."
		case m.NeedsGPU && gpuVariant(rt) == "":
			// The card is there; the build that uses it is not. Saying so is
			// the difference between a two minute fix and a mystery.
			blocked = "This machine has a usable GPU, but the GPU build of " + findSpeechRuntime(rt).Name +
				" has not been downloaded yet. Download it above and this becomes available. It is " +
				"deliberately not offered on the processor build: this model cannot keep pace with " +
				"live audio there and would drop most of what is said."
		}
		models = append(models, captionStatusModel{
			captionModel:  m,
			Installed:     modelInstalled(m),
			Engine:        rt,
			EngineName:    findSpeechRuntime(rt).Name,
			EngineReady:   runtimeInstalled(rt, cur),
			Recommended:   m.Key == pick,
			Why:           map[bool]string{true: why}[m.Key == pick],
			Runnable:      blocked == "",
			Blocked:       blocked,
			Memory:        mem,
			Reuse:         reuse,
			MemoryMB:      perMB,
			MemoryTotalMB: totalMB,
			MemoryTotal:   total,
			URL:           modelURL(m),
		})
	}
	engineURL, _, _ := engineAsset()
	needed := findSpeechRuntime(neededRuntime())
	curVariant, _ := findEngineVariant(cur)
	persistent, dir := captionDirPersistent()
	persistWarning := ""
	if !persistent {
		persistWarning = fmt.Sprintf("%s is not a bind mount, so anything downloaded here is lost when the container is recreated. Add this to your compose file and recreate it:  - ${HOST_DIR}/ah4c/captions:/opt/captions", dir)
	}
	drivers := make([]captionStatusDriver, 0, len(gpuRuntimes))
	for _, g := range gpuRuntimes {
		drivers = append(drivers, captionStatusDriver{
			gpuRuntime: g,
			Downloaded: driverDownloaded(g),
			Active:     driverActive(g),
		})
	}
	// Both engines' builds are offered, so the page can say what a model would
	// cost before it is chosen rather than after.
	engines := make(map[string][]captionStatusEngine, len(speechRuntimes))
	for _, eng := range speechRuntimes {
		list := make([]captionStatusEngine, 0, len(engineVariants))
		for _, v := range engineVariants {
			if !runtimeVariantOffered(eng.Key, runtime.GOOS, runtime.GOARCH, v.Key) {
				continue
			}
			url, _, _, ok := runtimeAssetFor(eng.Key, runtime.GOOS, runtime.GOARCH, v.Key)
			if !ok {
				continue
			}
			v.SizeMB = runtimeSizeMB(eng.Key, v)
			// The heading stays the thing being chosen — where it runs. Which
			// engine that build belongs to is said underneath, because it
			// identifies the download without pretending to be a decision.
			list = append(list, captionStatusEngine{
				engineVariant: v,
				Usable:        engineUsable(v),
				Installed:     runtimeInstalled(eng.Key, v.Key),
				Selected:      v.Key == cur,
				URL:           url,
				File:          path.Base(url),
				PartOf:        eng.Name + " " + eng.Version,
			})
		}
		// Some builds are the same archive under two names, because an engine
		// can put more than one backend in one file. Say so on both rows: they
		// otherwise read as two separate downloads of identical size, and
		// downloading one silently marks the other installed, which looks like
		// a bug rather than a convenience.
		for i := range list {
			for j := range list {
				if i == j || list[i].URL != list[j].URL {
					continue
				}
				list[i].Shared = fmt.Sprintf(
					"Same file as %q — one download covers both, so you only need to fetch one of them.",
					list[j].Name)
				break
			}
		}
		engines[eng.Key] = list
	}
	state := "ready"
	switch {
	case engineLibPath() == "":
		state = fmt.Sprintf("no %s build is published for %s/%s", needed.Name, runtime.GOOS, runtime.GOARCH)
	case !engineInstalled():
		state = needed.Name + " has not been downloaded yet"
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		state = "ffmpeg was not found, so audio cannot be decoded"
	}
	return captionStatus{
		Config:         cfg,
		Models:         models,
		Languages:      captionLanguageNames,
		Download:       downloadStatus(),
		Runtime:        state,
		RuntimeReady:   engineInstalled(),
		RuntimeSizeMB:  runtimeSizeMB(needed.Key, curVariant),
		RuntimeName:    needed.Name,
		Runtimes:       runtimeDescriptions(),
		RuntimeList:    speechRuntimes,
		Latencies:      captionLatencies,
		RuntimeVersion: needed.Version,
		RuntimeURL:     engineURL,
		Persistent:     persistent,
		PersistWarning: persistWarning,
		MemoryWarning:  memoryWarning(cfg),
		Engines:        engines,
		Drivers:        drivers,
		Accel:          accelStatus(),
		DriverInstall:  gpuInstallStatus(),
		Tuners:         len(tuners),
	}
}
