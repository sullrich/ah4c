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

// Everything the feature needs at runtime lives under the scripts directory,
// which is already a bind mount in every ah4c deployment. Nothing is added to
// the image and no new mount is required: the speech engine and the model are
// downloaded on demand from the Closed Captions page and persist there across
// container rebuilds.
const (
	captionDir     = "scripts/captions"
	captionCfgFile = "scripts/captions/config.json"
	captionModels  = "scripts/captions/models"
	captionRuntime = "scripts/captions/engine"
	captionSRTDir  = "scripts/captions/srt"
)

// parakeetRelease is the parakeet.cpp build fetched on demand. It is a ggml
// implementation of NVIDIA's Parakeet models with a flat C entry point, about a
// megabyte, opened at run time with purego. ah4c itself stays pure Go: there is
// no cgo, no ONNX Runtime and nothing linked into the binary.
const parakeetRelease = "v0.5.0"

// engineAsset names the engine archive for a platform and the library inside
// it. The archive is roughly a megabyte on CPU builds.
func engineAsset() (url, local string, ok bool) {
	base := "https://github.com/mudler/parakeet.cpp/releases/download/" + parakeetRelease + "/parakeet-" + parakeetRelease + "-lib-"
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return base + "linux-cpu-x64.tar.gz", "libparakeet.so", true
	case "linux/arm64":
		return base + "linux-cpu-arm64.tar.gz", "libparakeet.so", true
	case "darwin/arm64":
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
	return filepath.Join(captionRuntime, local)
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
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Desc      string   `json:"desc"`
	File      string   `json:"file"`
	SizeMB    int      `json:"sizeMB"`
	Languages []string `json:"languages"`
}

const captionModelRepo = "mudler/parakeet-cpp-gguf"

// Quantized weights: on CPU they are what make this run several times faster
// than real time, and they keep the download manageable.
var captionModelCatalog = []captionModel{
	{
		Key:    "parakeet-v3",
		Name:   "Parakeet TDT 0.6B v3 (multilingual)",
		Desc:   "25 European languages, detected automatically or pinned.",
		File:   "tdt-0.6b-v3-q8_0.gguf",
		SizeMB: 897,
		Languages: []string{"auto", "bg", "cs", "da", "de", "el", "en", "es", "et", "fi", "fr", "hr", "hu",
			"it", "lt", "lv", "mt", "nl", "pl", "pt", "ro", "ru", "sk", "sl", "sv", "uk"},
	},
	{
		Key:       "parakeet-110m",
		Name:      "Parakeet TDT-CTC 110M (English)",
		Desc:      "English only. A fifth of the size and quicker still, for lighter hardware.",
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
	// OffsetSec delays caption display, for trimming sync by hand. It never
	// delays the video: the stream is passed through untouched apart from the
	// caption bytes, so a tune is exactly as fast with captions on as off.
	OffsetSec int `json:"offsetSec"`
	// SaveSRT writes a subtitle file alongside every captioned stream, for
	// keeping with a recording.
	SaveSRT bool `json:"saveSRT"`
	// Tuners restricts captioning to specific tuner indexes. Empty means all.
	Tuners []int `json:"tuners"`
}

func defaultCaptionConfig() captionConfig {
	return captionConfig{
		Enabled:   false,
		Model:     "parakeet-v3",
		Language:  "auto",
		Style:     "rollup3",
		OffsetSec: 0,
	}
}

var (
	captionCfgLock sync.RWMutex
	captionCfg     = defaultCaptionConfig()
)

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
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s?download=true", captionModelRepo, m.File)
	dlLock.Lock()
	dlState.File, dlState.Index = m.File, 1
	dlLock.Unlock()
	logger("[CC] Downloading %s", m.File)
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
func startRuntimeDownload() error {
	url, local, ok := engineAsset()
	if !ok {
		return fmt.Errorf("no speech engine is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dlLock.Lock()
	if dlState.Active {
		dlLock.Unlock()
		return fmt.Errorf("a download is already running")
	}
	dlState = captionDownload{Model: "engine", Active: true, Count: 1, Index: 1, File: "parakeet.cpp " + parakeetRelease}
	dlLock.Unlock()

	go func() {
		err := fetchRuntime(url, local)
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
func fetchRuntime(url, local string) error {
	if err := os.MkdirAll(captionRuntime, 0o755); err != nil {
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
		dst := filepath.Join(captionRuntime, local)
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

// cc608Char maps a rune onto the 608 basic character set, which is ASCII with a
// handful of substitutions. Anything outside it becomes a space rather than
// garbage on screen.
func cc608Char(r rune) byte {
	switch {
	case r >= 0x20 && r <= 0x7F:
		switch r {
		case '`':
			return '\''
		case '\\', '^', '_', '{', '}', '|', '~':
			return ' '
		}
		return byte(r)
	case r == '‘' || r == '’':
		return '\''
	case r == '“' || r == '”':
		return '"'
	case r == '—' || r == '–':
		return '-'
	case r == '…':
		return '.'
	}
	return ' '
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
}

func newCEA608(style string) *cea608 {
	rows := byte(ccRU3)
	switch style {
	case "rollup2":
		rows = ccRU2
	case "rollup4":
		rows = ccRU4
	}
	return &cea608{rows: rows, maxCol: 32}
}

func (c *cea608) ctrl(code byte) {
	// Control codes are sent twice; a decoder that catches the pair twice acts
	// on it once, and the repeat is what survives a dropped frame.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(code)}, [2]byte{odd608(ccCtrlCC1), odd608(code)})
}

// begin puts the decoder into roll-up mode on the bottom row.
func (c *cea608) begin() {
	c.ctrl(c.rows)
	// Preamble address code for row 15, column 0, white non-italic: 0x14 0x70.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(0x70)}, [2]byte{odd608(ccCtrlCC1), odd608(0x70)})
	c.ctrl(ccCR)
	c.started = true
	c.col = 0
}

// ccMaxBacklog is the most unshown caption data we will hold, in byte pairs.
// The channel moves two bytes per picture, so at 30 fps this is about five
// seconds of text. Reaching it means recognition has outrun the display.
const ccMaxBacklog = 150

// push queues a phrase for display, wrapping it to the 32 column caption grid.
func (c *cea608) push(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
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
			c.ctrl(ccCR)
			c.col = 0
		}
		if c.col > 0 {
			c.writeRune(' ')
		}
		for _, r := range runes {
			if c.col >= c.maxCol {
				c.ctrl(ccCR)
				c.col = 0
			}
			c.writeRune(r)
		}
	}
	// Finish the phrase on its own line so the next one rolls up under it.
	c.ctrl(ccCR)
	c.col = 0
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
		case 1: // field 2: valid but empty
			b = append(b, 0xFD, odd608(cc608Null), odd608(cc608Null))
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
			// Resynchronize on the next sync byte rather than corrupting output.
			i := 1
			for i < len(b) && b[i] != 0x47 {
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
	// out untouched, carrying the source's own count. Pick that count up on the
	// first packet we recognize so the handover leaves no gap.
	if !ci.ccSeeded {
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

func (ci *captionInjector) parsePAT(p []byte) {
	if p[1]&0x40 == 0 || ci.pmtPID >= 0 {
		return
	}
	pl := tsPayload(p)
	if len(pl) < 1 {
		return
	}
	pl = pl[1+int(pl[0]):] // pointer_field
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
	pl := tsPayload(p)
	if len(pl) < 1 {
		return
	}
	pl = pl[1+int(pl[0]):]
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
	p[3] = (p[3] &^ 0x0F) | (ci.videoCC & 0x0F)
	if afc := (p[3] >> 4) & 0x03; afc == 0x01 || afc == 0x03 {
		// Only a packet with payload advances the count.
		ci.videoCC = (ci.videoCC + 1) & 0x0F
	}
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

// packetize turns a PES packet back into transport packets on the video PID,
// carrying the original adaptation field of the first packet so the PCR and any
// random access indicator survive.
func (ci *captionInjector) packetize(pes []byte) [][tsPacketSize]byte {
	var af []byte
	if len(ci.window) > 0 {
		for i := range ci.window {
			if ci.window[i].video {
				af = adaptationField(ci.window[i].buf[:])
				break
			}
		}
	}

	var out [][tsPacketSize]byte
	first := true
	for len(pes) > 0 {
		var pkt [tsPacketSize]byte
		pkt[0] = 0x47
		pkt[1] = byte(ci.videoPID >> 8)
		pkt[2] = byte(ci.videoPID)
		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
		}

		body := 4
		useAF := first && len(af) > 0
		if useAF {
			// adaptation_field_control = 11
			pkt[3] = 0x30 | (ci.videoCC & 0x0F)
			pkt[4] = byte(len(af))
			copy(pkt[5:], af)
			body = 5 + len(af)
		} else {
			pkt[3] = 0x10 | (ci.videoCC & 0x0F)
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
				pkt[3] = 0x30 | (ci.videoCC & 0x0F)
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
			ci.videoCC = (ci.videoCC + 1) & 0x0F
			break
		}
		copy(pkt[body:], pes[:space])
		pes = pes[space:]
		out = append(out, pkt)
		ci.videoCC = (ci.videoCC + 1) & 0x0F
		first = false
	}
	return out
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
			if n < len(pkts) {
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
		logger("[CC] Speech engine loaded, ABI %d", pkABIVersion())
	})
	return pkErr
}

// parakeet is a loaded model, reused across utterances.
type parakeet struct {
	ctx  uintptr
	lang string
	mu   sync.Mutex // the context holds decoder state, so one utterance at a time
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
	return &parakeet{ctx: ctx, lang: language}, nil
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
	return cStringFree(out), nil
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
// Subtitle files
// ---------------------------------------------------------------------------

// A recording made from a captioned stream already carries the captions in the
// picture, but a subtitle file beside it is easier to search, edit and feed to
// a player that does not read CEA-608.
//
// The cues use the real speech times rather than the times the text reached the
// screen, so the file is actually better aligned than the embedded captions: on
// screen a phrase cannot appear until it has been spoken and recognized, while
// in a file it can be stamped with the moment it was said.

type srtWriter struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	index   int
	cues    int
	lastEnd float64
}

// newSRTWriter opens a subtitle file for one stream. The name carries the
// channel and the time the stream started, so a file can be matched to a
// recording after the fact.
func newSRTWriter(channel string) (*srtWriter, error) {
	if err := os.MkdirAll(captionSRTDir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s %s.srt", time.Now().Format("2006-01-02 15.04.05"), srtSafeName(channel))
	path := filepath.Join(captionSRTDir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &srtWriter{f: f, path: path}, nil
}

// srtSafeName reduces a channel name to something a filesystem is happy with.
// Channel strings arrive as "MS NOW~6ea83d29-...", so the identifier after the
// tilde is dropped and what is left is trimmed.
func srtSafeName(channel string) string {
	if i := strings.IndexByte(channel, '~'); i > 0 {
		channel = channel[:i]
	}
	channel = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == ' ', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, channel)
	channel = strings.TrimSpace(channel)
	if len(channel) > 60 {
		channel = channel[:60]
	}
	if channel == "" {
		return "stream"
	}
	return channel
}

// srtStamp formats seconds as SRT's hours:minutes:seconds,milliseconds.
func srtStamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(sec*1000 + 0.5)
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	sd := ms / 1000
	ms -= sd * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, sd, ms)
}

// add appends one cue, flushing as it goes so a file left behind by a crash or
// a pulled plug still holds everything recognized up to that point.
func (w *srtWriter) add(start, end float64, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if end <= start {
		end = start + 1
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return
	}
	// A phrase carries a moment of the previous one forward so a cut does not
	// clip a word, which would otherwise leave one cue starting before the last
	// one ended. Players dislike that, so keep the cues in order.
	if start < w.lastEnd {
		start = w.lastEnd
	}
	if end <= start {
		end = start + 1
	}
	w.lastEnd = end
	w.index++
	if _, err := fmt.Fprintf(w.f, "%d\n%s --> %s\n%s\n\n",
		w.index, srtStamp(start), srtStamp(end), srtWrap(text)); err != nil {
		return
	}
	w.f.Sync()
	w.cues++
}

// srtWrap breaks a long cue over two lines at the word boundary nearest the
// middle, which is how a subtitle is normally laid out and stops one phrase
// spanning the whole picture.
func srtWrap(text string) string {
	const limit = 42
	if len(text) <= limit {
		return text
	}
	words := strings.Fields(text)
	if len(words) < 2 {
		return text
	}
	half := len(text) / 2
	split, run, best := 1, 0, len(text)+1
	for i := 0; i < len(words)-1; i++ {
		if i > 0 {
			run++ // the space before this word
		}
		run += len(words[i])
		d := run - half
		if d < 0 {
			d = -d
		}
		if d < best {
			best, split = d, i+1
		}
	}
	return strings.Join(words[:split], " ") + "\n" + strings.Join(words[split:], " ")
}

// Close finishes the file, removing it if nothing was ever recognized so an
// empty subtitle file is not left beside a recording.
func (w *srtWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return
	}
	w.f.Close()
	w.f = nil
	if w.cues == 0 {
		os.Remove(w.path)
		return
	}
	logger("[CC] Wrote %d subtitle cues to %s", w.cues, w.path)
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

	srt *srtWriter
}

func newCaptionEngine(cfg captionConfig, m captionModel, label, channel string) (*captionEngine, error) {
	model, err := loadParakeet(modelPath(m), cfg.Language)
	if err != nil {
		return nil, err
	}
	e := &captionEngine{
		enc:     newCEA608(cfg.Style),
		label:   label,
		cfg:     cfg,
		model:   model,
		audioCh: make(chan []byte, 64),
		closed:  make(chan struct{}),
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

	if cfg.SaveSRT {
		if w, err := newSRTWriter(channel); err != nil {
			logger("[CC] %s could not open a subtitle file: %v", label, err)
		} else {
			e.srt = w
		}
	}

	go e.pumpAudio()
	go e.listen(pcm)
	logger("[CC] %s captions started, model %s language %s", label, m.Key, cfg.Language)
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

// listen reads decoded audio, splits it into phrases and captions each one.
func (e *captionEngine) listen(pcm io.ReadCloser) {
	defer pcm.Close()

	raw := make([]byte, vadFrame*2)
	// Samples seen so far. The reader sits outside any playback gating, so this
	// clock starts where the recording does and the cue times line up with it.
	var consumed int64
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

		consumed += int64(vadFrame)
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
		end := float64(consumed) / asrSampleRate
		e.caption(audio, end-float64(len(audio))/asrSampleRate, end)
	}
}

// caption recognizes one phrase and queues it for display.
func (e *captionEngine) caption(audio []float32, start, end float64) {
	text, err := e.model.transcribe(audio)
	if err != nil {
		logger("[CC] %s recognition failed: %v", e.label, err)
		return
	}
	if text == "" {
		return
	}
	if e.srt != nil {
		e.srt.add(start, end, text)
	}
	if d := e.cfg.OffsetSec; d > 0 {
		// Hold the phrase back for hand-tuned sync. The video is never delayed,
		// so this only ever pushes text later.
		time.AfterFunc(time.Duration(d)*time.Second, func() { e.enc.push(text) })
		return
	}
	e.enc.push(text)
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
		if e.ffmpeg != nil && e.ffmpeg.Process != nil {
			e.ffmpeg.Process.Kill()
			e.ffmpeg.Wait()
		}
		if e.model != nil {
			e.model.Close()
		}
		if e.srt != nil {
			e.srt.Close()
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
func maybeWrapCaptions(src io.ReadCloser, tunerIndex int, label, channel string) io.ReadCloser {
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
	engine, err := newCaptionEngine(cfg, m, label, channel)
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
	Config         captionConfig        `json:"config"`
	Models         []captionStatusModel `json:"models"`
	Languages      map[string]string    `json:"languageNames"`
	Download       captionDownload      `json:"download"`
	Runtime        string               `json:"runtime"`
	RuntimeReady   bool                 `json:"runtimeReady"`
	RuntimeSizeMB  int                  `json:"runtimeSizeMB"`
	RuntimeVersion string               `json:"runtimeVersion"`
	Tuners         int                  `json:"tuners"`
}

type captionStatusModel struct {
	captionModel
	Installed bool `json:"installed"`
}

func captionStatusPayload() captionStatus {
	cfg := currentCaptionConfig()
	models := make([]captionStatusModel, 0, len(captionModelCatalog))
	for _, m := range captionModelCatalog {
		models = append(models, captionStatusModel{captionModel: m, Installed: modelInstalled(m)})
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
		Tuners:         len(tuners),
	}
}
