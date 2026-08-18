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
	// NoLanguage marks a single-language model that wants no language
	// parameter: the engine treats none-given as that language, and passing
	// nothing is the documented path.
	NoLanguage bool `json:"-"`
	// NeedsGPU marks a model that is not usable without graphics acceleration.
	// It is not a preference: these are offered only where a GPU build can
	// actually run, because the alternative is a model that loads, falls
	// steadily behind live audio and drops most of what is said. Captions that
	// bad are worse than none, and finding out costs a multi-gigabyte download.
	NeedsGPU  bool     `json:"needsGPU"`
	Languages []string `json:"languages"`
}

// Quantized weights: on CPU they are what make these run faster than real time,
// and they keep the download manageable.
var captionModelCatalog = []captionModel{
	cohereTranscribe,
	nemotronStreaming,
	moonshineTiny,
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

	// Locales, for the families that will not take a bare code. Named with
	// the country only where more than one is offered, so a list of thirty
	// does not read as thirty variations on the same word.
	"en-US": "English", "en-GB": "English (UK)",
	"es-ES": "Spanish", "es-US": "Spanish (Americas)",
	"fr-FR": "French", "fr-CA": "French (Canada)",
	"pt-BR": "Portuguese (Brazil)", "pt-PT": "Portuguese",
	"de-DE": "German", "it-IT": "Italian", "nl-NL": "Dutch",
	"tr-TR": "Turkish", "ru-RU": "Russian", "ar-AR": "Arabic",
	"hi-IN": "Hindi", "ja-JP": "Japanese", "ko-KR": "Korean",
	"vi-VN": "Vietnamese", "uk-UA": "Ukrainian", "pl-PL": "Polish",
	"sv-SE": "Swedish", "cs-CZ": "Czech", "nb-NO": "Norwegian",
	"da-DK": "Danish", "bg-BG": "Bulgarian", "fi-FI": "Finnish",
	"hr-HR": "Croatian", "sk-SK": "Slovak", "zh-CN": "Chinese",
	"hu-HU": "Hungarian", "ro-RO": "Romanian", "et-EE": "Estonian",
}

// modelLanguage maps the configured language onto one this model accepts. The
// families disagree about spelling — one wants en, another insists on en-US and
// refuses anything shorter — and switching models should not leave a saved
// setting that quietly fails every stream. An exact match wins; a bare code
// widens to the model's first matching locale; anything else falls back to the
// model's first language rather than to an error.
func modelLanguage(m captionModel, lang string) string {
	if lang == "" || len(m.Languages) == 0 {
		return lang
	}
	for _, l := range m.Languages {
		if l == lang {
			return l
		}
	}
	for _, l := range m.Languages {
		if strings.HasPrefix(l, lang+"-") {
			return l
		}
	}
	first := m.Languages[0]
	if first == "auto" && len(m.Languages) > 1 {
		first = m.Languages[1]
	}
	logger("[CC] %s does not take language %q; using %s", m.Name, lang, first)
	return first
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
	// GPURuntime names driver packages to keep installed in the container, so a
	// GPU build of the engine has something to talk to.
	GPURuntime string `json:"gpuRuntime"`
	// OnScreenSec is the least time a line stays readable before it is allowed
	// to leave, in seconds. Zero means the guidance minimum for the row count.
	OnScreenSec float64 `json:"onScreenSec"`
	// SpeedWPM is how fast captions are let onto the screen, in words a minute.
	// Zero means the default. The guidance ranges from 120 to 160 and the page
	// offers exactly that.
	SpeedWPM int `json:"speedWPM"`
	// PhraseSec is how long a phrase model may listen before it cuts, chosen on
	// the page from what the model offers. Zero means the model's own figure.
	PhraseSec float64 `json:"phraseSec"`
	// Engine selects which build of the recognizer to run: the processor, or a
	// GPU through Vulkan or CUDA.
	Engine string `json:"engine"`
	// Tuners restricts captioning to specific tuner indexes. Empty means all.
	Tuners []int `json:"tuners"`
}

func defaultCaptionConfig() captionConfig {
	return captionConfig{
		Enabled:     false,
		Model:       "cohere-transcribe",
		Language:    "en",
		Style:       "rollup3",
		OnScreenSec: 4,
		SpeedWPM:    160,
		Uppercase:   true,
		Engine:      "auto",
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
	// A phrase length the chosen model does not offer is not saved. It comes
	// from a config written for a different model, and the model's own figure
	// is a better answer than the nearest number to somebody else's setting.
	if m, ok := findCaptionModel(cfg.Model); ok {
		q := quirksFor(m)
		keep := false
		for _, w := range q.Windows {
			if cfg.PhraseSec == w {
				keep = true
			}
		}
		if !keep {
			cfg.PhraseSec = 0
		}
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
	was := captionCfg.Enabled
	captionCfg = cfg
	captionCfgLock.Unlock()
	refreshCaptionReady()
	// Switching captions on is what asks for the graphics driver, because
	// startup no longer installs one for a container that was not captioning.
	// It runs behind the same gates as any other install now that the server is
	// up, and it is a no-op if startup already did it.
	if cfg.Enabled && !was && runtime.GOOS == "linux" {
		go restoreSavedDriver()
	}
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
	return fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", m.Repo, m.File)
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
		// No gate at the door, deliberately.
		//
		// This download already yields all the way through: streamToFile stops
		// between reads for as long as any tune is in flight, a quarter second
		// at a time. Work that yields throughout does not need permission to
		// begin, because it is never the thing holding the disk when a tune
		// turns up.
		//
		// A gate here was worse than nothing. Ten seconds of proven quiet is
		// not a thing a three-tuner machine reliably produces, so the press
		// sat in a loop that said nothing while it waited — which from the
		// page is indistinguishable from a button that does not work.
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
		refreshCaptionReady()
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
	paused := false
	for {
		// Yield to tunes for the whole download, not merely at the start of it.
		//
		// The caller waits for a quiet moment before beginning, which was the
		// entire protection, and then this ran for minutes — half a gigabyte
		// for a streaming model, a gigabyte and a half for the phrase one —
		// over the same network and onto the same disk the tuners are using.
		// A tune that started thirty seconds in was competing with a transfer
		// that had no idea it existed, and lost: forty seconds without
		// confirming playback, which is longer than the DVR waits.
		//
		// So the transfer stops while a tune is in flight and resumes when it
		// settles. Tunes take seconds and the connection tolerates a pause of
		// that order; if it does not, the download fails and is retried, which
		// is a button rather than a recording.
		for tunesPending() {
			if !paused {
				paused = true
				logger("[CC] Pausing the download for a tune in progress")
			}
			time.Sleep(250 * time.Millisecond)
		}
		paused = false
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
		// Same rule as the model download: never fight a recording.
		// No gate here either, and for the same reason as the model download:
		// countingReader stops between reads while a tune is in flight, so the
		// transfer and the decompression behind it both stand aside on their
		// own. Gating it only added a silent wait in front of work that was
		// already polite.
		//
		// The engine is a library plus the ggml backends it loads from
		// alongside itself, so the whole archive is taken.
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
		refreshCaptionReady()
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
	r      io.Reader
	done   int64
	paused bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	// The engine archive is smaller than a model but the reasoning is the
	// same, and it is decompressed on the way in, so it costs processor as
	// well as network. It waits for a tune exactly as the model download does.
	for tunesPending() {
		if !c.paused {
			c.paused = true
			logger("[CC] Pausing the download for a tune in progress")
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.paused = false
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
	refreshCaptionReady()
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

// driverDownloaded reports whether the saved set in the bind mount is
// complete: every package the runtime names has its package file present. A
// partial set — a loader without the drivers behind it, say — installs
// something that looks alive and works as nothing, so partial does not count.
func driverDownloaded(g gpuRuntime) bool {
	ents, err := os.ReadDir(driverDir(g))
	if err != nil {
		return false
	}
	for _, pkg := range g.Packages {
		found := false
		for _, e := range ents {
			if !e.IsDir() && strings.HasPrefix(e.Name(), pkg+"_") && strings.HasSuffix(e.Name(), ".deb") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// driverActive reports whether the driver is loadable right now.
func driverActive(g gpuRuntime) bool {
	// Through the same cache the engine probe uses, because it is the same
	// question about the same library. This asked it afresh every time, and the
	// page asks on every poll — a second and a half apart, for as long as
	// anybody has it open — so the loader's global lock was being taken all day
	// for an answer that had already been worked out. The engine's own calls
	// into native code contend for that lock, which is the reason the cache was
	// written in the first place; this simply was not using it.
	return engineUsable(engineVariant{Needs: g.Needs})
}

type gpuInstallState struct {
	Kind     string `json:"kind"`
	Active   bool   `json:"active"`
	Finished bool   `json:"finished"`
	Err      string `json:"err"`
	Log      string `json:"log"`
	// Step names what is happening and Done of Total how far through it is, so
	// the page can show a bar rather than the word "Installing" for a minute
	// and a half while somebody wonders whether it has hung.
	Step  string `json:"step"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// driverUrgent is set while somebody is waiting at the page for this.
//
// The per-package quiet wait exists for work nobody asked for — the restore at
// startup, which is free anyway because it runs before the port is bound. On a
// button press it is the wrong trade entirely: up to fifteen seconds of waiting
// in front of each of thirty-seven packages is nine minutes of somebody staring
// at a page, to protect tunes that the person pressing the button knows about.
//
// So an explicit press goes straight through. It is still one dpkg at a time
// and still behind nice and ionice, so a tune that does arrive loses only the
// package in flight rather than the whole set.
var driverUrgent atomic.Bool

// noteDriverStep publishes progress for the page. Called from the download and
// the install, which are the two parts long enough to need one.
func noteDriverStep(step string, done, total int) {
	gpuLock.Lock()
	gpuState.Step, gpuState.Done, gpuState.Total = step, done, total
	gpuLock.Unlock()
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
		// Somebody is at the page waiting for this, so it goes now.
		//
		// There was a wait for a quiet minute in front of it and a wait for a
		// quiet moment in front of each of thirty-seven packages behind it,
		// which on a machine with tuners running is minutes of a progress bar
		// not moving. Those gates are for the restore at startup, which nobody
		// asked for and which is free anyway because it runs before the port is
		// bound. A button press is somebody who knows what they are doing and
		// what it costs.
		//
		// It is still one dpkg at a time and still behind nice and ionice, so a
		// tune arriving in the middle loses the package in flight rather than
		// the whole set.
		driverUrgent.Store(true)
		defer driverUrgent.Store(false)
		log, err := fetchDriver(g)
		if err == nil {
			var l2 string
			l2, err = applyDriver(g)
			log += l2
		}
		gpuLock.Lock()
		gpuState.Active = false
		gpuState.Finished = true
		gpuState.Step, gpuState.Done, gpuState.Total = "", 0, 0
		gpuState.Log = tailLines(log, 12)
		if err != nil {
			gpuState.Err = err.Error()
			logger("[CC] %s could not be set up: %v", g.Name, err)
		} else {
			logger("[CC] %s is ready", g.Name)
			// Whether a GPU build can load is cached, and installing a
			// driver is the one moment that answer changes.
			forgetEngineUsable()
			forgetBrokenDrivers()
			refreshGPUReady()
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var log strings.Builder
	// Polite, like everything else here. The two calls that go through this —
	// refreshing the package lists and reading the dependency tree — are the
	// part of the driver fetch that is neither gated nor divided: they run
	// straight after the gate whatever the gate answered, and one of them
	// rewrites every package list in the container. Losing to a tuner is the
	// least they can do.
	run := func(args ...string) error {
		logger("[CC] %v", args)
		cmd := politeCommand(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		log.Write(b)
		return e
	}
	// apt-get download writes into the working directory and has no option
	// that moves it, so the working directory is where the staging set is.
	// Not logged per call the way run is: this one is invoked once per package
	// in the closure, and forty lines of apt command line buries the two lines
	// that say what actually happened.
	runIn := func(dir string, args ...string) error {
		cmd := politeCommand(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		log.Write(b)
		return e
	}
	// The download lands in a staging directory and replaces the saved set
	// only once it is complete. Clearing first and downloading after was
	// tried, and a fetch that failed partway left a set with the loader and
	// no drivers behind it — which installs cleanly, loads cleanly, and
	// captions nothing. A failed fetch now changes nothing.
	staging := filepath.Join(dir, "incoming")
	os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Join(staging, "partial"), 0o755); err != nil {
		return "", err
	}
	stagingAbs, err := filepath.Abs(staging)
	if err != nil {
		return "", err
	}
	// The stable suite of the base image freezes its graphics drivers for
	// years — the driver it offers today shipped before this GPU's compute
	// paths were optimized, and a modern engine on a museum driver runs at a
	// fraction of the hardware's speed while looking perfectly healthy. The
	// distribution's backports suite carries the current driver for exactly
	// this reason, and it is the only place this fetches from.
	//
	// There is no fallback to the base image's own driver, deliberately. A
	// 2022 driver installs, loads and captions, and does it several times
	// slower on the same chip while every check passes — which is a worse
	// outcome than no driver and a sentence saying why. If the live suite
	// cannot be reached the second attempt is the archive of the same suite,
	// which can only ever hold the same era of driver.
	suite := backportsSuite()
	if suite == "" {
		return log.String(), fmt.Errorf("this image's distribution could not be identified, so the current graphics driver cannot be located")
	}
	// What has to be saved is everything a *fresh* container will need, and
	// that is not what apt would download here.
	//
	// "apt-get install --download-only" fetches what has to change in this
	// container. Run it once in a container where the driver already works and
	// it fetches the two named packages and nothing else, because everything
	// underneath them is already installed — and that two-package set is what
	// then replaced a complete one and got saved as the thing to restore. The
	// next rebuild installed a driver on top of an empty space where a dozen
	// libraries used to be, and the loader skipped every one of them.
	//
	// Worse, that state is self-sealing. The driver goes in with dpkg and
	// --force-depends, so the package sits there installed with its
	// dependencies unmet, and every apt run afterwards inherits the mess:
	// asked to fetch the package again it tries to satisfy the version already
	// installed, cannot, and exits without downloading anything. The only way
	// out was to delete the directory by hand.
	//
	// So the set is worked out from the packaging rather than from this
	// container's state: the full recursive dependency closure, fetched
	// one-by-one with a command that has no solver in it to be confused. It
	// downloads a package because it is in the closure, not because this
	// container happens to lack it, which makes the saved set the same whether
	// it is built on a clean container or a broken one.
	var (
		closure []string
		aptErr  error
	)
	for i, src := range backportsSources(suite) {
		if i > 0 {
			logger("[CC] The driver could not be fetched from %s (%v); trying %s", backportsSources(suite)[i-1].name, aptErr, src.name)
			os.RemoveAll(staging)
			if err := os.MkdirAll(filepath.Join(staging, "partial"), 0o755); err != nil {
				return log.String(), err
			}
		}
		if err := writeAptSource(src, &log); err != nil {
			aptErr = err
			continue
		}
		if err := run(append([]string{"apt-get", "update"}, src.opts...)...); err != nil {
			aptErr = fmt.Errorf("apt-get update: %w", err)
			continue
		}
		c, cerr := driverClosure(g.Packages, suite, src.opts, &log)
		if len(c) == 0 {
			aptErr = fmt.Errorf("could not work out what %s depends on: %w", strings.Join(g.Packages, " "), cerr)
			continue
		}
		logger("[CC] %s needs %d packages in all; fetching them from %s", g.Name, len(c), src.name)
		closure = c
		// A package at a time, for the same reason the install goes a package
		// at a time. The whole closure on one command line is tens of megabytes
		// over the network and onto the disk inside a single call that cannot
		// be paused, cannot be divided once it has started, and takes about as
		// long as a DVR is willing to wait for a tune. Split up, the longest it
		// can be in anybody's way is one package, and it stands aside between
		// them.
		warned := false
		noteDriverStep("Fetching packages", 0, len(c))
		for i, p := range c {
			if !driverUrgent.Load() && !waitTuneQuietHeld(5*time.Second, 15*time.Second) && !warned {
				warned = true
				logger("[CC] %s is being fetched through a busy machine, one package at a time", g.Name)
			}
			if aptErr = runIn(stagingAbs, downloadArgs(suite, src.opts, []string{p})...); aptErr != nil {
				aptErr = fmt.Errorf("fetching %s: %w", p, aptErr)
				break
			}
			noteDriverStep("Fetching packages", i+1, len(c))
			if i == len(c)-1 || (i+1)%10 == 0 {
				logger("[CC] %s: %d of %d packages fetched", g.Name, i+1, len(c))
			}
		}
		if aptErr == nil {
			break
		}
	}
	if len(closure) == 0 {
		os.RemoveAll(staging)
		return log.String(), fmt.Errorf("the current graphics driver could not be fetched: %w", aptErr)
	}
	// Completeness is judged before anything is replaced, and against the
	// whole closure rather than the two packages that were asked for by name.
	// Checking only those is what let a set with no libraries under it pass.
	var missing []string
	have := map[string]bool{}
	if ents, e := os.ReadDir(staging); e == nil {
		for _, f := range ents {
			if n, _, ok := strings.Cut(f.Name(), "_"); ok && strings.HasSuffix(f.Name(), ".deb") {
				have[n] = true
			}
		}
	}
	for _, pkg := range closure {
		if !have[pkg] {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		os.RemoveAll(staging)
		if aptErr != nil {
			return log.String(), fmt.Errorf("downloading %s: %w — the previously saved packages were left untouched", strings.Join(g.Packages, " "), aptErr)
		}
		return log.String(), fmt.Errorf("the download finished without %d of the %d packages needed (%s); the previously saved packages were left untouched",
			len(missing), len(closure), strings.Join(missing, " "))
	}
	if old, _ := savedDebs(g); len(old) > 0 {
		for _, p := range old {
			os.Remove(p)
		}
	}
	if ents, e := os.ReadDir(staging); e == nil {
		for _, f := range ents {
			if strings.HasSuffix(f.Name(), ".deb") {
				os.Rename(filepath.Join(staging, f.Name()), filepath.Join(dir, f.Name()))
			}
		}
	}
	os.RemoveAll(staging)

	// Whether or not apt honored the archive directory, the packages have to
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
	names := make([]string, len(n))
	for i, p := range n {
		names[i] = filepath.Base(p)
	}
	// The filenames carry the versions, and the versions are the whole story:
	// a driver from the wrong era looks identical in every way but speed.
	logger("[CC] Saved %d packages for %s in %s: %s", len(n), g.Name, dir, strings.Join(names, " "))
	return log.String(), nil
}

// backportsSuite names the backports suite for whatever distribution this
// image is built on, or "" if that cannot be worked out. Backports is where
// the distribution keeps current graphics drivers for a stable release.
func backportsSuite() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	codename := ""
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VERSION_CODENAME="); ok {
			codename = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	if codename == "" {
		return ""
	}
	return codename + "-backports"
}

// backportsSnapshot is the date the archive is read at when the live suite
// cannot be reached. Any timestamp is accepted and resolves to the archive as
// it stood then, so this only has to be a date the driver was known good.
const backportsSnapshot = "20260601T000000Z"

// aptSource is one place to fetch from: the line that names it, a name for the
// log, and any options apt needs to read it.
type aptSource struct {
	name string
	line string
	opts []string
}

// backportsSources is where the driver may be fetched from, in order.
//
// Both are the same suite of the same distribution, and that is the point.
// The second is the archive of the first, read at a fixed date, for the day
// the live index has moved on or the mirror will not answer. Neither can hand
// back the base image's own driver, which is the one outcome worth refusing:
// it installs, it loads, it captions, and it does all of it several times
// slower than the driver being asked for while every check passes.
func backportsSources(suite string) []aptSource {
	return []aptSource{
		{
			name: suite,
			line: fmt.Sprintf("deb http://deb.debian.org/debian %s main\n", suite),
		},
		{
			name: "the " + suite + " archive at " + backportsSnapshot,
			line: fmt.Sprintf("deb https://snapshot.debian.org/archive/debian/%s/ %s main\n", backportsSnapshot, suite),
			// The archive serves the Release file exactly as it was, so its
			// valid-until date is in the past by design. Refusing it on those
			// grounds would refuse the archive entirely.
			opts: []string{"-o", "Acquire::Check-Valid-Until=false"},
		},
	}
}

// writeAptSource puts one source in place, replacing whatever the last attempt
// left behind. One file, rewritten, so a fallback never ends up listed beside
// the source it was falling back from.
func writeAptSource(s aptSource, log *strings.Builder) error {
	if err := os.WriteFile("/etc/apt/sources.list.d/backports.list", []byte(s.line), 0o644); err != nil {
		fmt.Fprintf(log, "could not add %s: %v\n", s.name, err)
		return fmt.Errorf("could not add the %s package source: %w", s.name, err)
	}
	logger("[CC] Using %s for a current graphics driver", s.name)
	return nil
}

// downloadArgs fetches packages by name and nothing else. apt-get download
// takes the candidate version of each package it is given and writes the file
// out; it does not consult what is installed, plan an installation, or have an
// opinion about dependencies, which is exactly why it is used here. The set to
// fetch has already been decided by driverClosure.
func downloadArgs(suite string, opts, pkgs []string) []string {
	args := append([]string{"apt-get", "download"}, opts...)
	if suite != "" {
		// One knob, applied to the closure and the fetch alike, so both answer
		// about the same versions of the same packages.
		args = append(args, "-o", "APT::Default-Release="+suite)
	}
	return append(args, pkgs...)
}

// driverClosure is every package the named ones need, worked out from the
// packaging rather than from what this container has installed.
//
// The base system is left out: anything the distribution marks essential, or
// required, or important, is in every Debian image there is and will be in the
// rebuilt container too. Saving a copy of the C library to restore over the top
// of itself is at best wasted and at worst the way a container breaks.
// Everything below that line is fair game, because a slim image carries none of
// it and a driver needs a dozen of them.
func driverClosure(pkgs []string, suite string, opts []string, log *strings.Builder) ([]string, error) {
	args := append([]string{"apt-cache"}, opts...)
	if suite != "" {
		args = append(args, "-o", "APT::Default-Release="+suite)
	}
	args = append(args, "depends", "--recurse", "--no-recommends", "--no-suggests",
		"--no-conflicts", "--no-breaks", "--no-replaces", "--no-enhances")
	args = append(args, pkgs...)
	out, err := politeCommand(args[0], args[1:]...).CombinedOutput()
	log.Write(out)
	if err != nil {
		return nil, err
	}
	// Package names sit at the left margin. Everything indented is a
	// relationship, and a name in angle brackets is a virtual package, which
	// nothing can download — the real package providing it is listed under it.
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '|' || line[0] == '<' {
			continue
		}
		name := strings.TrimSpace(line)
		// An architecture qualifier is how apt disambiguates a name, not part
		// of it; the file it downloads is named without one, and the check that
		// every package arrived compares the two.
		if n, _, ok := strings.Cut(name, ":"); ok {
			name = n
		}
		if name == "" || strings.ContainsAny(name, " \t") || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("apt-cache named no packages")
	}

	keep := map[string]bool{}
	for _, p := range pkgs {
		// The packages actually asked for stay in whatever their priority.
		keep[p] = true
	}
	shown := append([]string{"apt-cache"}, opts...)
	if suite != "" {
		shown = append(shown, "-o", "APT::Default-Release="+suite)
	}
	shown = append(shown, "show", "--no-all-versions")
	shown = append(shown, names...)
	info, err := exec.Command(shown[0], shown[1:]...).Output()
	if err != nil {
		// Without priorities there is no safe way to leave anything out, and
		// too many packages restores correctly while too few does not.
		logger("[CC] Could not read the package priorities; saving the whole dependency set")
		return names, nil
	}
	base := map[string]bool{}
	pkg, prio, essential := "", "", false
	flush := func() {
		if pkg != "" && (essential || prio == "required" || prio == "important") {
			base[pkg] = true
		}
		pkg, prio, essential = "", "", false
	}
	for _, line := range strings.Split(string(info), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "Package: "):
			flush()
			pkg = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case strings.HasPrefix(line, "Priority: "):
			prio = strings.TrimSpace(strings.TrimPrefix(line, "Priority: "))
		case strings.HasPrefix(line, "Essential: "):
			essential = strings.TrimSpace(strings.TrimPrefix(line, "Essential: ")) == "yes"
		}
	}
	flush()

	var out2 []string
	for _, n := range names {
		if base[n] && !keep[n] {
			continue
		}
		out2 = append(out2, n)
	}
	return out2, nil
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
	logger("[CC] Installing %d saved packages for %s, one at a time with a look for a quiet moment between each", len(debs), g.Name)
	// Whatever was true about these libraries stops being true here.
	forgetEngineUsable()
	forgetBrokenDrivers()
	defer refreshGPUReady()
	// --force-unsafe-io because the fsync per file is the entire weight of
	// this. dpkg syncs to be certain a package survives a power cut mid
	// install; these packages exist in the bind mount either way, and a
	// container that loses power reinstalls them from there on the way back
	// up. What it buys is nothing, and what it costs is an array full of
	// synchronous writes beside a tuner trying to prove it is playing.
	// One package at a time, with the gate re-checked between each.
	//
	// A gate can only promise the moment it is asked. Ten seconds of proven
	// quiet said nothing about the thirty that followed, and a DVR starting
	// several recordings at once is exactly the thing that arrives in the
	// middle of them — which is what happened: the window was found, the
	// install began, the tunes started a second later and one of them died.
	//
	// The rule already written down for this is that long work yields
	// throughout rather than at the door. dpkg cannot be paused, so this was
	// treated as work that could only be gated and not yielded. That was
	// wrong: it cannot be paused, but it can be *divided*. Thirty-seven
	// packages is thirty-seven jobs of a second or two, and between any two of
	// them the machine is free. The exposure drops from the length of the
	// whole install to the length of one package.
	//
	// Order does not matter because --force-depends installs regardless of what
	// is not there yet; anything left unconfigured is configured by a later
	// package or caught by the verification below.
	// A package is never skipped. The first version of this loop meant to wait
	// and try the same one again, and wrote "i--; continue" inside a range —
	// where the next iteration assigns i from the range anyway, so the
	// decrement did nothing and the package was dropped instead. On a busy
	// machine that dropped most of them, and a driver missing most of its
	// libraries installs cleanly, loads cleanly and offers no device. That is
	// the shape of tonight's "the driver isn't loading": not a failure, a
	// silent partial success.
	//
	// So quiet is preferred and not required. One package is a second or two of
	// polite work, which is a smaller thing to risk against a tune than an
	// incomplete driver is against every tune afterwards.
	var out []byte
	var failed []string
	in := 0
	err = nil
	warned := false
	noteDriverStep("Installing packages", 0, len(debs))
	for i, deb := range debs {
		if !driverUrgent.Load() && !waitTuneQuietHeld(5*time.Second, 15*time.Second) && !warned {
			warned = true
			logger("[CC] %s is going in through a busy machine, one package at a time and yielding between them. Nothing is being skipped.", g.Name)
		}
		cmd := politeCommand("dpkg", "-i", "--force-depends", "--force-unsafe-io", deb)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		out = append(out, b...)
		if e != nil {
			err = e
			failed = append(failed, filepath.Base(deb))
		} else {
			in++
		}
		noteDriverStep("Installing packages", in, len(debs))
		if i == len(debs)-1 || (i+1)%10 == 0 {
			// Packages that went in, not loops that were run. This counted the
			// index, so it read "37 of 37 packages in" whatever dpkg had made
			// of them — a line that says the same thing on success and on total
			// failure is worse than no line, because it is believed.
			logger("[CC] %s: %d of %d packages in", g.Name, in, len(debs))
		}
	}
	if len(failed) > 0 {
		logger("[CC] %s: %d packages would not install: %s", g.Name, len(failed), strings.Join(failed, " "))
	}
	// Ask the loader again rather than reading back what was true before.
	//
	// driverActive goes through the same answer-store the engine probe uses,
	// and that store was emptied before this install and not after it. The
	// install takes the better part of a minute, and the Closed Captions page
	// polls every second and a half — so a poll during the install asked
	// whether libvulkan.so.1 loads, was told no because it genuinely did not
	// yet, and stored it. The check below then read that back and reported a
	// driver that had installed perfectly as one that will not load.
	//
	// Which is why it was reported by somebody watching the page and not by
	// somebody who pressed the button and walked away.
	forgetEngineUsable()
	if err != nil && !driverActive(g) {
		return string(out), fmt.Errorf("installing the saved packages: %w", err)
	}
	if !driverActive(g) {
		return string(out), fmt.Errorf("%s still will not load", g.Needs)
	}
	// The loader loading proves nothing about the drivers behind it: a
	// forced install can put a current driver on top of last-era libraries,
	// and the driver then fails to load while everything looks installed —
	// the loader just skips it and reports no devices. So each hardware
	// driver is opened the way the loader would open it, failures are named
	// with the loader's own words, and if the network is still here apt is
	// asked to complete what the forced install skipped.
	if bad := brokenVulkanDrivers(); len(bad) > 0 {
		// Asking apt to finish the job was tried here and it cannot: a forced
		// install leaves the package present with its dependencies unmet, and
		// the only repair apt will consider from there is removing it. It said
		// so in as many words — "The following packages will be REMOVED" —
		// and then refused, because removal is forbidden. Two dead ends, one
		// after the other, printed into the log every restart.
		//
		// The honest answer is that the saved set is wrong and no amount of
		// work in this container will make it right. That is a download, and
		// the page is where the button is.
		for _, b := range bad {
			logger("[CC] Vulkan driver %s", b)
		}
		setDriverFault("The saved driver packages are missing libraries they depend on, so the loader skips every driver and offers no device. Press the driver download below to fetch a complete set; the log names each missing piece.")
		return string(out), fmt.Errorf("%d graphics drivers installed but cannot load; the log names the missing pieces", len(bad))
	}
	// Zero broken drivers must mean drivers exist: an empty manifest
	// directory verifies vacuously and captions nothing. This is the state a
	// partial download installs into, and it is a failure with a next step,
	// not a success.
	if g.Key == "vulkan" && !anyVulkanManifests() {
		setDriverFault("The driver packages installed but left no driver behind them, so there is nothing for the loader to offer. Press the driver download below to fetch a complete set.")
		return string(out), fmt.Errorf("the packages installed but no Vulkan driver manifests exist; the saved set is incomplete — press the driver download to fetch it fresh")
	}
	setDriverFault("")
	return string(out), nil
}

// politeCommand builds a command that loses every contest with a tune.
//
// The quiet gate decides when heavy work may start; this decides what happens
// when it turns out to have been wrong. A package install cannot be paused
// partway — stopping dpkg between unpacking and configuring leaves a container
// worse than either — so the one thing left is to make sure that whatever it is
// doing, the tuners get the processor and the disk first. Nice and ionice are
// in the base image; if they ever are not, the command runs exactly as it did
// before.
func politeCommand(name string, args ...string) *exec.Cmd {
	pre := []string{}
	if p, err := exec.LookPath("ionice"); err == nil {
		pre = append(pre, p, "-c3")
	}
	if p, err := exec.LookPath("nice"); err == nil {
		pre = append(pre, p, "-n", "19")
	}
	if len(pre) == 0 {
		return exec.Command(name, args...)
	}
	full := append(append(pre, name), args...)
	return exec.Command(full[0], full[1:]...)
}

// driverFault is why the graphics driver cannot be used, in the words the page
// shows, or empty when there is nothing wrong with it.
//
// It is recorded where the answer is found rather than asked for when needed.
// Finding it means opening every driver library the manifests name, and the
// page asks on every poll; the install is the one moment the answer can change,
// so that is the moment it is written down.
var (
	driverFaultLock sync.Mutex
	driverFaultWhy  string
)

func setDriverFault(why string) {
	driverFaultLock.Lock()
	driverFaultWhy = why
	driverFaultLock.Unlock()
}

func driverFaultNow() string {
	driverFaultLock.Lock()
	defer driverFaultLock.Unlock()
	return driverFaultWhy
}

// anyVulkanManifests reports whether any driver manifest exists at all,
// hardware or otherwise.
func anyVulkanManifests() bool {
	for _, dir := range []string{
		"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d",
		"/usr/local/share/vulkan/icd.d", "/usr/local/etc/vulkan/icd.d",
	} {
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					return true
				}
			}
		}
	}
	return false
}

// brokenVulkanDrivers opens every hardware driver named by the Vulkan
// manifests and reports the ones that fail, each with the loader's verbatim
// error — which names the missing library, and the missing library names the
// stale dependency.
// The result is remembered until something happens that could change it, which
// is a driver being installed and nothing else. Working it out means opening
// every driver the manifests name, and the page asks for it on every poll; the
// answer only moves when dpkg has just rewritten the libraries underneath it.
var (
	brokenLock  sync.Mutex
	brokenKnown []string
	brokenSeen  bool
)

// forgetBrokenDrivers is called wherever the drivers themselves change.
func forgetBrokenDrivers() {
	brokenLock.Lock()
	brokenSeen = false
	brokenLock.Unlock()
}

func brokenVulkanDrivers() []string {
	brokenLock.Lock()
	if brokenSeen {
		out := brokenKnown
		brokenLock.Unlock()
		return out
	}
	brokenLock.Unlock()

	bad := scanBrokenVulkanDrivers()

	brokenLock.Lock()
	brokenKnown, brokenSeen = bad, true
	brokenLock.Unlock()
	return bad
}

func scanBrokenVulkanDrivers() []string {
	var bad []string
	for _, dir := range []string{
		"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d",
		"/usr/local/share/vulkan/icd.d", "/usr/local/etc/vulkan/icd.d",
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			l := strings.ToLower(e.Name())
			if strings.Contains(l, "lvp") || strings.Contains(l, "llvmpipe") || strings.Contains(l, "swiftshader") ||
				strings.Contains(l, "gfxstream") || strings.Contains(l, "virtio") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var manifest struct {
				ICD struct {
					LibraryPath string `json:"library_path"`
				} `json:"ICD"`
			}
			if json.Unmarshal(b, &manifest) != nil || manifest.ICD.LibraryPath == "" {
				continue
			}
			lib := manifest.ICD.LibraryPath
			if !filepath.IsAbs(lib) && strings.Contains(lib, "/") {
				lib = filepath.Join(dir, lib)
			}
			h, err := purego.Dlopen(lib, purego.RTLD_NOW)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: %s cannot load: %v", e.Name(), lib, err))
				continue
			}
			_ = h
		}
	}
	return bad
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
	case driverFaultNow() != "":
		// The loader loading says nothing about the drivers behind it. This is
		// the state a half-complete package set installs into: everything above
		// passes, the loader offers no devices, and without this the page went
		// green while every stream ran on the processor.
		r.Headline = "Not accelerated: the graphics driver cannot load"
		r.Detail = driverFaultNow()
	case txStarted() && !txBackendAvailable(txBackend(v.Key)):
		r.Headline = "Not accelerated: the engine started before " + v.Name + " was ready"
		r.Detail = "Everything it needs is here now, but the engine looks for its backends once, when it first loads, and that had already happened. Restart the container to caption on the GPU."
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
// gpuReady is gpuAvailable's answer, kept where the tune path can read it
// without asking.
//
// gpuAvailable calls engineUsable, and engineUsable dlopens the library when its
// cache is cold: libvulkan and the whole Mesa chain, every symbol resolved
// eagerly, off a disk that may be cold. That is seconds, and maybeWrapCaptions
// was calling it on the tune path while the global tuner lock was held — so the
// tune, and every tune queued behind it, waited for a driver probe.
//
// The cache made this rare rather than impossible, and installing a driver
// empties it on purpose, because the answer really does change when one goes in.
// So the first captioned tune after any install paid the full cost: on a
// container start that is the first tune of the night, which is exactly where
// playback confirmation was timing out, and only with captions on, because
// nothing else on that path asks this question.
//
// It is a stored answer now, refreshed off the tune path wherever the cache is
// emptied. False until something has looked, which costs nothing: the warm-up
// runs before the port is bound, so it has always looked by the time a tune can
// arrive.
var gpuReady atomic.Bool

// refreshGPUReady re-answers the question. Never call it from the tune path: it
// is the thing that dlopens.
func refreshGPUReady() {
	gpuReady.Store(gpuAvailable())
	refreshCaptionReady()
}

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

// driverRestoreDone closes when the startup driver restore has finished (or
// found nothing to do). The engine's first initialization waits on it, so a
// fresh container does not open the Vulkan library half-installed and settle
// on the processor for the life of the process.
var driverRestoreDone = make(chan struct{})

// serving reports whether ah4c has opened its door yet.
//
// This is the difference between quiet that is observed and quiet that is
// guaranteed, and every gate in this file has been arguing about the former for
// want of the latter. Startup is the one stretch in a container's life where a
// tune cannot arrive: the port is not bound, so the DVR gets connection refused
// rather than a request nobody answers. Not "the machine looks quiet" — the
// door is shut.
//
// So while this is false the gates stop guessing and say yes. Nothing they are
// protecting against can happen yet.
var serving atomic.Bool

// driverRestoreBudget is how long startup will wait for the driver before
// coming up without it.
//
// It has to be finite. A container that cannot install its driver must still
// answer the DVR — ah4c that never listens is worse than ah4c with no graphics
// acceleration, by a distance. Restoring an already-downloaded set is
// thirty-odd packages of a second or two, so this is roughly double what it
// takes and nowhere near what it costs to be wrong.
const driverRestoreBudget = 2 * time.Minute

// restoreGPURuntime puts the graphics driver back, and does it before ah4c
// starts serving.
//
// This used to return immediately and install in the background behind the tune
// gate, on the reasoning that a container which has just restarted faces every
// recording the DVR wants back at once and the server should come up first.
// Both halves of that were true and the conclusion was still wrong, because it
// put the one piece of un-pausable work in this program into a permanent race
// with the tunes it must not interrupt — and the gate that was supposed to
// referee the race could only ever report on the instant it was asked.
//
// Running it here removes the race rather than refereeing it. main calls this
// before it binds the port, so for as long as this takes there is no tune to
// interrupt and no gate to satisfy: dpkg can fsync its way through the whole
// set at full speed and the worst it can do is delay the door opening.
//
// Bounded, because it is now in front of everything. If the install has not
// finished inside the budget, ah4c comes up regardless and the rest of it
// carries on behind the gates like before.
func restoreGPURuntime() {
	// The door opens the moment this returns, whichever way it returns.
	defer serving.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		restoreGPURuntimeQuietly()
	}()
	select {
	case <-done:
	case <-time.After(driverRestoreBudget):
		logger("[CC] The graphics driver has been going in for %s and is not finished. ah4c is coming up now; the rest of it waits for quiet like everything else does.", driverRestoreBudget)
	}
}

// releaseDriverWaiters lets the engine's first open proceed. Called from more
// than one place and possibly more than once, so it closes exactly once:
// closing a channel twice is a panic that takes every tuner with it.
func releaseDriverWaiters() {
	driverReleased.Do(func() { close(driverRestoreDone) })
}

var (
	driverReleased   sync.Once
	driverRestoreRun sync.Once
)

func restoreGPURuntimeQuietly() {
	defer releaseDriverWaiters()
	if runtime.GOOS != "linux" {
		return
	}
	// A graphics driver is for captioning and nothing else here, so a container
	// with captions switched off does not install one. It is not free: on a
	// fresh bind mount it is a package at a time through dpkg, and that is
	// forty seconds of a startup nobody asked for.
	//
	// It is not simply skipped, though. This is the only stretch of a
	// container's life where nothing can be tuning, so refusing to install here
	// means the install has to happen later, beside live tunes, which is the
	// case that has cost recordings. Switching captions on is what asks for it,
	// and that is where it now runs from — gated and divided, as it has to be
	// once the door is open.
	if !currentCaptionConfig().Enabled {
		logger("[CC] Captions are off, so the graphics driver is left where it is. It goes in when captions are switched on.")
		warmEngineCache()
		return
	}
	restoreSavedDriver()
}

// restoreSavedDriver puts the saved driver back, at most once per process.
func restoreSavedDriver() {
	driverRestoreRun.Do(restoreSavedDriverNow)
}

func restoreSavedDriverNow() {
	// Before the quiet gate, only the check that costs nothing: is anything
	// saved at all. One directory read. The common case — no GPU driver in
	// use — settles instantly and the engine init never waits.
	saved := false
	for _, g := range gpuRuntimes {
		if driverDownloaded(g) {
			saved = true
			continue
		}
		// Some packages but not all of them is a set a failed download left
		// behind. It cannot be restored into anything that works, and the
		// fix needs a button on the page, so say so rather than restoring
		// silence.
		if ents, err := os.ReadDir(driverDir(g)); err == nil {
			for _, e := range ents {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".deb") {
					logger("[CC] The saved %s packages are incomplete — a download must have failed partway. Press the driver download on the Closed Captions page to fetch a fresh set.", g.Name)
					break
				}
			}
		}
	}
	if !saved {
		warmEngineCache()
		return
	}
	// The install never runs while a tune is in flight. Never, not "unless it
	// has been waiting a while".
	//
	// This waited forty seconds for a quiet stretch and then went ahead
	// anyway, on the theory that nice and ionice made that safe. They do not.
	// dpkg fsyncs its way through thirty-seven packages and an array does not
	// care what priority the process asking is; the tune that was running
	// missed its playback confirmation and the recording died. That was my
	// fallback, not the gate — the gate was working, and I overrode it.
	//
	// It was bounded because the engine's one-time open waits on this
	// finishing, and an unbounded wait starved captions completely. So the two
	// are separated instead. If no quiet stretch turns up in the first forty
	// seconds, the engine is released to start without the driver — captions
	// run on the processor for this session and say so — while the install
	// goes on waiting for a moment when it can run without costing anybody a
	// recording. It takes effect at the next container start, because the
	// engine scans for backends once and has already done it by then.
	//
	// A driver is a convenience. A recording is not.
	//
	// On the ordinary path none of that applies any more: this runs before the
	// port is bound, the gate answers yes immediately because nothing can be
	// tuning, and the install is finished before anybody could have asked. All
	// of the above is what happens when it overruns the startup budget and finds
	// itself running beside real tunes after all — the case this was written
	// for, now the exception rather than the rule.
	if !waitTuneQuietHeld(10*time.Second, 40*time.Second) {
		logger("[CC] No quiet stretch for the graphics driver install yet. Captions will run on the processor this session; the driver goes in when the machine is idle and is used from the next start.")
		releaseDriverWaiters()
		// Five more minutes of looking for a quiet stretch, and then it goes in
		// anyway. Everything below is divided a package at a time and runs
		// behind nice and ionice, so proceeding is a second of polite work per
		// package rather than the un-pausable install this gate was written
		// for. Waiting for ever is the worse of the two: a machine that never
		// goes quiet is a machine that never gets its driver, and captions
		// spend the rest of the container's life on the processor.
		awaitQuiet("The graphics driver restore", 10*time.Second, 5*time.Minute)
	}
	need := false
	for _, g := range gpuRuntimes {
		if driverDownloaded(g) && (!driverActive(g) || len(brokenVulkanDrivers()) > 0) {
			need = true
		}
	}
	if need {
		if !serving.Load() {
			logger("[CC] Putting the saved graphics driver back before the web server starts. Nothing can be tuning yet, so this is the one time it costs nobody anything; ah4c answers as soon as it is done.")
		}
		reinstallSavedDriver()
	}
	warmEngineCache()
	// The engine scans for backends once per process. If it already ran that
	// scan while the driver was broken, this repair cannot reach it until the
	// process restarts — which deserves saying plainly, because from outside
	// it looks like a fixed driver being ignored.
	if txStarted() && len(brokenVulkanDrivers()) == 0 {
		if v := currentEngineVariant(); strings.Contains(v, "vulkan") && !txBackendAvailable(txBackendVulkan) {
			logger("[CC] The graphics driver is repaired, but the engine had already started without it. Restart the container to caption on the GPU.")
		}
	}
}

// awaitDriverRestore blocks until the startup driver restore has finished, or
// until waiting stops being worth it.
//
// Bounded, because a restore that cannot find its quiet stretch may take
// minutes and a stream should not be silent for all of them. Two minutes
// matches what the engine's own open allows, so a caller that waits here and
// then opens the engine cannot be caught out twice by the same delay.
func awaitDriverRestore() {
	select {
	case <-driverRestoreDone:
	case <-time.After(2 * time.Minute):
		logger("[CC] The graphics driver is still being put back after two minutes; choosing a backend with whatever loads now")
	}
}

// txInited reports whether the engine's one-time initialization has run.
func txInited() bool {
	select {
	case <-txInitedCh:
		return true
	default:
		return false
	}
}

// txInitedCh closes when initTranscribe's Once has completed.
var txInitedCh = make(chan struct{})

// txStarted reports whether the engine is up and can be asked questions.
//
// txInited is not enough on its own: initialization that gives up early — no
// build published here, nothing downloaded yet — still closes the channel on
// its way out, having registered none of the entry points. Calling one of them
// on that path is a nil call, which takes the process down.
func txStarted() bool {
	return txInited() && txErr == nil && txBackendAvailable != nil
}

// warmEngineCache primes the which-builds-can-load cache off the tune path.
// The first tune otherwise pays those dlopens itself — under the tuner lock,
// where one slow driver chain delays every tuner's tune.
func warmEngineCache() {
	// Held quiet, not observed quiet. This opens driver libraries, and a dlopen
	// cannot be stopped once it starts, so it is the uninterruptible work the
	// first rule is about. The plain wait does not do it: before the first tune
	// of a container's life the machine reads as quiet because nothing has
	// asked yet, and this runs before the server is even listening — so it
	// answered instantly and started loading drivers into the storm.
	//
	// Bounded, and it proceeds if the minute runs out. What it is protecting
	// against is a dlopen chain landing inside a tune, which is a fraction of a
	// second; what waiting for ever costs is the cache never being primed at
	// all, which puts that same dlopen chain on the first tune instead, under
	// the tuner lock, where it delays every tuner rather than none.
	awaitQuiet("The engine cache warm-up", 10*time.Second, time.Minute)
	// Throw away whatever was learned before this point first.
	//
	// The driver restore waits for the machine to go quiet, so on a fresh
	// container there is a stretch — a minute, sometimes several — where the
	// Vulkan loader genuinely is not installed yet. Anything that asks during
	// that stretch gets a truthful "no" and the cache keeps it for the life of
	// the process, so the driver arriving a moment later changes nothing: the
	// page still says the loader will not load, the engine picker still refuses
	// to offer a GPU build, and everything runs on the processor until somebody
	// recreates the container.
	//
	// This is why it showed up on Intel and nowhere else. An NVIDIA card gets
	// its loader injected by the container runtime before ah4c starts, so the
	// answer is the same whenever it is asked. An Intel or AMD chip depends on
	// exactly the packages this restore puts back, which is the only case where
	// the answer changes underneath the cache.
	//
	// So the moment the restore is done is the moment to forget, and priming
	// straight afterwards means nothing is left to ask a stale question of.
	// Built beside the old answers rather than on top of the space where they
	// were. Emptying the cache and refilling it leaves a window in which it is
	// empty, and a tune arriving in that window pays for the driver load
	// itself — which is the one thing this function exists to prevent.
	fresh := map[string]bool{}
	for _, v := range engineVariants {
		if v.Key == "auto" || v.Needs == "" {
			continue
		}
		h, err := purego.Dlopen(v.Needs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		fresh[v.Needs] = err == nil && h != 0
	}
	usableLock.Lock()
	usableCache = fresh
	usableLock.Unlock()
	// The tune path reads the stored answer and never asks. This is where it is
	// answered, before the port is bound.
	refreshGPUReady()
}

// reinstallSavedDriver puts the driver back after a container rebuild, from
// the copy in the bind mount, so the choice survives without anyone pressing
// anything or the network being reachable.
func reinstallSavedDriver() {
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
// viewer would recognize.
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
// recognize, which is what a caption needs.
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
	// lastCR is when the display last rolled, and crCopies how many repeats of
	// that carriage return are still owed; see next() for what they pace.
	lastCR   time.Time
	crCopies int
	// popon writes the caption where it cannot be seen and then shows it whole,
	// instead of typing it onto the screen as it arrives. held is the words of
	// the caption being assembled, which have not been sent anywhere yet.
	// lastBlock is when a caption was last shown and blockCopies how many
	// repeats of that command are still owed, the same bookkeeping the roll
	// keeps for its carriage return.
	popon bool
	held  []string
	// popPending is how many finished captions are queued but not yet shown.
	popPending  int
	lastBlock   time.Time
	blockCopies int
	rows        byte // ccRU2 / ccRU3 / ccRU4
	started     bool
	col         int
	maxCol      int
	upper       bool
	// drains counts calls to next since drainFrom, which is how fast this
	// channel actually clears its queue: one entry leaves per call, whatever
	// the picture rate or the caption packet layout upstream turns out to be.
	// Measuring it rather than assuming it is what lets the backlog be talked
	// about in seconds — the only unit a model, a frame rate or a format can
	// all be compared in.
	drains    int
	drainFrom time.Time
	drainRate float64
	// toldRate is the picture rate the injector read out of the stream, used
	// until this channel has clocked itself.
	toldRate float64
	// minRollGap is the least time between two rolls, from the page's roll
	// speed setting.
	minRollGap time.Duration
	// credit is the pacing allowance, in characters, accrued per picture, and
	// pace the rate it accrues at. maxLag is how much unread text may wait
	// before the meter stands aside.
	credit float64
	pace   float64
	maxLag float64
	// pendingBreak is a carriage return the last phrase finished with and this
	// one has yet to spend. See pushText.
	pendingBreak bool
}

// pairRate is how many byte pairs a second this channel is clearing, measured.
//
// It settles within a second or two of the stream starting and is re-measured
// on a rolling window, so a stream that changes rate mid-flight is followed
// rather than remembered. Before there is enough to go on it answers with the
// format's base rate, which is right for the common case and never far wrong.
func (c *cea608) pairRate() float64 {
	if c.drainRate > 0 {
		return c.drainRate
	}
	if c.toldRate > 0 {
		return c.toldRate
	}
	return cc608NominalRate
}

// setPictureRate takes the rate the injector derived from the stream's own
// timestamps, for the stretch before this channel has clocked itself. The
// measured rate wins once there is one: what is wanted is the rate pairs are
// actually leaving at, and the picture rate is the best available guess at it
// rather than a substitute for it.
func (c *cea608) setPictureRate(fps float64) {
	if fps <= 0 {
		return
	}
	c.mu.Lock()
	c.toldRate = fps
	c.mu.Unlock()
}

// countDrain records one call and re-measures the rate once a window's worth
// has gone by. Called with the lock held.
func (c *cea608) countDrain() {
	now := time.Now()
	if c.drainFrom.IsZero() {
		c.drainFrom, c.drains = now, 0
		return
	}
	c.drains++
	// The first window is short, because until it closes the only thing to go
	// on is an assumption about the format, and an assumption about the format
	// is what gets this wrong on a sixty-picture stream. One second of real
	// counting replaces it. After that the window widens: the rate is known,
	// and what is wanted is a steady figure that still follows a stream which
	// changes underneath it.
	window := 4 * time.Second
	if c.drainRate == 0 {
		window = time.Second
	}
	if elapsed := now.Sub(c.drainFrom); elapsed >= window {
		if r := float64(c.drains) / elapsed.Seconds(); r > 1 {
			c.drainRate = r
		}
		c.drainFrom, c.drains = now, 0
	}
}

// captionLag is how much unread text the meter may hold for this model.
func captionLag(m captionModel, cfg captionConfig) float64 {
	if m.Streaming {
		return ccLagStreaming
	}
	if w := phraseWindowFor(quirksFor(m), cfg); w > 0 {
		return w
	}
	return ccLagFallback
}

func newCEA608(style string, upper bool, onScreen float64, wpm int, maxLag float64) *cea608 {
	rows := byte(ccRU3)
	popon := false
	switch style {
	case "rollup2":
		rows = ccRU2
	case "rollup4":
		rows = ccRU4
	case "box2":
		popon = true
	}
	n := 3
	switch rows {
	case ccRU2:
		n = 2
	case ccRU4:
		n = 4
	}
	gap := rollGapFor(onScreen, n)
	if popon {
		// A roll divides the wanted time on screen by the rows, because a line
		// survives that many rolls. A box does not divide it: the caption goes
		// up whole and comes down whole, so the whole of it is what the setting
		// asks for.
		//
		// Floored at the guidance for two lines rather than for one, because
		// two lines is what a box puts up.
		gap = rollGapFor(onScreen, 1)
		if min := ccMinOnScreen(2); gap < min {
			gap = min
		}
	}
	return &cea608{rows: rows, popon: popon, maxCol: 32, upper: upper, pace: paceFor(wpm), maxLag: maxLag,
		minRollGap: gap}
}

func (c *cea608) ctrl(code byte) {
	// Control codes are sent twice; a decoder that catches the pair twice acts
	// on it once, and the repeat is what survives a dropped frame.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(code)}, [2]byte{odd608(ccCtrlCC1), odd608(code)})
}

// begin puts the decoder into roll-up mode on the bottom row.
//
// Box style has no mode to set here: every caption states its own, and the
// display is cleared instead. Starting is not always starting from nothing —
// the backlog cull above throws away the queue and starts again, and whatever
// the decoder was showing when that happened is still on the screen. A roll
// scrolls it away in its own time; a box has to erase it. Erasing a blank
// display costs a control code and does nothing, which is what it does at a
// genuine stream start.
func (c *cea608) begin() {
	if c.popon {
		c.ctrl(ccEDM)
		c.started = true
		c.col = 0
		return
	}
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

// wrap608 lays words out into caption rows of at most maxCol characters.
//
// A separate function from the roll-up's wrapping because the two need
// different things at different times. A roll-up wraps as it writes, because it
// is writing to the screen and the screen is what it is. A box has to know the
// shape of the whole caption before it sends any of it: the rows are addressed
// by number, so which row a word lands on has to be decided first.
//
// A word longer than a row is broken rather than dropped. Nothing in English
// runs to thirty-two characters, but a caption is not always English and a
// silently missing word is worse than an ugly one.
func wrap608(words []string, maxCol int) []string {
	var lines []string
	cur := ""
	for _, w := range words {
		for len(w) > maxCol {
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
			lines = append(lines, w[:maxCol])
			w = w[maxCol:]
		}
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= maxCol:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// popPages is how many rows of caption may wait for an utterance to end.
//
// A box cannot show anything until the caption is complete, and a streaming
// model only says a phrase is complete when the speaker stops. Somebody talking
// without a break would otherwise hold every word until they did. Two captions'
// worth is the bound: past it the earlier rows go up on their own, which is
// what a broadcast encoder does with a long sentence anyway.
const popPages = 4

// pac addresses a row, so the next characters land on it.
//
// Row 15 is the bottom of the screen and row 14 the line above it. The two
// share a first byte and are told apart by the second: 0x40 to 0x5F selects row
// 14 and 0x60 to 0x7F row 15, and within either range the low bits carry the
// color, the indent and the underline. 0x50 and 0x70 are the same choice in
// each — white, no italics, no underline, indent zero — which is why they
// differ by the one bit that picks the row.
//
// Sent twice, like every other control pair: a decoder that catches the repeat
// acts once, and the repeat is what survives a dropped frame.
func (c *cea608) pac(row int) {
	b := byte(0x70)
	if row == 14 {
		b = 0x50
	}
	c.queue = append(c.queue,
		[2]byte{odd608(ccCtrlCC1), odd608(b)},
		[2]byte{odd608(ccCtrlCC1), odd608(b)})
}

// showPopon writes one caption where it cannot be seen and then shows it whole.
//
// This is the difference between the two styles and it is the whole of it. A
// roll-up writes to the screen, so the viewer watches the words arrive one at a
// time and watches the line above scroll away. A box writes to the decoder's
// other memory, which is not on screen, and then swaps the two: the caption
// appears finished, all of it at once, and stays until the next swap replaces
// it. It is what broadcast captioning does on anything not being typed live.
//
// Four commands say it. RCL puts the decoder in this mode and points writing at
// the memory nobody can see; ENM clears that memory first, so a caption cannot
// inherit the end of an older one; the rows are addressed and written; EOC
// swaps. Nothing is visible between the first three and the fourth, which is
// the point — and it is why the pacing meter has no business here, because
// there is nothing to pace when there is nothing to watch. What the meter does
// for a roll-up, the dwell on EOC does for this: it decides how long a finished
// caption stays up, which is the only timing a viewer of this style can see.
func (c *cea608) showPopon(lines []string) {
	if len(lines) == 0 {
		return
	}
	if len(lines) > 2 {
		lines = lines[:2]
	}
	c.ctrl(ccRCL)
	c.ctrl(ccENM)
	row := 15
	if len(lines) == 2 {
		row = 14
	}
	for _, line := range lines {
		c.pac(row)
		c.col = 0
		for _, r := range line {
			c.writeRune(r)
		}
		row++
	}
	c.ctrl(ccEOC)
	c.popPending++
	c.col = 0
}

// ccMaxBacklogSec is the most unshown caption data we will hold, measured as
// the time it would take to air. Reaching it means recognition has outrun the
// display, and what is held past this point can only ever be shown late.
//
// In seconds rather than byte pairs for the same reason as the roll pressure:
// the pair count that used to stand here was a hundred and fifty, annotated as
// five seconds at thirty pictures a second, which made it two and a half on a
// sixty-picture stream. The ceiling moved with the format instead of staying
// where it was meant to be.
const ccMaxBacklogSec = 5.0

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
	if float64(len(c.queue)) > ccMaxBacklogSec*c.pairRate() {
		c.queue = c.queue[:0]
		c.started = false
		c.col = 0
		c.popPending = 0
	}
	if !c.started {
		c.begin()
	}
	if c.popon {
		// Held rather than written. A box shows a caption complete or not at
		// all, so there is nothing to send until the phrase is finished — and
		// for a streaming model, which finalizes a few words at a time, that is
		// the end of the utterance rather than the end of every arrival.
		c.lastText = time.Now()
		c.held = append(c.held, strings.Fields(text)...)
		if breakAfter {
			c.flushPopon()
		} else if lines := wrap608(c.held, c.maxCol); len(lines) >= popPages {
			c.flushPopon()
		}
		return
	}
	// The break owed by the previous phrase is taken now, with this phrase's
	// words behind it, and not when that phrase ended.
	//
	// Rolling at the end of a phrase rolls to a blank row. The line just read
	// moves up — off the screen entirely at two rows — and the viewer is left
	// looking at nothing for as long as it takes the speaker to say the next
	// thing, which is most of three seconds. That is the hold: not the pace of
	// the roll, but a roll performed before there was anything to roll to.
	// A broadcast encoder rolls when the new line arrives, because the roll is
	// how the new line gets its row, and that is all this is.
	if c.pendingBreak {
		c.pendingBreak = false
		c.newRow()
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
		// Owed, not taken: the next phrase collects it on the way in.
		c.pendingBreak = true
	}
}

// flushPopon sends everything held as one caption, or as several when it does
// not fit on two rows.
//
// A sentence longer than sixty-four characters becomes two captions shown one
// after the other, which is how a broadcast encoder handles the same thing.
// Each of them is a caption in its own right and waits out its own time on
// screen, because a page nobody could read is not a page that was shown.
//
// Called with the lock held.
func (c *cea608) flushPopon() {
	lines := wrap608(c.held, c.maxCol)
	c.held = c.held[:0]
	for i := 0; i < len(lines); i += 2 {
		end := i + 2
		if end > len(lines) {
			end = len(lines)
		}
		c.showPopon(lines[i:end])
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

// The roll speeds offered on the page: the least time between two rolls of the
// display.
//
// The text itself is paced by the channel — so many characters a second, no
// faster — but a carriage return is only two byte pairs, so a burst of
// recognition can roll the display several times in well under a second and a
// line leaves the screen before anyone has read it. This floor keeps a finished
// line put for a beat, which is what broadcast roll-up looks like.
//
// How long a beat should be is a matter of taste and eyesight, and of how many
// rows are up: at two rows a roll is the only thing standing between a line and
// the edge of the screen, while at four it has three more rows to travel. So it
// is a setting rather than a number, and the default is the broadcast one it
// has always been.
const ()

// waiting reports whether anything is queued behind the carriage return at the
// head — that is, whether holding the roll is holding words back.
//
// This is the whole of the rule the pacing needs, and every threshold tried
// here was an approximation of it. A count of pairs was a number about one
// model on one stream, and half a second of channel time was a number about
// one taste; both asked "is a lot waiting", when the question is "is anything
// waiting". A finished line may sit and be read when there is nothing to say.
// The moment there are words recognized and not yet on screen, every
// millisecond of dwell is a millisecond they are late by, and no amount of
// composure is worth that — least of all in the middle of a sentence, where a
// wrapped line holds back the end of the phrase it belongs to.
//
// Two, because a control code occupies the queue twice: the pair and the copy
// a decoder is only guaranteed to recognize when it arrives back to back.
// ccCharsPerPair is what a queued pair is worth in reading time. writeRune
// packs two characters into one wherever it can, and measurement across real
// captions puts the average at 1.97, so two is the figure to reason with.
const ccCharsPerPair = 2.0

// ccMaxLagSec is how much unread text may wait before the meter stands aside.
//
// The meter smooths a phrase onto the screen instead of dumping it, which costs
// nothing in the long run: the words arrive at the speed they were spoken, so a
// pace set to reading speed matches them over any window longer than a phrase.
// What it cannot do is run slower than the speaker indefinitely. If it did, the
// queue would grow without bound and the captions would fall further behind the
// picture every minute.
//
// So the meter yields once more than this much reading time is waiting. Six
// seconds is a phrase and a half at the longest sentence length offered: enough
// to smooth one phrase completely, and short enough that two piling up drains
// at the channel's own speed rather than settling into a permanent lag.
//
// It was five seconds of *channel* time, which sounds similar and is not: the
// channel carries sixty characters a second and nobody reads at a quarter of
// that, so five seconds of channel time is twenty seconds of reading. A backlog
// could sit just under that threshold for ever — captions a quarter of a minute
// late, with nothing in the design to drain them.
func (c *cea608) waiting() bool {
	// A box is behind when captions are queued behind the one on screen, and
	// that is the only reading of it that means anything here.
	//
	// The character count below asks how much reading time is waiting, which
	// assumes the queue is text on its way to the screen. For a box it is not:
	// a whole caption sits in the queue and then appears at once, so a single
	// ordinary two line caption measures as five seconds of reading and would
	// declare a backlog every time — standing the dwell down permanently and
	// flipping captions as fast as they were recognized.
	if c.popon {
		return c.popPending > 1
	}
	if c.maxLag <= 0 {
		return len(c.queue) > 2
	}
	if c.pace <= 0 {
		return len(c.queue) > 2
	}
	return float64(len(c.queue))*ccCharsPerPair/c.pace > c.maxLag
}

// The tolerance is the shape of the model, not a constant.
//
// A phrase model hands over a whole sentence at once, so the meter must be
// allowed to hold roughly one phrase in order to spread it — that is the entire
// job. A streaming model hands over words as they are spoken, so nothing needs
// spreading and every queued character is pure lag on a model chosen precisely
// for not having any. Giving both the same six seconds put Nemotron six seconds
// behind the picture to smooth a burst it never produces.
// Zero turns the meter off, which is what a streaming model wants.
//
// The meter exists to spread a burst. A phrase model hands over a whole
// sentence at once and the words have to be let onto the screen at reading
// speed or they land in a heap; that is the entire job. A streaming model hands
// over words as it hears them, already at the speed they were spoken, so there
// is nothing to spread and every character the meter holds is delay added to a
// model chosen for not having any.
//
// The dwell still applies either way: a finished line rests before it rolls
// whichever model wrote it.
const (
	ccLagStreaming = 0
	ccLagFallback  = 4.0
)

// cc608NominalRate is the pair rate assumed until the channel has been running
// long enough to measure its own. Field 1 of CEA-608 carries one pair per
// picture at the base rate of the format.
const cc608NominalRate = 29.97

// next returns the pair of bytes to attach to the next video frame.
// cc608Pace is how fast characters are let onto the screen, per second.
//
// The caption channel carries one character pair per picture — sixty characters
// a second on a sixty hertz stream, four times the rate anybody speaks. So a
// phrase emptied
// onto the display in a quarter of the time it took to say, the display then sat
// idle until the next phrase arrived, and the carriage return dwell added
// another pause on top of that. Text flew, then stopped, then flew. It is the
// channel's rate rather than the model's, which is why it looked the same
// whichever model was running.
//
// Fifteen is where the published guidance overlaps. The BBC puts subtitle
// speed at 160 to 180 words a minute, the DCMP's captioning key at 130 to 160,
// and the industry figure for characters a second is 20 at the most with 12 to
// 18 comfortable. Fifteen characters a second is about 155 words a minute, which
// is inside both word ranges and in the middle of the character one.
//
//	https://www.clevercast.com/bbc-subtitling-guidelines/
//	https://dcmp.org/learn/601-captioning-key---presentation-rate
//
// The reason it works is simpler than the standards: it is how fast people
// speak, which is the rate the words arrive at. Pacing the display to it makes
// the screen fill at the speed the words were said, which is what makes real
// roll-up readable — the dwell was never doing that job.
//
// It cannot fall behind on its own: the words arrive at speaking speed, so a
// pace set to speaking speed matches them over any window longer than one
// phrase. What it cannot absorb is a genuine backlog, and that is what the
// catch-up below is for.
// ccMinOnScreen is how long a line must be readable before it leaves, from the
// captioning guidance: a minimum of one second for a single line, one and a half
// for two, two for three. A roll-up does not put lines up together — each row is
// added and the oldest scrolls away — so what a row gets is the gap between
// rolls multiplied by the number of rows above it.
//
// So the floor is on the product rather than on the gap, and it is a floor and
// not a setting. Roll speed is taste; a line leaving before it can be read is
// not a taste, it is a caption nobody got to have.
func ccMinOnScreen(rows int) time.Duration {
	switch {
	case rows <= 1:
		return time.Second
	case rows == 2:
		return 1500 * time.Millisecond
	default:
		return 2 * time.Second
	}
}

// captionOnScreen is what the page offers for the least time a line stays
// readable, in seconds. The guidance minimum for three rows is two; the rest is
// room for anybody who wants longer.
var captionOnScreen = []float64{2, 3, 4, 5, 6, 8}

// rollGapFor turns a wanted time on screen into the gap between rolls.
//
// A roll-up does not put lines up together — each row is added and the oldest
// scrolls away — so a row is readable for the gap between rolls multiplied by
// the number of rows above it. Asking for six seconds at three rows is a two
// second gap.
//
// Floored at the guidance whatever is asked for: a minimum of one second for a
// single line, one and a half for two, two for three. Time on screen is taste
// above that floor and not below it, because a line leaving before it can be
// read is not a preference, it is a caption nobody got to have.
func rollGapFor(want float64, rows int) time.Duration {
	if rows < 1 {
		rows = 1
	}
	gap := time.Duration(want * float64(time.Second) / float64(rows))
	if min := ccMinOnScreen(rows) / time.Duration(rows); gap < min {
		gap = min
	}
	return gap
}

const cc608Pace = 15.0

// captionSpeeds is what the page offers, in words a minute. The published
// guidance puts subtitle speed between 120 and 160 depending on audience and
// medium — the BBC at the top of it, the DCMP lower — so the range is offered
// whole rather than a number chosen out of it on somebody's behalf.
var captionSpeeds = []int{120, 130, 140, 150, 160}

// ccCharsPerWord converts words a minute into characters a second.
//
// Derived rather than published, and the derivation matters because there are
// two conventions and they differ by sixteen percent. The five character word
// is the typing standard; captioning counts real words — the DCMP is explicit
// that "each word is counted, as opposed to basing the calculation on the
// number of characters". English prose averages 4.79 letters a word across
// Norvig's corpus of 743 billion, which is 5.79 with the space.
//
// The check that settles it: seventeen characters a second divided by 5.79 is
// 176 words a minute, and the BBC's own research put "175WPM about right". The
// typing constant gives 204, outside every published band.
//
//	https://norvig.com/mayzner.html
const ccCharsPerWord = 5.8

func paceFor(wpm int) float64 {
	for _, w := range captionSpeeds {
		if wpm == w {
			return float64(wpm) * ccCharsPerWord / 60
		}
	}
	return cc608Pace
}

func (c *cea608) next() [2]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.countDrain()
	// Metered at speaking speed unless there is a real backlog, in which case
	// being current matters more than reading evenly and the channel is used
	// for what it is worth.
	//
	// Characters only. A control code is sent twice and a decoder is only
	// guaranteed to drop the repeat when the two arrive back to back — so
	// withholding between them turns one carriage return into two, which rolls
	// twice and leaves a blank row between every pair of lines. The meter is
	// about how fast words appear; it has no business inside a control pair.
	if rate := c.pairRate(); rate > 0 {
		c.credit += c.pace / rate
		if c.credit > c.pace {
			c.credit = c.pace
		}
	}
	// Charged per character, not per pair.
	//
	// writeRune packs two characters into one pair wherever it can, and this
	// spent one credit per pair — so a rate set to fifteen characters a second
	// put thirty on the screen, and the words-a-minute figure on the page meant
	// half what it said. Reported as captions still being too fast, which they
	// were, by a factor of two.
	//
	// A pair whose second byte is padding carries one character; any other
	// carries two. Control codes are not charged at all: they are not words,
	// and withholding inside a doubled pair splits it and rolls twice.
	if c.maxLag > 0 && !c.popon && len(c.queue) > 0 && !c.headIsControl() && !c.waiting() {
		cost := 1.0
		if c.queue[0][1] != odd608(cc608Null) {
			cost = 2
		}
		if c.credit < cost {
			return [2]byte{odd608(cc608Null), odd608(cc608Null)}
		}
		c.credit -= cost
	}
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
	// The moment a caption changes waits for the dwell, so that what is on the
	// screen has been there long enough to read. Which code that is depends on
	// the style: a roll-up changes the screen with a carriage return, a box
	// with the swap that shows what it has loaded. Either one's doubled copy is
	// exempt — control codes go out twice back to back, and a decoder is only
	// guaranteed to drop the repeat when it arrives as one.
	//
	// Everything a box sends before that swap goes out at the channel's own
	// speed and is held by nothing, which is the point of the style: the rows
	// are being written where they cannot be seen, so there is no reason to
	// spread them out and every reason not to. The caption is loaded and ready,
	// and the only thing waiting is the instant it appears.
	if p := c.queue[0]; p[0] == odd608(ccCtrlCC1) {
		switch {
		case c.popon && p[1] == odd608(ccEOC):
			switch {
			case c.blockCopies > 0:
				c.blockCopies--
			case time.Since(c.lastBlock) < c.minRollGap && !c.waiting():
				return [2]byte{odd608(cc608Null), odd608(cc608Null)}
			default:
				c.lastBlock = time.Now()
				c.blockCopies = 1
				if c.popPending > 0 {
					c.popPending--
				}
			}
		case !c.popon && p[1] == odd608(ccCR):
			switch {
			case c.crCopies > 0:
				c.crCopies--
			case time.Since(c.lastCR) < c.minRollGap && !c.waiting():
				return [2]byte{odd608(cc608Null), odd608(cc608Null)}
			default:
				c.lastCR = time.Now()
				c.crCopies = 1
			}
		}
	}
	p := c.queue[0]
	c.queue = c.queue[1:]
	return p
}

// headIsControl reports whether the next thing out is a control code rather
// than text. Callers hold the lock.
func (c *cea608) headIsControl() bool {
	return len(c.queue) > 0 && c.queue[0][0] == odd608(ccCtrlCC1)
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
		switch {
		case i == 0:
			// Field 1, which carries the captions.
			b = append(b, 0xFC, pair[0], pair[1])
		case i == 1:
			// Field 2, likewise. Claiming a field carries something it does not
			// makes a player offer a caption track with nothing in it.
			b = append(b, 0xF9, 0x00, 0x00)
		default: // nothing to send this picture; padding, marked invalid
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
	// pmtPatch is the program table rewritten to announce the caption
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

	// Announce the caption service in the program table, so a player that
	// does not decode the video to look for caption messages still knows they
	// are there.
	if pid == ci.pmtPID && ci.videoPID >= 0 && !ci.pmtDone {
		if q := addCaptionDescriptor(p, ci.videoPID); q != nil {
			ci.pmtPatch = q
			logger("[CC] %s announced the caption service in the program table", ci.log)
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

// captionDescriptor announces the one service this stream carries.
//
// A player that does not decode the video looking for caption messages finds
// out captions exist from this and nothing else, which is why some show none
// without it. Announcing a service that turns out to be empty is the thing to
// avoid: a decoder told it is there and finding nothing shows a blank.
var captionDescriptor = []byte{
	0x86, 0x07, // tag, length
	0xE1, // reserved, one service

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
			desc := captionDescriptor
			entry = append(entry, desc...)
			n := esil + len(desc)
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
				// One field-1 pair leaves per access unit, so the picture rate
				// is the rate the caption channel clears its queue at — sixty
				// a second here, thirty there, and nothing in the encoder
				// should be guessing which. It is measured independently as
				// well; this is so the first second is right too.
				ci.enc.setPictureRate(fps)
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
// something the receiver needs: the program clock, or a flag marking a
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
			// A video packet with no payload carries the program clock, not
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
// Where the models attach
// ---------------------------------------------------------------------------

// Every model has knobs, and every model's knobs live in that model's own file.
// This is the joint they meet at, and it is deliberately the only one.
//
// captions.go is the common part: the audio splitter, the voice detector, the
// recognizer, both caption encoders, the injector, the tune gate. None of it
// knows which model it is serving. cohere.go, nemotron.go and moonshine.go each
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
	case nemotronStreaming.Key:
		return nemotronQuirks
	case moonshineTiny.Key:
		return moonshineQuirks
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
// Speech recognition
// ---------------------------------------------------------------------------

// asrSampleRate is the one rate every model here listens at.
const asrSampleRate = 16000

// recognizer is a loaded speech model. The phrase segmenter, the CEA-608
// encoder and the injector talk to this and nothing below it.
//
// beginStream returns an error on a model that cannot transcribe continuously,
// which is the caller's cue to fall back to a phrase at a time.
type recognizer interface {
	transcribe(pcm []float32) (string, error)
	beginStream(language string) error
	feedStream(pcm []float32) *streamResult
	finishStream() *streamResult
	idleFlush() *streamResult
	Close()
}

// streamResult is what a streaming feed reports: finalized words, grouped and
// cleaned, plus whether the speaker finished an utterance.
type streamResult struct {
	Text  string       `json:"text"`
	EOU   int          `json:"eou"`
	Words []streamWord `json:"words"`
}

// streamWord is one finalized word and where it fell in the audio.
type streamWord struct {
	W     string  `json:"w"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// words joins the grouped words of this result.
func (r *streamResult) words() string {
	if len(r.Words) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.Words))
	for _, w := range r.Words {
		if t := cleanRecognized(w.W); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
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
	// Capability probes, from transcribe.h. A model that does not expose a
	// toggle must not be asked to change it: the engine warns and carries on
	// with its default, once per call, for ever.
	txFeaturePNC = 4
	txFeatureITN = 5

	// Punctuation and capitalization, and inverse text normalization — turning
	// spoken numbers, dates and currencies into their written form. Both from
	// transcribe.h, both per call, both left at the family's own default
	// unless a model says otherwise.
	txPNCDefault = 0
	txPNCOff     = 1
	txPNCOn      = 2
	txITNDefault = 0
	txITNOff     = 1
	txITNOn      = 2

	// K/V activation precision, from transcribe.h. Auto is f16 whenever the
	// weights are quantized, which ours always are.
	txKVAuto = 0
	txKVF32  = 1
	txKVF16  = 2

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
	txABIToken         = 8
	txABIStreamUpdate  = 9
	txABIStreamText    = 10
	txABIBackendDevice = 13
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
	// A registered compute device, as the engine sees it. The strings are
	// owned by the library and live as long as it does.
	txBackendDevice struct {
		structSize  uint64
		name        *byte
		description *byte
		kind        *byte
		deviceID    *byte
		memoryTotal uint64
		memoryFree  uint64
		deviceType  int32
		_           int32
	}
)

var (
	txOnce sync.Once
	txErr  error

	txVersion       func() string
	txStatusString  func(status int32) string
	txInitBackends  func(dir string) int32
	txABIStructSize func(which int32) uint64
	txNTokens       func(session uintptr) int32
	txGetToken      func(session uintptr, i int32, out unsafe.Pointer) int32
	txBatchNTokens  func(session uintptr, i int32) int32
	txBatchGetToken func(session uintptr, i, j int32, out unsafe.Pointer) int32

	txDeviceCount func() int32
	txDeviceInit  func(p unsafe.Pointer)
	txGetDevice   func(index int32, out unsafe.Pointer) int32

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

	txSetAbortCallback func(session uintptr, cb uintptr, userData unsafe.Pointer)
	txBackendAvailable func(kind int32) bool
	// Returns the backend's name — "cpu", "vulkan", "cuda" — not an enum.
	txModelSupports     func(model uintptr, feature int32) bool
	txModelBackend      func(model uintptr) string
	txRunBatch          func(session uintptr, pcm, nSamples unsafe.Pointer, n int32, params unsafe.Pointer) int32
	txBatchNResults     func(session uintptr) int32
	txBatchStatus       func(session uintptr, i int32) int32
	txBatchFullText     func(session uintptr, i int32) string
	txLogSet            func(cb uintptr, userData unsafe.Pointer)
	txWasAborted        func(session uintptr) bool
	txLoadParamsInit    func(p unsafe.Pointer)
	txSessionParamsInit func(p unsafe.Pointer)
	txRunParamsInit     func(p unsafe.Pointer)
	txStreamParamsInit  func(p unsafe.Pointer)
	txStreamUpdateInit  func(p unsafe.Pointer)
	txStreamTextInit    func(p unsafe.Pointer)
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
		// The engine can only scan for backends once per process, so tell
		// the restore-repair goroutine when that has happened; a driver fixed
		// after this point needs a restart to be seen, and the log says so.
		defer close(txInitedCh)
		// A fresh container may still be putting the driver back; opening the
		// engine before that finishes would find no Vulkan library and settle
		// on the processor for the life of the process. Bounded: a restore
		// that is stuck does not get to hold captions hostage.
		select {
		case <-driverRestoreDone:
		case <-time.After(120 * time.Second):
			logger("[CC] The driver restore is still running after two minutes; opening the engine with whatever is loadable now")
		}
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
		purego.RegisterLibFunc(&txBackendAvailable, handle, "transcribe_backend_available")
		purego.RegisterLibFunc(&txModelSupports, handle, "transcribe_model_supports")
		purego.RegisterLibFunc(&txModelBackend, handle, "transcribe_model_backend")
		purego.RegisterLibFunc(&txRunBatch, handle, "transcribe_run_batch")
		purego.RegisterLibFunc(&txBatchNResults, handle, "transcribe_batch_n_results")
		purego.RegisterLibFunc(&txBatchStatus, handle, "transcribe_batch_status")
		purego.RegisterLibFunc(&txBatchFullText, handle, "transcribe_batch_full_text")
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
		purego.RegisterLibFunc(&txNTokens, handle, "transcribe_n_tokens")
		purego.RegisterLibFunc(&txGetToken, handle, "transcribe_get_token")
		purego.RegisterLibFunc(&txBatchNTokens, handle, "transcribe_batch_n_tokens")
		purego.RegisterLibFunc(&txBatchGetToken, handle, "transcribe_batch_get_token")
		purego.RegisterLibFunc(&txDeviceCount, handle, "transcribe_backend_device_count")
		purego.RegisterLibFunc(&txDeviceInit, handle, "transcribe_backend_device_init")
		purego.RegisterLibFunc(&txGetDevice, handle, "transcribe_get_backend_device")

		if err := txCheckABI(); err != nil {
			txErr = err
			return
		}
		// Take the engine's logging before the backend scan, or its running
		// commentary goes to stderr and buries everything else. It writes a
		// line about its key/value cache for every phrase it transcribes, which
		// on a busy channel is a line a second, for ever.
		txLogSet(txLogCallback(), nil)
		// Ask the engine for its per-stage timing breakdown; the log callback
		// samples it every half minute. This is how a slow stage gets named
		// by the engine itself instead of inferred from the outside.
		if os.Getenv("TRANSCRIBE_PERF_DEBUG") == "" {
			os.Setenv("TRANSCRIBE_PERF_DEBUG", "cohere")
		}
		// The conv-dispatch overrides were tried here and measured: the
		// per-utterance encoder time did not move. The engine's own
		// defaults stand.
		// Point the graphics driver's compiled-shader cache at the bind
		// mount. The first Vulkan initialization compiles every compute
		// shader the engine uses — seconds of every core, and un-pausable.
		// In the container's own filesystem that cache dies with every
		// rebuild and the storm repeats on the first captioned tune of each
		// new container; in the bind mount it is paid once per install.
		persistShaderCache()
		// Before the Vulkan module creates its instance: never let it pick a
		// software renderer. Mesa's driver package installs llvmpipe alongside
		// the real drivers, and when the real one cannot reach the card the
		// loader hands ggml llvmpipe instead — a "GPU" that is the processor
		// wearing a costume, three times slower than the processor backend
		// used honestly. Better no Vulkan device than that one: the engine
		// then lands on its native CPU backend and says so.
		pinHardwareVulkanICDs()
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
		logComputeDevices()
		if strings.Contains(variant, "vulkan") && !txBackendAvailable(txBackendVulkan) {
			explainMissingVulkan()
		}
	})
	return txErr
}

// explainMissingVulkan prints everything that decides whether Vulkan works,
// at the moment it is found missing. One block in the log instead of a day
// of comparing guesses: the device nodes, the loader, each driver manifest
// and whether its library loads, and what the manifest pin was set to.
func explainMissingVulkan() {
	logger("[CC] Vulkan was asked for and is not available. The facts:")
	if nodes := renderNodes(); len(nodes) == 0 {
		logger("[CC]   - /dev/dri has no render nodes: the compose file is not passing the GPU through")
	} else {
		logger("[CC]   - /dev/dri render nodes: %s", strings.Join(nodes, ", "))
	}
	if h, err := purego.Dlopen("libvulkan.so.1", purego.RTLD_NOW); err != nil || h == 0 {
		logger("[CC]   - the Vulkan loader itself does not load: %v", err)
	} else {
		logger("[CC]   - the Vulkan loader loads")
	}
	if v := os.Getenv("VK_DRIVER_FILES"); v != "" {
		logger("[CC]   - drivers limited to: %s", v)
	}
	found := 0
	for _, dir := range []string{
		"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d",
		"/usr/local/share/vulkan/icd.d", "/usr/local/etc/vulkan/icd.d",
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			found++
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var manifest struct {
				ICD struct {
					LibraryPath string `json:"library_path"`
				} `json:"ICD"`
			}
			if json.Unmarshal(b, &manifest) != nil || manifest.ICD.LibraryPath == "" {
				logger("[CC]   - %s: unreadable manifest", e.Name())
				continue
			}
			lib := manifest.ICD.LibraryPath
			if !filepath.IsAbs(lib) && strings.Contains(lib, "/") {
				lib = filepath.Join(dir, lib)
			}
			if h, err := purego.Dlopen(lib, purego.RTLD_NOW); err != nil || h == 0 {
				logger("[CC]   - %s: %s FAILS to load: %v", e.Name(), lib, err)
			} else {
				logger("[CC]   - %s: %s loads", e.Name(), lib)
			}
		}
	}
	if found == 0 {
		logger("[CC]   - no driver manifests anywhere: the driver package is not installed. Press the Vulkan driver download on the Closed Captions page.")
	}
}

// logComputeDevices names every device the engine registered, so "which chip is
// actually doing the work" is a line in the log instead of a day of guessing.
// A backend can report itself as Vulkan and run at a third of the processor's
// speed because the device behind it is llvmpipe; without a printed device
// name that is invisible.
func logComputeDevices() {
	n := txDeviceCount()
	for i := int32(0); i < n; i++ {
		var d txBackendDevice
		txDeviceInit(unsafe.Pointer(&d))
		if st := txGetDevice(i, unsafe.Pointer(&d)); st != txOK {
			continue
		}
		desc := txGoString(d.description)
		logger("[CC] compute device %d: %s (%s, %d MB free of %d)",
			i, desc, txGoString(d.kind), d.memoryFree>>20, d.memoryTotal>>20)
		if softwareRenderer(desc) {
			logger("[CC] WARNING: %q is a software renderer, not a graphics card. Transcription on it is slower than the plain processor backend. Check that the compose file passes /dev/dri through and that the Vulkan driver install finished cleanly.", desc)
		}
	}
}

// softwareRenderer spots a Vulkan device that is really the CPU: Mesa's
// llvmpipe/lavapipe and Google's SwiftShader announce themselves in the
// device description.
func softwareRenderer(desc string) bool {
	l := strings.ToLower(desc)
	return strings.Contains(l, "llvmpipe") || strings.Contains(l, "lavapipe") || strings.Contains(l, "swiftshader")
}

// persistShaderCache points the graphics driver's compiled-shader cache into
// the caption directory, which is a bind mount, so shaders compiled on the
// first Vulkan initialization survive the container being rebuilt. A cache
// location already set by hand is respected.
func persistShaderCache() {
	if os.Getenv("MESA_SHADER_CACHE_DIR") != "" {
		return
	}
	dir, err := filepath.Abs(filepath.Join(captionDir, "shader-cache"))
	if err != nil || os.MkdirAll(dir, 0o755) != nil {
		return
	}
	os.Setenv("MESA_SHADER_CACHE_DIR", dir)
	// The older spelling, read by the driver generations that predate the
	// current one.
	os.Setenv("MESA_GLSL_CACHE_DIR", dir)
}

// pinHardwareVulkanICDs points the Vulkan loader at the hardware drivers only.
//
// The Vulkan loader reads driver manifests from the icd.d directories, and
// mesa-vulkan-drivers ships one for llvmpipe — a renderer that runs on the
// processor — next to the Intel and AMD ones. When the hardware driver comes
// up empty (no /dev/dri in the container, wrong permissions, a half-finished
// install) the loader silently offers llvmpipe instead, and everything
// downstream believes it is on a GPU. Excluding the software manifests up
// front turns that failure into "no Vulkan devices", which the engine answers
// by using its native CPU backend — the fastest honest option — and the log
// says what happened.
//
// A caller who has set VK_DRIVER_FILES or VK_ICD_FILENAMES themselves is
// assumed to mean it, and nothing is touched.
func pinHardwareVulkanICDs() {
	if os.Getenv("VK_DRIVER_FILES") != "" || os.Getenv("VK_ICD_FILENAMES") != "" {
		return
	}
	var hardware []string
	sawSoftware := false
	for _, dir := range []string{
		"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d",
		"/usr/local/share/vulkan/icd.d", "/usr/local/etc/vulkan/icd.d",
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			// Mesa names the llvmpipe manifest lvp_icd.<arch>.json. gfxstream
			// and virtio are paravirtual drivers for guests of a VM — on real
			// hardware they offer nothing and have been seen to get in the
			// way of the instance the real driver needs.
			l := strings.ToLower(e.Name())
			if strings.Contains(l, "lvp") || strings.Contains(l, "llvmpipe") || strings.Contains(l, "swiftshader") ||
				strings.Contains(l, "gfxstream") || strings.Contains(l, "virtio") {
				sawSoftware = true
				continue
			}
			hardware = append(hardware, filepath.Join(dir, e.Name()))
		}
	}
	if !sawSoftware {
		return
	}
	list := strings.Join(hardware, ":")
	// Both names: VK_DRIVER_FILES is the current loader's spelling,
	// VK_ICD_FILENAMES the one older loaders read.
	os.Setenv("VK_DRIVER_FILES", list)
	os.Setenv("VK_ICD_FILENAMES", list)
	if len(hardware) == 0 {
		logger("[CC] The only Vulkan driver here is a software renderer, which is slower than using the processor directly. Ignoring it; captions will run on the processor. Pass /dev/dri through in the compose file and reinstall the driver to use the GPU.")
	} else {
		logger("[CC] Vulkan drivers limited to the hardware ones: %s", list)
	}
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
		{"backend device", txABIBackendDevice, unsafe.Sizeof(txBackendDevice{})},
		{"token", txABIToken, unsafe.Sizeof(txToken{})},
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
	// Nothing chooses a backend while the answer is still changing.
	//
	// The driver restore runs behind the quiet gate, so on a fresh container
	// there is a stretch where the Vulkan loader is not installed yet and the
	// honest answer to "can this machine run the GPU build" is no. A stream
	// that asked during that stretch got the processor — and then loaded the
	// model strictly on the processor, and every stream after it shared that
	// copy, because a loaded model is keyed by the backend it was loaded for.
	// One early tune therefore put the whole container on the processor for
	// the life of the process, with eight threads of a phrase model on it, and
	// nothing said so: the log line reads "using cpu backend" whether that was
	// chosen or merely settled for.
	//
	// Waiting costs this stream some of its first minute of captions and
	// nothing else. It is not on the tune path — the video has been flowing
	// for a second by the time anything here runs, the caller retries while it
	// plays, and the alternative is a processor pinned until somebody notices
	// the heat.
	awaitDriverRestore()
	variant := currentEngineVariant()
	// Say it when the machine has a graphics device and is about to use its
	// processor anyway. That is either a driver that has not arrived or a
	// build that was never downloaded, and both are fixable from the page —
	// but only by somebody who has been told.
	if variant == "cpu" && len(renderNodes()) > 0 {
		logger("[CC] WARNING: this container has a graphics device but no GPU build it can load, so %s will run on the processor. "+
			"That is several times the work and all of it heat. Check the Closed Captions page: the driver, and the GPU build of the engine.", m.Name)
	}
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

// A recognition that never returns would take the captions with it: the
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

// The abort watchdog is off unless asked for. Installing the callback has the
// engine poll it between decode steps, and every poll re-enters Go from the
// engine's compute thread — a tax paid per token, on every backend, for the
// whole life of the process — a measurable fraction of the recognizer's
// speed. A wedged decode without the watchdog costs one worker until restart —
// the reply deadline keeps every stream alive and pressure brings up a second
// copy — which is a fair price for full speed the rest of the time.
// CC_WATCHDOG=1 turns the polling back on.
var withWatchdog = os.Getenv("CC_WATCHDOG") == "1"

// maxBatchAudioSec bounds how much audio one dispatch may carry. Compute time
// follows audio length, and the run deadline is fixed: a batch allowed to grow
// without limit under backlog was the one path left where a single call could
// outgrow its deadline, fail wholesale, and spike the processor long enough to
// trouble the streams. Anything past the cap simply waits for the next
// dispatch, which leaves immediately.
const maxBatchAudioSec = 20.0

func (t *transcribeModel) enterGPU() bool {
	if !t.onGPU {
		return false
	}
	// Bounded: a decode that wedged while holding a slot must not turn the
	// gate into a plug for every session that comes after it. Past the
	// deadline the slot is presumed lost and the work proceeds without one —
	// oversubscribing the device beats freezing every stream on it.
	select {
	case gpuGate <- struct{}{}:
		return true
	case <-time.After(txRunDeadline + 5*time.Second):
		logger("[CC] a GPU slot was not released in time; proceeding without one")
		return false
	}
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
	// at is when the phrase was handed over. The gap between this and the
	// moment it is transcribed is the only honest answer to "is the recognizer
	// keeping up": it needs no assumption about how much of a stream is speech,
	// and it does not change meaning when the stream count does.
	at time.Time
}

type txBatchReply struct {
	text string
	err  error
}

// txBatchService is the shared recognizer for a phrase-at-a-time model: one
// request queue, served by one worker that owns the one copy of the weights
// and its session. The engine permits one run in flight per copy, and one
// copy is all there is: when the worker cannot keep pace, the freshness rules
// thin the phrases and the telemetry says which backend would do better.
type txBatchService struct {
	path    string
	backend int32
	// lang mirrors transcribeModel.lang: NUL-terminated, heap-resident.
	lang  []byte
	onGPU bool
	// kvType is the attention cache precision this model asked for, and pnc and
	// itn its punctuation and number formatting.
	kvType   int32
	pnc      int32
	itn      int32
	requests chan txBatchRequest
	refs     int
	closed   chan struct{}
	// ready is closed once the service is usable (or failed, with err set).
	// Streams that arrive while the weights are still loading wait on it
	// rather than loading their own copy, which is the entire point.
	ready chan struct{}
	err   error

	// Telemetry.
	telMu       sync.Mutex
	tDispatches int64
	tLag        time.Duration
	// tWinN is dispatches inside the current window, for the page's average.
	tWinN    int64
	tPhrases int64
	tCompute time.Duration
	tAudio   time.Duration
	// advise-once bookkeeping: the first dispatches decide whether this
	// backend is pulling its weight.
	advN       int64
	advCompute time.Duration
	advAudio   time.Duration
	advised    bool
}

// telemetryWindow is how many dispatches the throughput figure averages over
// before it is published and the counters reset.
//
// A hundred rather than twenty-five, because the page shows this continuously
// now and the log only has to leave a trail. At twenty-five, three captioned
// streams produced a line every half minute reporting a number that had not
// moved.
const telemetryWindow = 100

// recognizerReport is the measured throughput of the shared recognizer, in the
// form the page asks about it: how much faster than real time it is running, and
// therefore roughly how many captioned tuners it will carry.
//
// Measured, never estimated. It depends on the model, the quantization, the
// backend and the machine, and there is no figure that is true of everybody's —
// so the page shows this one or shows nothing.
type recognizerReport struct {
	Measured bool `json:"measured"`
	// Streaming says which path took the measurement, because the two paths
	// can answer different questions. A batch recognizer has a queue, so it
	// can say how long a phrase waited; a continuous one has no queue and
	// nothing to say about waiting. Reporting a wait of zero for it would
	// read as "keeping up perfectly" when it is really "not a thing here".
	Streaming bool    `json:"streaming"`
	Speed     float64 `json:"speed"`
	Phrases   float64 `json:"phrases"`
	Backend   string  `json:"backend"`
	AgeSec    int     `json:"ageSec"`
	// Streams is how many captioned streams were feeding this recognizer when
	// the figure was taken. Reported, never used to derive anything: measuring
	// four streams at the same speed as one is what proved the old arithmetic
	// wrong, and the figure is here so that comparison stays possible.
	Streams int `json:"streams"`
	// LagSec is how long a phrase waited between being cut and being
	// transcribed, averaged. This is the number that answers "is it keeping
	// up", and unlike a stream count it is measured rather than modelled.
	LagSec float64 `json:"lagSec"`
	// Waiting is how many phrases were in the queue at the last dispatch.
	Waiting int `json:"waiting"`
}

var recogStat struct {
	sync.Mutex
	r  recognizerReport
	at time.Time
}

// noteRecognizerSpeed publishes the latest measurement for the page.
func noteRecognizerSpeed(r recognizerReport) {
	r.Measured = true
	recogStat.Lock()
	recogStat.r = r
	recogStat.at = time.Now()
	recogStat.Unlock()
}

// recognizerSnapshot is the last measurement, with its age.
//
// The age is shown rather than hidden because a stale figure and a fresh one
// mean different things: one recognizer that stopped being asked anything an
// hour ago is not evidence about what is happening now.
func recognizerSnapshot() recognizerReport {
	recogStat.Lock()
	defer recogStat.Unlock()
	r := recogStat.r
	if r.Measured {
		r.AgeSec = int(time.Since(recogStat.at).Seconds())
	}
	return r
}

// txToken is one token of a result, laid out to match transcribe.h. Only p is
// read; the rest are here because the struct has to be the right size and the
// library writes all of it.
//
// The size is checked against the engine's own answer at startup, with every
// other struct in this file. A layout that has drifted is caught by a message
// naming the release rather than by writing over the stack.
type txToken struct {
	structSize uint64
	id         int32
	p          float32
	t0ms       int64
	t1ms       int64
	segIndex   int32
	wordIndex  int32
	text       *byte
}

// tokenConfidence averages the engine's per-token probabilities for one result.
//
// This is the engine's own number and not a heuristic of ours. From
// transcribe.h, in as many words:
//
//	"transcribe_token::p is the per-token probability when the architecture
//	produces one, or NaN when it does not. ... callers should treat it as a
//	confidence hint, not a calibrated probability."
//
// A hint is what it is used as. The question being asked of it is not "how sure
// is the model" in any absolute sense — it is whether a phrase the model
// invented over a stretch of music scores lower than a phrase somebody actually
// said, on this audio, on this machine. That is a question about the separation
// between two piles of numbers and not about any one of them, which is why this
// only reports for now and decides nothing.
//
// NaN means the family publishes no probability, and one NaN is taken as the
// answer for the whole result: a family either produces these or it does not,
// and averaging the ones that are not NaN would invent a figure out of whichever
// tokens happened to have one. Not-ok means no opinion, and no opinion must
// never become a reason to drop a caption.
//
// Split from the reading of it so it can be tested without an engine, which is
// the rule for anything that will end up deciding whether text is thrown away.
func tokenConfidence(n int, at func(int) (float32, bool)) (mean float64, counted int, ok bool) {
	var sum float64
	for i := 0; i < n; i++ {
		p, present := at(i)
		if !present {
			continue
		}
		v := float64(p)
		if math.IsNaN(v) {
			return 0, 0, false
		}
		sum += v
		counted++
	}
	if counted == 0 {
		return 0, 0, false
	}
	return sum / float64(counted), counted, true
}

// confidence reads the tokens of the result sitting in the session. The direct
// call and the batch call fill different accessors, so which one to ask is the
// caller's to say — exactly as it is for the text.
func (w *txWorker) confidence(batchIndex int, batched bool) (float64, int, bool) {
	if txNTokens == nil || txGetToken == nil {
		return 0, 0, false
	}
	read := func(i int) (float32, bool) {
		t := txToken{structSize: uint64(unsafe.Sizeof(txToken{}))}
		var st int32
		if batched {
			st = txBatchGetToken(w.session, int32(batchIndex), int32(i), unsafe.Pointer(&t))
		} else {
			st = txGetToken(w.session, int32(i), unsafe.Pointer(&t))
		}
		// A row that is not there comes back with text NULL rather than an
		// error, which is the header's own way of saying "no such row".
		if st != txOK || t.text == nil {
			return 0, false
		}
		return t.p, true
	}
	if batched {
		return tokenConfidence(int(txBatchNTokens(w.session, int32(batchIndex))), read)
	}
	return tokenConfidence(int(txNTokens(w.session)), read)
}

// The per-phrase measurement that was here has been taken out, having answered
// both of its questions with a no.
//
// The engine's confidence was one of them. transcribe_token::p is documented as
// NaN when the architecture does not produce a probability, and Cohere does not:
// every phrase printed "confidence none". The binding above stays because it is
// the engine's own accessor and another family may well fill it in, but nothing
// here can be built on it today.
//
// Level was the other, and it failed more interestingly. The idea was that a
// quiet music bed transcribed as lyrics would sit below ordinary speech in rms
// or crest. It does not. A commercial's sung vocals measured rms 0.019 to 0.033
// at crest 3.7 to 6.9, and the dialogue either side of them measured rms 0.017
// to 0.039 at crest 3.3 to 8.4 — the same audio by every number available here.
// Singing is speech, mixed and mastered to the same loudness as speech, because
// a broadcaster wants both heard.
//
// So neither number separates them, and printing a line per phrase to keep
// looking was noise. What would separate them is something that measures
// spectral shape over time rather than amplitude — the 4 Hz syllable-rate
// modulation speech has and music does not — or a trained voice-activity model.
// Both were considered and set aside; the choice sits with whoever picks it up.

// txWorker is one copy of the weights and the session that runs it.
type txWorker struct {
	shared  *sharedTxModel
	session uintptr
	abort   *txAbortHandle
}

var (
	txServiceLock sync.Mutex
	txServices    = map[string]*txBatchService{}
)

// makeWorker loads a copy of the weights and opens a session on it.
func (svc *txBatchService) makeWorker(alive func() bool) (*txWorker, error) {
	shared, key, err := acquireTxModel(svc.path, svc.backend, alive)
	if err != nil {
		return nil, err
	}
	_ = key
	sp := txSessionParams{}
	txSessionParamsInit(unsafe.Pointer(&sp))
	// The one worker gets the full compute allowance on the processor. On a
	// GPU backend it gets a fraction of it: the GPU does the arithmetic, the
	// CPU threads exist for the frontend and the handoffs — and ggml spin-
	// waits every one of them while the GPU computes, so each extra thread
	// is a core burned idling. Eight spinning threads beside a tune's
	// playback checks was part of how captions cost a recording.
	threads := captionComputeThreads()
	if svc.onGPU {
		threads = captionGPUThreads()
	}
	sp.nThreads = int32(threads)
	sp.kvType = svc.kvType
	// Asked of the weights now they are loaded, once, rather than per call.
	svc.pnc, svc.itn = supportedToggles(shared.handle, quirksFor(mustFindModel(currentCaptionConfig().Model)))
	// The decoder window stays at the model's own maximum. The engine's header
	// warns that for families where audio tokens share the decoder window,
	// capping it constrains the run — and capping it was another unmeasured
	// guard on the model's throat.
	var session uintptr
	if st := txSessionInit(shared.handle, unsafe.Pointer(&sp), unsafe.Pointer(&session)); st != txOK || session == 0 {
		releaseTxModel(svc.path+"|"+fmt.Sprint(svc.backend), shared)
		return nil, fmt.Errorf("opening a session: %s", txStatusString(st))
	}
	w := &txWorker{shared: shared, session: session, abort: &txAbortHandle{}}
	flags := ""
	if withWatchdog {
		txSetAbortCallback(session, txAbortCallback(), unsafe.Pointer(&w.abort.deadlineUnixNano))
		flags = ", watchdog on"
	}
	logger("[CC] recognizer worker up: %s, %s backend, %d threads, decoder window at the model's own default%s",
		filepath.Base(svc.path), txBackendName(svc.backend), threads, flags)
	return w, nil
}

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
	svc := &txBatchService{
		path:     path,
		backend:  backend,
		kvType:   quirksFor(mustFindModel(cfg.Model)).KVType,
		onGPU:    backend != txBackendCPU,
		requests: make(chan txBatchRequest, 32),
		refs:     1,
		closed:   make(chan struct{}),
		ready:    make(chan struct{}),
	}
	if l := cfg.Language; l != "" && l != "auto" {
		svc.lang = append([]byte(l), 0)
	}
	txServices[key] = svc
	txServiceLock.Unlock()

	go func() {
		defer close(svc.ready)
		w, err := svc.makeWorker(alive)
		if err != nil {
			svc.err = err
			txServiceLock.Lock()
			delete(txServices, key)
			txServiceLock.Unlock()
			return
		}
		go svc.run(w)
		// Every stream that wanted this may have timed out and moved on
		// while the load ran. A live service nobody references would hold
		// the weights until restart; reap it here, and a later stream simply
		// creates a fresh one.
		txServiceLock.Lock()
		if svc.refs <= 0 {
			delete(txServices, key)
			close(svc.closed)
			logger("[CC] The shared model finished loading after every stream stopped waiting; releasing it")
		}
		txServiceLock.Unlock()
	}()

	select {
	case <-svc.ready:
	case <-time.After(150 * time.Second):
		txServiceLock.Lock()
		svc.refs--
		txServiceLock.Unlock()
		return nil, fmt.Errorf("the shared model did not finish loading in time")
	}
	if svc.err != nil {
		return nil, svc.err
	}
	return svc, nil
}

// release drops one stream's claim; the last closes the service and its workers.
func (svc *txBatchService) release() {
	txServiceLock.Lock()
	defer txServiceLock.Unlock()
	if svc.refs--; svc.refs > 0 {
		return
	}
	delete(txServices, svc.path+"|"+fmt.Sprint(svc.backend))
	close(svc.closed)
}

// One recognizer, and that is the measured answer rather than a simplification.
//
// This was a setting on the page, up to eight copies of the weights, and it made
// transcription slower every time it was raised — not slower per copy, slower
// outright, further behind real time with four than with one. Three reasons, all
// of them pushing the same way:
//
// Batching is the first and the largest. The engine runs several phrases in one
// dispatch far faster than the same phrases one at a time, and the log has been
// printing both numbers side by side since the telemetry went in. Splitting the
// queue across workers meant nothing ever batched.
//
// Threads are the second. captionComputeThreads is a figure for the machine, not
// for a worker, and it was never divided — so every copy took the full
// allowance. Four copies asked for four times the machine's cores, and ggml
// spin-waits its threads rather than sleeping them, so they did not politely
// interleave: they fought each other, and the tuners, for the same cores.
//
// The device is the third. One graphics chip does one piece of arithmetic at a
// time whatever is queued on it, and each copy holds its own two gigabytes of
// weights in the same system memory the iGPU reads through. Two copies buy no
// parallelism and double the traffic through the bottleneck.
//
// So the queue has one server, it takes everything waiting, and it batches.
func (svc *txBatchService) run(w *txWorker) {
	defer func() {
		if w.session != 0 {
			txSessionFree(w.session)
		}
		releaseTxModel(svc.path+"|"+fmt.Sprint(svc.backend), w.shared)
		logger("[CC] The recognizer released its copy of the weights")
	}()
	for {
		var first txBatchRequest
		select {
		case <-svc.closed:
			return
		case first = <-svc.requests:
		}
		// Inference stops dead while any tune has yet to deliver video. A
		// softer version — small dispatches with breathers — starves a slow
		// device's 40-second playback confirmation into a timed-out tune.
		// Captions pause for the pending window and the
		// freshness ceiling snaps them back to live afterward; a recording
		// is never the thing that pays.
		audioCap := maxBatchAudioSec
		// Polled finely, not every two seconds. Yielding to the tune is the
		// point and that does not change; what changed is how long the
		// recognizer sits idle after the tune has already finished. On a
		// ten-tuner box the DVR is starting something often, and each hold
		// used to end up to two seconds after there was anything to hold for
		// — added to every phrase queued behind it. The check is a lock and a
		// slice walk, so asking eight times as often costs nothing worth
		// measuring against the second and a half it gives back.
		for tunesPending() {
			select {
			case <-svc.closed:
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
		batch := []txBatchRequest{first}
		audioSec := float64(len(first.pcm)) / asrSampleRate
		// Take everything already waiting; wait for nothing that is not.
		//
		// There was a hundred and fifty millisecond gather here, holding the
		// door open in case another stream was about to cut a phrase. The
		// measurements say it was not worth the wait: one and a tenth phrases
		// to a dispatch with three streams running, so nearly every gather
		// waited the full time and got nobody, and paid for it on the one
		// phrase it did have. A wait that usually comes back empty is a
		// latency charge on every phrase in exchange for an occasional saving
		// on one.
		//
		// Nothing is given up by dropping it. Streams cut phrases on their own
		// clocks, and when those clocks do line up the requests are already in
		// the channel — the loop below takes them, in the same dispatch, at no
		// cost. What is gone is only the speculative part: waiting to find out
		// whether somebody might be about to speak.
		_ = audioCap
		// One worker takes everything waiting, deliberately.
		//
		// There was rationing here — a worker stopped drawing once what was
		// left would be one apiece for the others — written when the answer to
		// a slow recognizer was thought to be more of them. It was the wrong
		// answer and this was how it did its damage: batching several phrases
		// into one dispatch is the single biggest speed-up the engine offers,
		// and holding phrases back for other workers turned every batched
		// dispatch into a solo one. The log printed both figures side by side
		// the whole time.
		for len(batch) < 16 && audioSec < maxBatchAudioSec && len(svc.requests) > 0 {
			select {
			case r := <-svc.requests:
				batch = append(batch, r)
				audioSec += float64(len(r.pcm)) / asrSampleRate
			default:
				goto ready
			}
		}
	ready:
		svc.dispatch(w, batch)
	}
}

func (svc *txBatchService) dispatch(w *txWorker, batch []txBatchRequest) {
	ptrs := make([]unsafe.Pointer, len(batch))
	lens := make([]int32, len(batch))
	for i, r := range batch {
		ptrs[i] = unsafe.Pointer(&r.pcm[0])
		lens[i] = int32(len(r.pcm))
	}
	p := txRunParams{}
	txRunParamsInit(unsafe.Pointer(&p))
	p.timestamps = txTimestampsNone
	p.pnc, p.itn = svc.pnc, svc.itn
	if len(svc.lang) > 0 {
		p.language = &svc.lang[0]
	}

	w.shared.compute.Lock()
	held := false
	if svc.onGPU {
		gpuGate <- struct{}{}
		held = true
	}
	atomic.StoreInt64(&w.abort.deadlineUnixNano, time.Now().Add(txRunDeadline).UnixNano())
	began := time.Now()
	// A single phrase goes through the plain run call. The batch entry point
	// earns its keep only with company: the engine runs each utterance's
	// encoder serially and shares only the decode loop, so batch-of-one is
	// the direct call's work routed through a driver tuned for lockstep
	// decoding — overhead with nothing bought. The direct path is the one the
	// family tunes for a lone utterance.
	var st int32
	if len(batch) == 1 {
		st = txRun(w.session, ptrs[0], lens[0], unsafe.Pointer(&p))
	} else {
		st = txRunBatch(w.session, unsafe.Pointer(&ptrs[0]), unsafe.Pointer(&lens[0]), int32(len(batch)), unsafe.Pointer(&p))
	}
	compute := time.Since(began)
	atomic.StoreInt64(&w.abort.deadlineUnixNano, 0)
	if held {
		<-gpuGate
	}
	w.shared.compute.Unlock()
	for _, r := range batch {
		runtime.KeepAlive(r.pcm)
	}
	runtime.KeepAlive(svc.lang)
	runtime.KeepAlive(w.abort)

	if st != txOK {
		err := fmt.Errorf("%s", txStatusString(st))
		for _, r := range batch {
			r.reply <- txBatchReply{err: err}
		}
		return
	}
	if len(batch) == 1 {
		// The direct call fills the single-result accessors, not the batch ones.
		text := cleanRecognized(txFullText(w.session))
		batch[0].reply <- txBatchReply{text: text}
	} else {
		nres := int(txBatchNResults(w.session))
		for i, r := range batch {
			if i >= nres {
				r.reply <- txBatchReply{err: fmt.Errorf("no result for this phrase")}
				continue
			}
			if pst := txBatchStatus(w.session, int32(i)); pst != txOK {
				r.reply <- txBatchReply{err: fmt.Errorf("%s", txStatusString(pst))}
				continue
			}
			text := cleanRecognized(txBatchFullText(w.session, int32(i)))
			r.reply <- txBatchReply{text: text}
		}
	}

	var audio time.Duration
	for _, l := range lens {
		audio += time.Duration(float64(l) / asrSampleRate * float64(time.Second))
	}
	var lag time.Duration
	for _, r := range batch {
		if !r.at.IsZero() {
			lag += began.Sub(r.at)
		}
	}
	waiting := len(svc.requests)
	svc.telMu.Lock()
	svc.tLag += lag / time.Duration(len(batch))
	if !svc.advised && svc.backend != txBackendCPU {
		svc.advN++
		svc.advCompute += compute
		svc.advAudio += audio
		if svc.advN == 15 {
			svc.advised = true
			if speed := float64(svc.advAudio) / float64(svc.advCompute); speed < 1.5 {
				logger("[CC] The %s backend is managing only %.1fx real time on this machine after %d dispatches. "+
					"Some integrated GPUs are slower than their own processor for this model — pin the CPU build "+
					"on the Closed Captions page and compare this line. Whichever reads higher is the right setting here.",
					txBackendName(svc.backend), speed, svc.advN)
			}
		}
	}
	svc.tDispatches++
	svc.tWinN++
	svc.tPhrases += int64(len(batch))
	svc.tCompute += compute
	svc.tAudio += audio
	// The page is published every dispatch, from the window so far.
	//
	// It was published at the window boundary, which meant the page showed
	// nothing at all until a hundred dispatches had gone by — several minutes on
	// one stream — and showing nothing is what "I put it on the page" turned out
	// to mean in practice. The log wants a settled average over a long window;
	// the page wants the current answer. They are different needs and they now
	// have different cadences instead of sharing the log's.
	txServiceLock.Lock()
	streams := svc.refs
	txServiceLock.Unlock()
	if svc.tCompute > 0 {
		noteRecognizerSpeed(recognizerReport{
			Speed:   float64(svc.tAudio) / float64(svc.tCompute),
			Phrases: float64(svc.tPhrases) / float64(svc.tWinN),
			Backend: txBackendName(svc.backend),
			Streams: streams,
			LagSec:  svc.tLag.Seconds() / float64(svc.tWinN),
			Waiting: waiting,
		})
	}
	if svc.tDispatches%telemetryWindow == 0 {
		speed := float64(svc.tAudio) / float64(svc.tCompute)
		// What this number is, and what it is not.
		//
		// It is the real-time factor: seconds of audio transcribed per second of
		// compute. It is a property of the model, the quantization, the backend
		// and the machine, and it is measured here rather than looked up.
		//
		// It said "about N streams' worth" as well, on the reasoning that a
		// captioned stream makes a second of audio for every second it runs, so a
		// recognizer at N times real time carries about N streams. That was
		// arithmetic on an assumption nobody had checked, and it is wrong twice
		// over. A stream does not submit a second of audio a second — only speech
		// is ever queued, and television has pauses, music and stretches with
		// nobody talking. And the factor itself does not move with the stream
		// count: four streams measured the same 4.6x as one, which is exactly
		// what should have been expected of a figure that is per second of
		// compute, and which no derived stream count survives.
		//
		// So the stream count is gone and the wait is here instead. How long a
		// phrase sits between being cut and being transcribed is the actual
		// question — it needs no assumption about how much of a broadcast is
		// speech, and it says the same thing whatever the stream count is. Under
		// a second is keeping up. Climbing is not.
		logger("[CC] recognizer: %.1fx real time, %.1f phrases per dispatch, %.2fs compute for %.1fs of audio, "+
			"phrases waited %.2fs, %d in the queue, %d %s captioned — over the last %d dispatches",
			speed, float64(svc.tPhrases)/telemetryWindow,
			svc.tCompute.Seconds()/telemetryWindow, svc.tAudio.Seconds()/telemetryWindow,
			svc.tLag.Seconds()/telemetryWindow, waiting, streams, plural(int64(streams), "stream", "streams"),
			telemetryWindow)
		svc.tPhrases, svc.tCompute, svc.tAudio, svc.tWinN, svc.tLag = 0, 0, 0, 0, 0
	}
	svc.telMu.Unlock()
}

// batchClient is the per-stream face of the shared service. It satisfies
// recognizer; the streaming entry points refuse, which sends the caption
// engine down the phrase-at-a-time path — the only path these models have.
type batchClient struct {
	svc *txBatchService
	// stop is the owning engine's closed channel. A teardown must not sit
	// out a reply that a tune hold is delaying — that was a circle: the
	// close waited on the reply, the reply waited on the quiet gate, and
	// the gate waited on the tune the close was making way for.
	stop <-chan struct{}
}

func (b *batchClient) transcribe(pcm []float32) (string, error) {
	if len(pcm) < asrSampleRate/4 {
		return "", nil
	}
	reply := make(chan txBatchReply, 1)
	select {
	case b.svc.requests <- txBatchRequest{pcm: pcm, reply: reply, at: time.Now()}:
	default:
		return "", fmt.Errorf("the shared recognizer is full; this phrase is dropped")
	}
	select {
	case r := <-reply:
		return r.text, r.err
	case <-b.stop:
		return "", fmt.Errorf("stream is closing")
	case <-time.After(txRunDeadline + 8*time.Second):
		// The service dispatch is itself bounded by the run deadline, so this
		// only fires if the service has died with the request in hand. Waiting
		// forever here would freeze this stream's recognizer for the tune.
		return "", fmt.Errorf("the shared recognizer did not answer")
	}
}

// submit queues a phrase and returns the channel its reply will arrive on, so
// a stream can keep more than one phrase in flight and the batch call can
// actually batch. A nil channel with nil error means the clip was too short.
func (b *batchClient) submit(pcm []float32) (<-chan txBatchReply, error) {
	if len(pcm) < asrSampleRate/4 {
		return nil, nil
	}
	reply := make(chan txBatchReply, 1)
	select {
	case b.svc.requests <- txBatchRequest{pcm: pcm, reply: reply, at: time.Now()}:
		return reply, nil
	default:
		return nil, fmt.Errorf("the shared recognizer is full")
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
// The budget is longer than anything the open legitimately waits on, which is
// the whole trick to setting it. It was sixty seconds while the open itself
// waited up to ninety for the driver restore, so a restore that took its time
// did not delay captions — it failed them, every attempt, because the caller
// gave up thirty seconds before the thing it was waiting for could possibly
// have finished. A deadline shorter than the work it bounds is not a deadline,
// it is a guarantee of failure.
//
// Three minutes covers the driver restore's two, plus the open itself. Nothing
// is on the tune path here: this runs on a goroutine, a second after the
// stream's video is already flowing, and the caller retries while the stream
// plays. What it costs is captions arriving late on the first tune of a fresh
// container. What it buys is their arriving at all.
func initTranscribeDeadline(variant string) error {
	var err error
	ok, _ := runWithDeadline(3*time.Minute, "loading the speech engine", func() { err = initTranscribe(variant) })
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
	//
	// The file is warmed into the page cache first, in a loop that yields to
	// tunes; then the short native load runs only on a quiet machine. Neither
	// step fights a tune, ever: a tune is a recording and captions are
	// decoration, so when the machine will not go quiet this attempt fails
	// fast instead — the stream runs on uncaptioned and the caller's retry
	// loop tries again at the next lull. Forcing the load through on a timer
	// would put it inside exactly the window this exists to avoid.
	if !prewarmModelFile(path) {
		return nil, "", fmt.Errorf("gave up warming %s while tunes kept starting; will retry at a quiet moment", filepath.Base(path))
	}
	if !waitTuneQuiet(30 * time.Second) {
		return nil, "", fmt.Errorf("a tune has been starting the whole wait; will retry at a quiet moment")
	}
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

	// Freed outside the lock for the same reason it is loaded outside it —
	// and off any caller's path, behind the tune gate: freeing gigabytes of
	// weights (and a GPU's copy of them) is heavy work, and the last stream
	// ends at exactly the moment a channel change begins a new tune.
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for tunesPending() && time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
		}
		txModelFree(handle)
		logger("[CC] Freed %s (%d in memory)", filepath.Base(strings.SplitN(key, "|", 2)[0]), n)
	}()
}

// txLogCallback filters what the engine has to say down to what a person
// running a TV proxy would want to see: warnings and errors, nothing else.
// Chatter about buffer allocations is the engine's business.
var (
	txLogOnce sync.Once
	txLogPtr  uintptr

	txPerfLastSample int64
	txPerfWindowEnd  int64
)

// txPerfSampleOpen opens a two-second pass-through window for the engine's
// debug lines every thirty seconds, so the per-stage timing breakdown the
// engine prints per phrase lands in the log as a periodic sample rather than
// as either a firehose or silence. Races here cost at most a few extra lines.
func txPerfSampleOpen() bool {
	now := time.Now().UnixNano()
	if now < atomic.LoadInt64(&txPerfWindowEnd) {
		return true
	}
	last := atomic.LoadInt64(&txPerfLastSample)
	if now-last < int64(30*time.Second) {
		return false
	}
	if atomic.CompareAndSwapInt64(&txPerfLastSample, last, now) {
		atomic.StoreInt64(&txPerfWindowEnd, now+int64(2*time.Second))
		return true
	}
	return false
}

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
			// dropped where they are made rather than filtered later — except
			// during a sampling window: the engine's per-stage timing
			// breakdown arrives as DEBUG lines, and a sample of it every
			// half minute is how "which stage is slow" stays a fact in the
			// log instead of a day of guessing.
			if level != 2 && level != 3 && !txPerfSampleOpen() {
				return
			}
			text := strings.TrimSpace(txGoString(msg))
			// The sample window is for the per-stage timing breakdown; the
			// engine's routine allocation notes ride the same level and say
			// nothing anyone acts on.
			if level != 2 && level != 3 && strings.Contains(text, "kv_cache") {
				return
			}
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
	// pnc and itn are this model's punctuation and number-formatting choices.
	pnc int32
	itn int32
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
	// heldGPU is set between arm and disarm while this stream holds a place on
	// the accelerator. Only the goroutine inside a call touches it.
	heldGPU bool
	// backend is what this session opened on, kept so the continuous path can
	// name it the way the batch path names svc.backend.
	backend int32
	// telAudio and telCompute are the continuous path's throughput: seconds of
	// audio fed against seconds of compute spent on it. Guarded by mu, which
	// every caller already holds. telSince is the audio since the last time
	// the figure was published.
	telAudio, telCompute, telSince time.Duration
}

// How much audio goes by between publications, and how much is kept in the
// average. A feed is a fraction of a second and nobody reads the page that
// fast, so the figure is published about once a second from a window of half a
// minute.
const (
	streamTelPublish = time.Second
	streamTelWindow  = 30 * time.Second
)

// noteStreamCompute accumulates what the continuous path spent and publishes it
// on a cadence. The caller holds mu.
//
// This exists because the figure was only ever taken on the batch path, so the
// one model that transcribes a phrase at a time had a speed on the page and the
// models that transcribe continuously showed nothing at all. It is the same
// quantity either way — seconds of audio per second of compute — and it is the
// figure that says whether this machine can keep up, which is if anything more
// pressing for a continuous model: a batch that falls behind grows a queue that
// can be seen, and a stream that falls behind just drifts.
func (t *transcribeModel) noteStreamCompute(audio, compute time.Duration) {
	t.telAudio += audio
	t.telCompute += compute
	t.telSince += audio
	if t.telSince < streamTelPublish || t.telCompute <= 0 {
		return
	}
	t.telSince = 0
	noteRecognizerSpeed(recognizerReport{
		Streaming: true,
		Speed:     float64(t.telAudio) / float64(t.telCompute),
		Backend:   txBackendName(t.backend),
	})
	if t.telAudio > streamTelWindow {
		// Halved rather than cleared, so the window keeps sliding without the
		// figure jumping at the boundary: both sides are scaled by the same
		// amount and the ratio is exactly what it was.
		t.telAudio /= 2
		t.telCompute /= 2
	}
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

	backend := txBackend(variant)
	shared, key, err := acquireTxModel(weights, backend, alive)
	if err != nil {
		return nil, err
	}

	sp := txSessionParams{}
	txSessionParamsInit(unsafe.Pointer(&sp))
	// The same rule the shared recognizer follows, because the reason for it
	// is the backend and not which path opened the session. This one is
	// per-stream — a streaming model keeps its own copy — so the allowance it
	// starts from is already divided among the streams that may caption at
	// once, and the GPU share is taken from that. Left alone, every one of
	// those sessions would have opened a processor's worth of threads to spin
	// while the graphics chip did the arithmetic.
	streamThreads := captionThreads(cfg)
	if variant != "cpu" {
		streamThreads = gpuThreadShare(streamThreads)
	}
	sp.nThreads = int32(streamThreads)
	sp.kvType = quirksFor(m).KVType
	var session uintptr
	if st := txSessionInit(shared.handle, unsafe.Pointer(&sp), unsafe.Pointer(&session)); st != txOK || session == 0 {
		releaseTxModel(key, shared)
		return nil, fmt.Errorf("opening a session: %s", txStatusString(st))
	}

	if txModelBackend != nil {
		got := txModelBackend(shared.handle)
		want := ""
		switch {
		case strings.Contains(variant, "vulkan"):
			want = "vulkan"
		case strings.Contains(variant, "cuda"):
			want = "cuda"
		}
		if want != "" && !strings.Contains(strings.ToLower(got), want) {
			logger("[CC] WARNING: asked for the %s backend and the engine is using %s instead. The %s module did not load — check the driver inside the container, and /dev/dri for Vulkan.",
				want, got, want)
		} else {
			logger("[CC] %s is running on %s", filepath.Base(gguf), got)
		}
	}

	pnc, itn := supportedToggles(shared.handle, quirksFor(m))
	t := &transcribeModel{model: shared.handle, shared: shared, modelKey: key, session: session,
		abort: &txAbortHandle{}, onGPU: variant != "cpu", pnc: pnc, itn: itn, backend: backend}
	if withWatchdog {
		txSetAbortCallback(session, txAbortCallback(), unsafe.Pointer(&t.abort.deadlineUnixNano))
	}
	// "auto" is this page's word for detection, not the engine's: it wants a
	// null language for that, and would reject "auto" as a locale. A family
	// that takes no language parameter at all gets none — it rejects even
	// the right answer.
	if l := cfg.Language; l != "" && l != "auto" && !m.NoLanguage {
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
	p.pnc, p.itn = t.pnc, t.itn
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
	// The same yield the shared service gives: no decode while any tune has
	// yet to deliver video. This path is the fallback when a streaming
	// session will not open; without this it would decode straight through a
	// channel change.
	for tunesPending() {
		time.Sleep(2 * time.Second)
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
	// A null family extension selects the family's own trained defaults,
	// which is the operating point each streaming model prefers.
	rp := t.runParams()
	t.armSetup()
	st := txStreamBegin(t.session, unsafe.Pointer(&rp), unsafe.Pointer(&sp))
	t.disarmSetup()
	runtime.KeepAlive(t.lang)
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
		// Unguarded on purpose: a reopen that only ran when nothing needed
		// reopening would turn a single slow feed into permanent silence —
		// the failure this exists to prevent.
		if err := t.beginStreamLocked(); err != nil {
			return nil
		}
		logger("[CC] continuous recognition recovered after a failed session")
	}
	u := txStreamUpdate{}
	txStreamUpdateInit(unsafe.Pointer(&u))
	t.arm()
	began := time.Now()
	st := txStreamFeed(t.session, unsafe.Pointer(&pcm[0]), int32(len(pcm)), unsafe.Pointer(&u))
	compute := time.Since(began)
	t.disarm()
	runtime.KeepAlive(pcm)
	// Audio only when the feed took it. A failed feed still spent the compute,
	// and counting it against audio that never went through would report a
	// machine as faster than it is — the one direction the figure must not err
	// in, since its whole job is to say when the machine cannot keep up.
	audio := time.Duration(0)
	if st == txOK {
		audio = time.Duration(float64(len(pcm)) / asrSampleRate * float64(time.Second))
	}
	t.noteStreamCompute(audio, compute)
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
	began := time.Now()
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
	// Counted with no audio against it, because that is what it is: real work
	// on the same session that consumes nothing new. Timing only the feeds
	// would leave this out and read high.
	t.noteStreamCompute(0, time.Since(began))
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
	where := currentEngineVariant()
	if where == "cpu" {
		where = "processor"
	}
	logger("[CC] %s captions on: %s, %s, %s, on the %s, ready in %s",
		e.label, m.Key, cfg.Language, mode, where, time.Since(began).Round(time.Millisecond))
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
	// says, a floor under blips rather than a floor under short answers. Thirty
	// five hundredths keeps a one-word reply and still refuses a door closing.
	vadMinSpeech = 0.35
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
				take(e.model.idleFlush())
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
				take(e.model.feedStream(l))
				uttered += float64(len(l)) / asrSampleRate
			}
			lead = nil
			settled = false
		}
		take(e.model.feedStream(buf))
		uttered += chunkSec

		// An utterance cannot run forever: the model decodes each one against
		// a generation budget, and an utterance that outruns it comes back
		// truncated mid-sentence. Close it off at a brief lull once it is
		// eight seconds long, or at twelve seconds wherever the speech is —
		// a seam at a word beats a sentence that ends in nothing.
		if !settled && (uttered >= 12 || (uttered >= 8 && quiet >= 0.2)) {
			take(e.model.idleFlush())
			quiet, settled, uttered = 0, true, 0
			continue
		}

		if loud {
			quiet = 0
			continue
		}
		if quiet += chunkSec; quiet >= flushSilence {
			take(e.model.idleFlush())
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
			silenceRun = 0
		} else {
			pending = nil
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
		heldSec := float64(len(pending)) / asrSampleRate
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
		if len(window) < 2 {
			// Two in flight is the ceiling, so the channel is only listened to
			// with room to accept. Not listening is the backpressure: it was a
			// blocking settle before, which is the same bound reached by
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
				logger("[CC] %s captions are running %.0fs behind (%d phrases dropped)", e.label, lag.Seconds(), drops)
			} else {
				logger("[CC] %s captions are running %.0fs behind (nothing dropped)", e.label, lag.Seconds())
			}
		}
	}
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
func (e *captionEngine) trimOverlap(text string, carried bool) string {
	words := strings.Fields(text)
	if len(words) == 0 || len(e.tail) == 0 || !carried {
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
	RuntimeList    []speechRuntime `json:"runtimeList"`
	RuntimeVersion string          `json:"runtimeVersion"`
	RuntimeURL     string          `json:"runtimeURL"`
	// Engines carries the builds of both engines, keyed by engine, so the page
	// can show what a model would need before it has been saved. Picking a
	// radio button is browsing, not a decision, and must not change what a tune
	// starting right now will do.
	Engines       map[string][]captionStatusEngine `json:"engines"`
	Drivers       []captionStatusDriver            `json:"drivers"`
	Accel         accelReport                      `json:"accel"`
	DriverInstall gpuInstallState                  `json:"driverInstall"`
	// Recognizer is the measured throughput, so the page can answer "will this
	// keep up" without anybody reading a log.
	Recognizer recognizerReport `json:"recognizer"`
	// Speeds is what the page offers for caption speed, in words a minute, and
	// OnScreen what it offers for the least time a line stays readable.
	Speeds   []int     `json:"speeds"`
	OnScreen []float64 `json:"onScreen"`
	// Streaming is how many tuners are busy, so the page can refuse to switch
	// captions on in the middle of a recording.
	Streaming      int    `json:"streaming"`
	Persistent     bool   `json:"persistent"`
	PersistWarning string `json:"persistWarning"`
	Tuners         int    `json:"tuners"`
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
	MemoryMB      int `json:"memoryMB"`
	MemoryTotalMB int `json:"memoryTotalMB"`
	// Windows is the phrase lengths this model offers and Window the one in
	// force. Empty means the page shows no choice, which is every streaming
	// model: there is no phrase to lengthen.
	Windows     []float64 `json:"windows"`
	Window      float64   `json:"window"`
	MemoryTotal string    `json:"memoryTotal"`
	URL         string    `json:"url"`
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
func recommendedModel() (key, why string) {
	// Guidance, never a gate: every model runs anywhere, and the page only
	// says where to start.
	return "cohere-transcribe", "The place to start: the most accurate captioning available, with one copy shared by every tuner. The smaller models below are for machines that cannot keep up with it."
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
// runtime.NumCPU is not that number. It honors the affinity mask but knows
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

// captionComputeThreads is the shared recognizer's allowance: the machine's
// performance cores, one thread per physical core.
//
// More measures worse. ggml synchronizes its workers with spin
// barriers, so every thread waits for the slowest at every step: mix in
// hyperthread siblings and the barriers pay for shared execution units; mix
// in a hybrid chip's efficiency cores and every op finishes at E-core speed.
// The fast configuration was a handful of threads landing on performance
// cores — which is also what leaves the efficiency cores free for the ffmpeg
// decoders and the proxy, the things captions decorate rather than compete
// with.
func captionComputeThreads() int {
	n := performanceCores()
	if reserve := availableCPUs() / 4; n > availableCPUs()-reserve {
		n = availableCPUs() - reserve
	}
	if n < 2 {
		n = 2
	}
	return n
}

// captionGPUThreads is the shared recognizer's allowance when the arithmetic
// is happening on a graphics chip: half of what the processor path would get.
//
// Half, and not a fixed number, because the machine is the thing that decides
// how much of itself it can spare — a four-core box and a thirty-two-core box
// are not both entitled to the same four threads, and neither should be told a
// figure that was measured on somebody else's hardware. It is derived from the
// same probe as the processor path, so a cgroup quota, a hybrid chip's
// efficiency cores and a machine with very few cores are all already accounted
// for by the time this halves it.
//
// Half rather than all of it because on this path the threads are not doing
// the arithmetic. The GPU is; these run the mel frontend and the handoffs, and
// ggml spin-waits every one of them while the GPU computes, so a thread beyond
// what the frontend needs is a core burned idling next to a tune. The floor of
// two is the same floor the processor path has: below that there is nothing to
// share out.
func captionGPUThreads() int {
	return gpuThreadShare(captionComputeThreads())
}

// gpuThreadShare turns a processor thread allowance into the GPU path's share
// of it. One rule, applied wherever a session is opened, so the two places
// that open one cannot drift apart on what a GPU backend is entitled to.
//
// It never raises the figure it is given. The allowance handed in has already
// been divided by whatever else is running — on the per-stream path that is
// the other streams — and half of a small share is still smaller than the
// share, which is the direction this may move it and the only one.
func gpuThreadShare(cpuThreads int) int {
	n := cpuThreads / 2
	if n < 1 {
		n = 1
	}
	if n > cpuThreads {
		n = cpuThreads
	}
	return n
}

// performanceCores counts physical performance cores. On an Intel hybrid chip
// the kernel lists the P-cores' logical CPUs under cpu_core; elsewhere every
// core is a performance core and the count is physical rather than logical.
// Both fall back conservatively: half the logical CPUs.
func performanceCores() int {
	if b, err := os.ReadFile("/sys/devices/cpu_core/cpus"); err == nil {
		if n := countCPUList(strings.TrimSpace(string(b))); n > 0 {
			// The list is logical CPUs; P-cores have two apiece.
			return (n + 1) / 2
		}
	}
	return (availableCPUs() + 1) / 2
}

// countCPUList counts entries in a kernel cpu list like "0-15" or "0-7,16-19".
func countCPUList(s string) int {
	total := 0
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || b < a {
				return 0
			}
			total += b - a + 1
		} else if _, err := strconv.Atoi(part); err == nil {
			total++
		} else {
			return 0
		}
	}
	return total
}

// captionedStreams is how many tuners could be captioning at the same time.
// tunersStreaming counts tuners with a stream on them right now.
//
// The page uses it to refuse to switch captions on mid-stream. Enabling them is
// no longer only a settings write: it is what asks for the graphics driver, and
// that install is a package at a time against whatever is playing. Startup does
// it where nothing can be tuning; asking for it in the middle of a recording is
// the one thing this program is not allowed to make easy.
func tunersStreaming() int {
	tunerLock.Lock()
	defer tunerLock.Unlock()
	n := 0
	for i := range tuners {
		if tuners[i].active {
			n++
		}
	}
	return n
}

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
			Windows:       quirksFor(m).Windows,
			Window:        phraseWindowFor(quirksFor(m), cfg),
			URL:           modelURL(m),
		})
	}
	engineURL, _, _ := engineAsset()
	needed := findSpeechRuntime(neededRuntime())
	curVariant, _ := findEngineVariant(cur)
	recog := recognizerSnapshot()
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
		RuntimeVersion: needed.Version,
		RuntimeURL:     engineURL,
		Recognizer:     recog,
		Speeds:         captionSpeeds,
		OnScreen:       captionOnScreen,
		Streaming:      tunersStreaming(),
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
