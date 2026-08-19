package main

// The speech engine: loading transcribe.cpp and talking to it.
//
// The library is opened at run time rather than linked, so a build without it
// still runs and captions simply stay off. Everything below the binding layer
// is about sharing one copy of a model between tuners.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

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

// runningOn says where the work is happening, in English.
//
// The build variants are lowercase identifiers — cpu, vulkan, cuda — and the
// line said "on the" in front of whichever one it was. A processor takes the
// article and an API does not, so this read "on the vulkan", which is a
// different franchise.
func runningOn(variant string) string {
	switch {
	case variant == "cpu" || variant == "":
		return "on the processor"
	case strings.HasPrefix(variant, "vulkan"):
		return "on Vulkan"
	case strings.HasPrefix(variant, "cuda"):
		return "on CUDA"
	case strings.HasPrefix(variant, "metal"):
		return "on Metal"
	}
	return "on " + variant
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
		logger("[CC] %s needs a GPU, so it runs %s rather than on the processor", m.Name, runningOn(g))
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

// gpuGate limits how many phrase decodes are issued at the accelerator at once.
//
// A phrase decode is long and bursty: several seconds of audio arriving in a
// clump when a phrase closes. Issuing every tuner's at the card together does
// not run them faster — the card runs them one at a time regardless, and the
// interleaving costs command buffer churn and working memory on top. Letting a
// couple through keeps it busy without the pile-up.
//
// It does not apply to continuous recognition. A streaming session holds its
// own copy of the model, which is the arrangement the engine's threading
// contract names for parallel work, and its calls are short and steady rather
// than long and bursty; see enterGPU. Back-pressure there is the per-stream job
// queue instead.
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

// What bounds one dispatch, and in which unit.
//
// It was twenty seconds of audio, on the reasoning that compute time follows
// audio length and a batch allowed to grow without limit could outgrow the run
// deadline. The first half of that is not what the engine reports: it prints
// the encoder time per dispatch and it barely moves between a forty-four frame
// phrase and a fifty frame one. The cost is per phrase.
//
// So the phrase count is the bound that matters and the audio figure is a
// backstop. Ten phrases at the six hundred milliseconds this measures on
// integrated graphics is six seconds against a twelve second deadline, and it
// leaves the same room on a machine half the speed.
//
// Twenty seconds of audio was binding first and it was binding at five phrases
// — so with six tuners captioned, one of them waited a whole dispatch for no
// reason but the unit this was counted in.
const (
	maxBatchPhrases  = 10
	maxBatchAudioSec = 90.0
)

func (t *transcribeModel) enterGPU() bool {
	if !t.onGPU {
		return false
	}
	// A per-stream copy does not queue behind the others, because the engine
	// says so in as many words.
	//
	// Its threading contract is that at most one compute may be in flight
	// across all sessions of a given model — sessions share the model's backend
	// instances, so overlapping runs race, and it names what that looks like:
	// corrupted decodes on the processor, command buffer failures on Metal.
	// Then it says what to do instead: "Callers that want parallel
	// transcription today should load one model per worker."
	//
	// Which is exactly the shape here. acquireTxModel loads a fresh handle for
	// every streaming session, so each has its own model, and the compute lock
	// taken in armSetup is per model rather than shared. The engine's rule is
	// already satisfied — and the gate below was a second, self-imposed limit
	// on top of it, holding seven streams that the engine permits to run
	// together down to two at a time.
	//
	// The back-pressure that gate was providing is provided properly now, per
	// stream, by the job queue in listenStreaming: audio that cannot be kept up
	// with is coalesced and then passed over, with a count and a reason, rather
	// than piling up behind a semaphore.
	//
	//	https://github.com/handy-computer/transcribe.cpp/blob/main/include/transcribe.h
	if t.streaming {
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
		for len(batch) < maxBatchPhrases && audioSec < maxBatchAudioSec && len(svc.requests) > 0 {
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
	// Published so the segmenter can see it. See phraseCutAt.
	recogPressure.Store(int64(waiting))
	if len(batch) > 0 {
		phraseCost.Store(int64(compute) / int64(len(batch)))
	}
	txServiceLock.Lock()
	phraseStreams.Store(int64(svc.refs))
	txServiceLock.Unlock()
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
			"phrases waited %.2fs, %d in the queue, %s captioned — over the last %d dispatches",
			speed, float64(svc.tPhrases)/telemetryWindow,
			svc.tCompute.Seconds()/telemetryWindow, svc.tAudio.Seconds()/telemetryWindow,
			svc.tLag.Seconds()/telemetryWindow, waiting, plural(int64(streams), "stream", "streams"),
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
