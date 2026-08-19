package main

// The model catalog and the saved configuration.
//
// What models exist, what each one asks of the code around it, and what the
// Closed Captions page has been set to. A model itself lives in its own file —
// cohere.go, canary.go, nemotron.go, parakeet.go — and is attached here.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

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
	canaryFlash,
	cohereTranscribe,
	nemotronStreaming,
	parakeetTDT,
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
	// Spelling is which English the captions are written in: "us", "gb", or
	// empty for whatever the model wrote.
	//
	// Only some of the models are told which English to write. Canary's command
	// line offers "en" and no variants, so it writes whichever spelling its
	// training data leaned toward and there is nothing to pass it — it wrote
	// "tyre". This corrects that afterward, in either direction, and does
	// nothing unless asked: somebody watching British television may well want
	// what was said spelled the way they spell it.
	Spelling string `json:"spelling"`
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
		Model:       "canary-180m",
		Language:    "en",
		Style:       "box2",
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
