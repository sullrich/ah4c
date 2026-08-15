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
func engineUsable(v engineVariant) bool {
	if v.Needs == "" {
		return true
	}
	h, err := purego.Dlopen(v.Needs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil || h == 0 {
		return false
	}
	return true
}

// engineAsset names the engine archive for this build and the library inside
// it. The architecture is decided at compile time, so an arm64 image fetches
// the arm64 build without being told which it is.
func engineAsset() (url, local string, ok bool) {
	return engineAssetFor(runtime.GOOS, runtime.GOARCH, currentEngineVariant())
}

// currentEngineVariant is the configured build, falling back to the processor
// if the chosen one cannot load here.
func currentEngineVariant() string {
	cfg := currentCaptionConfig()
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

// engineLibPath is where the downloaded engine lands.
func engineLibPath() string {
	_, local, ok := engineAsset()
	if !ok {
		return ""
	}
	return filepath.Join(captionRuntime, currentEngineVariant(), local)
}

func engineInstalled() bool {
	p := engineLibPath()
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
	// Hardware what it is comfortable running on. All three are shown on the
	// page, because the choice between these is not really about accuracy.
	Desc      string   `json:"desc"`
	Latency   string   `json:"latency"`
	Hardware  string   `json:"hardware"`
	File      string   `json:"file"`
	SizeMB    int      `json:"sizeMB"`
	Streaming bool     `json:"streaming"`
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
		Key:       "realtime-multilingual",
		Name:      "Nemotron 3.5 Streaming 0.6B (recommended)",
		Desc:      "Transcribes continuously as the audio arrives and writes proper punctuation and sentence case, in any of its languages. This is the one that reads like real captions.",
		Latency:   "Under a second",
		Hardware:  "A modern multi-core CPU. Roughly five times the work of the 120M.",
		File:      "nemotron-3.5-asr-streaming-0.6b-q8_0.gguf",
		SizeMB:    938,
		Streaming: true,
		Languages: euroLanguages,
	},
	{
		Key:       "realtime-120m",
		Name:      "Parakeet Realtime 120M",
		Desc:      "Just as quick and a fifth of the size, for hardware that cannot spare the cores. Writes no punctuation at all: the model produces none, and no setting changes that.",
		Latency:   "Under a second",
		Hardware:  "Runs on almost anything. A low-power NAS, a mini PC or a Raspberry Pi class board is plenty.",
		File:      "realtime_eou_120m-v1-q8_0.gguf",
		SizeMB:    168,
		Streaming: true,
		Languages: []string{"en"},
	},
	{
		Key:       "parakeet-v3",
		Name:      "Parakeet TDT 0.6B v3",
		Desc:      "Waits for a whole phrase and then transcribes it, with punctuation. The most accurate of the multilingual options, at the cost of arriving later.",
		Latency:   "Three to four seconds",
		Hardware:  "A modern multi-core CPU.",
		File:      "tdt-0.6b-v3-q8_0.gguf",
		SizeMB:    897,
		Languages: euroLanguages,
	},
	{
		Key:       "parakeet-110m",
		Name:      "Parakeet TDT-CTC 110M",
		Desc:      "Phrase at a time, English only, with punctuation. A fifth of the size of v3 and the lightest way to get accurate English if latency does not matter.",
		Latency:   "Three to four seconds",
		Hardware:  "Modest hardware. Comfortable on a NAS.",
		File:      "tdt_ctc-110m-q8_0.gguf",
		SizeMB:    170,
		Languages: []string{"en"},
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
		Engine:    "cpu",
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
	return fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", captionModelRepo, m.File)
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

// startRuntimeDownload fetches the parakeet.cpp engine in the background. It is
// the one piece of native code captions need, it is about a megabyte, and it is
// pulled on demand rather than shipped in the image.
func startRuntimeDownload(variant string) error {
	if _, found := findEngineVariant(variant); !found {
		variant = currentEngineVariant()
	}
	url, local, ok := engineAssetFor(runtime.GOOS, runtime.GOARCH, variantSuffix(variant))
	if !ok {
		return fmt.Errorf("no speech engine is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dlLock.Lock()
	if dlState.Active {
		dlLock.Unlock()
		return fmt.Errorf("a download is already running")
	}
	dlState = captionDownload{Model: "engine", Active: true, Count: 1, Index: 1, File: "parakeet.cpp " + parakeetRelease + " (" + variant + ")"}
	dlLock.Unlock()

	logger("[CC] Downloading the speech engine from %s", url)
	go func() {
		err := fetchRuntime(url, local, variant)
		dlLock.Lock()
		dlState.Active = false
		dlState.Finished = true
		if err != nil {
			dlState.Err = err.Error()
			logger("[CC] Speech engine download failed: %v", err)
		} else {
			logger("[CC] Speech engine %s is ready", parakeetRelease)
		}
		dlLock.Unlock()
	}()
	return nil
}

// fetchRuntime downloads the release archive and extracts the shared library
// out of it, matching on file name so a change to the directory prefix inside
// the archive does not break the download.
func fetchRuntime(url, local, variant string) error {
	dir := filepath.Join(captionRuntime, variant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s was not found in the archive", local)
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != local {
			continue
		}
		dst := filepath.Join(dir, local)
		tmp := dst + ".part"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
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
	mu      sync.Mutex
	queue   [][2]byte
	rows    byte // ccRU2 / ccRU3 / ccRU4
	started bool
	col     int
	maxCol  int
	upper   bool
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
func (c *cea608) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		c.ctrl(ccEDM)
		c.started = false
		c.col = 0
	}
}

// next returns the pair of bytes to attach to the next video frame.
func (c *cea608) next() [2]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
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
		handle, err := purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
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

// parakeet is a loaded model, reused across utterances.
type parakeet struct {
	ctx  uintptr
	lang string
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
func (p *parakeet) beginStream(language string) (uintptr, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == 0 {
		return 0, fmt.Errorf("model is closed")
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
			return 0, fmt.Errorf("%s", msg)
		}
		return 0, fmt.Errorf("this model does not support streaming")
	}
	return s, nil
}

// streamResult is what a streaming feed reports.
//
// The plain text entry point hands back whatever tokens finalized in that call,
// which is sub-word: "broadcast" arrives as "broad" then "cast" with nothing to
// say whether the two join or are separate words. The JSON entry point groups
// them properly and timestamps each word, which is both what the display needs
// and what makes a decent subtitle cue.
type streamResult struct {
	Text  string `json:"text"`
	EOU   int    `json:"eou"`
	Words []struct {
		W     string  `json:"w"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	} `json:"words"`
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
func (p *parakeet) feedStream(s uintptr, pcm []float32) *streamResult {
	if s == 0 || len(pcm) == 0 {
		return nil
	}
	out := pkStreamFeed(s, unsafe.Pointer(&pcm[0]), int32(len(pcm)))
	runtime.KeepAlive(pcm)
	return parseStreamResult(cStringFree(out))
}

// finishStream flushes the tail when the stream ends.
func (p *parakeet) finishStream(s uintptr) *streamResult {
	if s == 0 {
		return nil
	}
	return parseStreamResult(cStringFree(pkStreamFinalize(s)))
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
// Listening
// ---------------------------------------------------------------------------

// captionEngine turns the transport stream into captions: ffmpeg decodes the
// audio, a voice activity check cuts it into phrases, and each phrase is
// recognized and handed to the CEA-608 encoder.
type captionEngine struct {
	enc     *cea608
	label   string
	cfg     captionConfig
	model   *parakeet
	ffmpeg  *exec.Cmd
	audioIn io.WriteCloser
	audioCh chan []byte
	closed  chan struct{}
	once    sync.Once

	// done is closed when the listening goroutine has returned. Nothing the
	// engine owns may be freed before then: the recognizer is native code, and
	// freeing a session out from under a call in flight is a crash, not an
	// error.
	done chan struct{}
	// stream is non-zero when the chosen model transcribes continuously; the
	// phrase segmenter is not used in that case.
	stream uintptr
	// tail is the end of the phrase last shown. A forced cut carries a moment
	// of audio forward so it does not slice through a word, and that moment is
	// then recognized twice, so the repeat is trimmed against this.
	tail []string
}

func newCaptionEngine(cfg captionConfig, m captionModel, label string) (*captionEngine, error) {
	model, err := loadParakeet(modelPath(m), cfg.Language)
	if err != nil {
		return nil, err
	}
	e := &captionEngine{
		enc:     newCEA608(cfg.Style, cfg.Uppercase),
		label:   label,
		cfg:     cfg,
		model:   model,
		audioCh: make(chan []byte, 64),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}

	// ffmpeg is already in the image and is only asked for the audio, so the
	// video never passes through a codec: the caption bytes are the only change
	// this feature makes to the stream.
	// Probe and analysis are held to a second so a tune starts captioning
	// quickly. They are not switched off: "nobuffer" and "low_delay" make
	// ffmpeg emit silence for these encoders rather than audio, which shows up
	// as captions that simply never appear.
	e.ffmpeg = exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-probesize", "1000000", "-analyzeduration", "1000000",
		"-f", "mpegts", "-i", "pipe:0",
		"-vn", "-sn", "-dn",
		"-ac", "1", "-ar", strconv.Itoa(asrSampleRate), "-f", "s16le", "pipe:1")
	audioIn, err := e.ffmpeg.StdinPipe()
	if err != nil {
		model.Close()
		return nil, err
	}
	pcm, err := e.ffmpeg.StdoutPipe()
	if err != nil {
		model.Close()
		return nil, err
	}
	e.ffmpeg.Stderr = os.Stderr
	e.audioIn = audioIn

	if err := e.ffmpeg.Start(); err != nil {
		model.Close()
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}

	if m.Streaming {
		if st, err := model.beginStream(cfg.Language); err != nil {
			logger("[CC] %s could not start continuous recognition (%v), falling back to phrase at a time", label, err)
		} else {
			e.stream = st
		}
	}

	go e.pumpAudio()
	if e.stream != 0 {
		go e.listenStreaming(pcm)
	} else {
		go e.listen(pcm)
	}
	mode := "phrase at a time"
	if e.stream != 0 {
		mode = "continuous"
	}
	logger("[CC] %s captions started, model %s language %s, %s", label, m.Key, cfg.Language, mode)
	return e, nil
}

// pumpAudio hands buffered transport stream bytes to ffmpeg. Writes here must
// never block the video path, so a slow recognizer loses audio rather than
// stalling the DVR.
func (e *captionEngine) pumpAudio() {
	defer e.audioIn.Close()
	for {
		select {
		case <-e.closed:
			return
		case b, ok := <-e.audioCh:
			if !ok {
				return
			}
			if _, err := e.audioIn.Write(b); err != nil {
				return
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
)

// listenStreaming feeds audio to a cache-aware streaming session and shows text
// the moment the model finalizes it.
//
// There is no phrase segmenter here and no waiting: the model returns words as
// the audio arrives and marks where an utterance ends, which is what keeps this
// about a second behind instead of a phrase behind.
func (e *captionEngine) listenStreaming(pcm io.ReadCloser) {
	defer close(e.done)
	defer pcm.Close()

	// 100 ms per feed: short enough that nothing waits on a buffer, long enough
	// that the call overhead is irrelevant next to the work inside.
	const chunk = asrSampleRate / 10
	raw := make([]byte, chunk*2)
	buf := make([]float32, chunk)

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
			take(e.model.finishStream(e.stream))
			return
		}
		for i := range buf {
			buf[i] = float32(int16(uint16(raw[2*i])|uint16(raw[2*i+1])<<8)) / 32768.0
		}
		take(e.model.feedStream(e.stream, buf))
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

// listen reads decoded audio, splits it into phrases and captions each one.
func (e *captionEngine) listen(pcm io.ReadCloser) {
	defer close(e.done)
	defer pcm.Close()

	raw := make([]byte, vadFrame*2)
	var pending []float32
	speaking := false
	var silenceRun, speechLen float64
	floor := 0.005

	for {
		select {
		case <-e.closed:
			return
		default:
		}
		if _, err := io.ReadFull(pcm, raw); err != nil {
			return
		}

		frame := make([]float32, vadFrame)
		var sum float64
		for i := range frame {
			v := float32(int16(uint16(raw[2*i])|uint16(raw[2*i+1])<<8)) / 32768.0
			frame[i] = v
			sum += float64(v) * float64(v)
		}
		rms := math.Sqrt(sum / float64(len(frame)))

		loud := rms > math.Max(floor*3.0, 0.012)
		if !loud {
			floor = 0.995*floor + 0.005*rms
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
		gapped := phrase >= vadMinPhrase && silenceRun >= vadWordGap
		forced := phrase >= vadMaxPhrase
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
		e.caption(audio)
	}
}

// caption recognizes one phrase and queues it for display.
func (e *captionEngine) caption(audio []float32) {
	text, err := e.model.transcribe(audio)
	if err != nil {
		logger("[CC] %s recognition failed: %v", e.label, err)
		return
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
		if e.ffmpeg != nil && e.ffmpeg.Process != nil {
			e.ffmpeg.Process.Kill()
			e.ffmpeg.Wait()
		}
		// Wait for the listener to finish before releasing anything it might be
		// inside. If it somehow does not stop, leaking the session is far
		// better than freeing it from under a call in flight.
		stopped := true
		select {
		case <-e.done:
		case <-time.After(10 * time.Second):
			stopped = false
			logger("[CC] %s recognizer did not stop in time; leaving its memory alone", e.label)
		}
		if stopped {
			if e.stream != 0 {
				pkStreamFree(e.stream)
				e.stream = 0
			}
			if e.model != nil {
				e.model.Close()
			}
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
	Config         captionConfig         `json:"config"`
	Models         []captionStatusModel  `json:"models"`
	Languages      map[string]string     `json:"languageNames"`
	Download       captionDownload       `json:"download"`
	Runtime        string                `json:"runtime"`
	RuntimeReady   bool                  `json:"runtimeReady"`
	RuntimeSizeMB  int                   `json:"runtimeSizeMB"`
	RuntimeVersion string                `json:"runtimeVersion"`
	RuntimeURL     string                `json:"runtimeURL"`
	Engines        []captionStatusEngine `json:"engines"`
	Drivers        []captionStatusDriver `json:"drivers"`
	Accel          accelReport           `json:"accel"`
	DriverInstall  gpuInstallState       `json:"driverInstall"`
	Persistent     bool                  `json:"persistent"`
	PersistWarning string                `json:"persistWarning"`
	Tuners         int                   `json:"tuners"`
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
}

type captionStatusModel struct {
	captionModel
	Installed bool   `json:"installed"`
	URL       string `json:"url"`
}

func captionStatusPayload() captionStatus {
	cfg := currentCaptionConfig()
	models := make([]captionStatusModel, 0, len(captionModelCatalog))
	for _, m := range captionModelCatalog {
		models = append(models, captionStatusModel{captionModel: m, Installed: modelInstalled(m), URL: modelURL(m)})
	}
	engineURL, _, _ := engineAsset()
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
	cur := currentEngineVariant()
	cpuURL, _, _ := engineAssetFor(runtime.GOOS, runtime.GOARCH, "cpu")
	engines := make([]captionStatusEngine, 0, len(engineVariants))
	for _, v := range engineVariants {
		url, local, ok := engineAssetFor(runtime.GOOS, runtime.GOARCH, v.Suffix)
		if !ok {
			continue
		}
		// A variant with no build of its own for this platform is not a choice.
		// Apple silicon is the clear case: Metal is in the one build there, and
		// arm64 Linux has no CUDA build at all.
		if v.Key != "cpu" && url == cpuURL {
			continue
		}
		st, err := os.Stat(filepath.Join(captionRuntime, v.Key, local))
		engines = append(engines, captionStatusEngine{
			engineVariant: v,
			Usable:        engineUsable(v),
			Installed:     err == nil && st.Size() > 0,
			Selected:      v.Key == cur,
			URL:           url,
		})
	}
	state := "ready"
	switch {
	case engineLibPath() == "":
		state = fmt.Sprintf("no speech engine is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	case !engineInstalled():
		state = "the speech engine has not been downloaded yet"
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
		RuntimeSizeMB:  1,
		RuntimeVersion: parakeetRelease,
		RuntimeURL:     engineURL,
		Persistent:     persistent,
		PersistWarning: persistWarning,
		Engines:        engines,
		Drivers:        drivers,
		Accel:          accelStatus(),
		DriverInstall:  gpuInstallStatus(),
		Tuners:         len(tuners),
	}
}
