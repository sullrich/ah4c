package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// USB capture cards, set up the way the Channels community does it: Channels
// DVR captures from the card itself through a custom-channels source made of
// capture:// lines, and each card's stream from that source becomes a tuner's
// encoder address. ah4c writes that source and reads the addresses back, so the
// card never has to be driven by a CMD that the preview cannot open.
const (
	captureSourceKey  = "AH4CCapture"
	captureSourceName = "AH4C Capture"
	captureDeviceID   = channelsM3UDevicePrefix + captureSourceKey
	// A list of cards is a few kilobytes; this bounds a request gone wrong.
	maxCaptureRequestBytes int64 = 256 << 10
)

var (
	captureVideoPattern = regexp.MustCompile(`^video[0-9]+$`)
	captureAudioPattern = regexp.MustCompile(`^(plug)?hw:[A-Za-z0-9_]+,[0-9]+$`)
	usbPortPattern      = regexp.MustCompile(`^[0-9]+-[0-9.]+$`)
	usbBusPattern       = regexp.MustCompile(`^usb[0-9]+$`)
	soundCardPattern    = regexp.MustCompile(`^card([0-9]+)$`)
	soundCapturePattern = regexp.MustCompile(`^pcmC[0-9]+D([0-9]+)c$`)
	captureLinePattern  = regexp.MustCompile(`^capture://v4l2/([^/]+)/(?:([^/]+)/)?\?(.*)$`)
)

// captureCard is one USB capture card: its video node and ALSA capture device
// as Channels DVR sees them, and the picture it is asked for.
type captureCard struct {
	Name      string `json:"name"`
	Bus       string `json:"bus"`
	Video     string `json:"video"`
	Audio     string `json:"audio"`
	AudioCard int    `json:"audioCard"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Framerate int    `json:"framerate"`
	// PixelFormat is how the card sends its picture at this size and rate,
	// such as mjpeg. Empty leaves the choice to Channels DVR's ffmpeg, which
	// takes the uncompressed format, and many cards send full HD uncompressed
	// at only a few frames a second.
	PixelFormat string `json:"pixelFormat"`
	// Modes are the sizes and rates the card itself says it can send.
	Modes []captureMode `json:"modes,omitempty"`
}

// captureMode is one picture size a card can send in one format, with the
// frame rates it offers at that size.
type captureMode struct {
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Rates  []int  `json:"rates"`
}

// capturePixelFormats are the formats ah4c names in a capture line, keyed by
// the four-character code a USB card gives its uncompressed formats.
var capturePixelFormats = map[string]string{"YUY2": "yuyv422", "NV12": "nv12"}

// captureAudioChoice is an ALSA capture device that no video device claimed,
// offered so a card can be paired with it by hand.
type captureAudioChoice struct {
	Card  int    `json:"card"`
	Audio string `json:"audio"`
	Name  string `json:"name"`
	Bus   string `json:"bus"`
}

type captureStream struct {
	Card  int    `json:"card"`
	Video string `json:"video"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

type v4l2Device struct {
	name  string
	bus   string
	video string
	modes []captureMode
}

type alsaCard struct {
	index  int
	id     string
	name   string
	bus    string
	device int
}

// captureMissingDriver is a USB capture card the kernel has no driver bound
// for, so it has no /dev/video or no sound device at all.
type captureMissingDriver struct {
	Bus     string `json:"bus"`
	Product string `json:"product"`
}

// captureDrivers is what detection found about drivers: capture cards the
// kernel sees on USB but cannot use, and which of the two drivers are loaded.
type captureDrivers struct {
	MissingVideo []captureMissingDriver `json:"missingVideo"`
	MissingSound []captureMissingDriver `json:"missingSound"`
	Loaded       []string               `json:"loaded"`
}

func registerCaptureCardRoutes(r *gin.Engine) {
	r.POST("/api/capture/detect", detectCaptureCardsHandler)
	r.GET("/api/tuner/:index/preview-check", previewCheckHandler)
	r.GET("/api/tuner/:index/capture-preview", capturePreviewHandler)
	r.GET("/api/capture/channels", findCaptureStreamsHandler)
	r.PUT("/api/capture/channels", putCaptureSourceHandler)
}

// captureSysRoot is where the kernel's device tree is read. A container sees
// the host's /sys, read-only, so ah4c can find the cards itself without anyone
// typing a command on the server.
var captureSysRoot = "/sys"

func readSysValue(path string) string {
	value, _ := os.ReadFile(path)
	return strings.TrimSpace(string(value))
}

// usbDeviceOf follows a sysfs device link to the USB device it hangs off, and
// names that device's port the way the kernel does, usb-<controller>-<port>, so
// a card's picture and sound can be matched by where they are plugged in.
func usbDeviceOf(link string) (string, string) {
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(filepath.ToSlash(resolved), "/")
	for i := len(parts) - 1; i > 0; i-- {
		if !usbPortPattern.MatchString(parts[i]) {
			continue
		}
		controller := ""
		for j := i - 1; j > 0; j-- {
			if usbBusPattern.MatchString(parts[j]) {
				controller = parts[j-1]
				break
			}
		}
		// A name like 1-0050 outside any USB controller is not USB at all.
		if controller == "" {
			return "", ""
		}
		_, port, _ := strings.Cut(parts[i], "-")
		return strings.Join(parts[:i+1], "/"), "usb-" + controller + "-" + port
	}
	return "", ""
}

// detectVideoDevices lists each USB capture node: a card makes two video nodes,
// and the one with index 0 is the picture, the other its metadata.
func detectVideoDevices(root string) []v4l2Device {
	nodes, _ := filepath.Glob(filepath.Join(root, "class", "video4linux", "video*"))
	sort.Slice(nodes, func(a, b int) bool { return videoNumber(nodes[a]) < videoNumber(nodes[b]) })
	devices := []v4l2Device{}
	for _, node := range nodes {
		video := filepath.Base(node)
		if !captureVideoPattern.MatchString(video) {
			continue
		}
		if index := readSysValue(filepath.Join(node, "index")); index != "" && index != "0" {
			continue
		}
		name := readSysValue(filepath.Join(node, "name"))
		// "USB3.0 Video: USB3.0 Video" names the card twice; once is enough.
		if first, rest, found := strings.Cut(name, ": "); found && first == rest {
			name = first
		}
		// Only USB cards: a camera or codec built into the board is not one.
		usbDir, bus := usbDeviceOf(filepath.Join(node, "device"))
		if bus == "" {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(usbDir, "descriptors"))
		devices = append(devices, v4l2Device{name: name, bus: bus, video: video, modes: parseUVCModes(raw)})
	}
	return devices
}

func videoNumber(node string) int {
	number, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(node), "video"))
	return number
}

// detectSoundCards lists each sound card that can record, with its first
// recording device, named the way Channels DVR is given it: hw:<id>,<device>.
func detectSoundCards(root string) []alsaCard {
	entries, _ := os.ReadDir(filepath.Join(root, "class", "sound"))
	cards := []alsaCard{}
	for _, entry := range entries {
		match := soundCardPattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		index, _ := strconv.Atoi(match[1])
		dir := filepath.Join(root, "class", "sound", entry.Name())
		device := -1
		for _, other := range entries {
			if !strings.HasPrefix(other.Name(), fmt.Sprintf("pcmC%dD", index)) {
				continue
			}
			if found := soundCapturePattern.FindStringSubmatch(other.Name()); found != nil {
				if number, _ := strconv.Atoi(found[1]); device < 0 || number < device {
					device = number
				}
			}
		}
		id := readSysValue(filepath.Join(dir, "id"))
		if device < 0 || id == "" {
			continue
		}
		usbDir, bus := usbDeviceOf(filepath.Join(dir, "device"))
		name := id
		if usbDir != "" {
			if product := readSysValue(filepath.Join(usbDir, "product")); product != "" {
				name = product
			}
		}
		cards = append(cards, alsaCard{index: index, id: id, name: name, bus: bus, device: device})
	}
	sort.Slice(cards, func(a, b int) bool { return cards[a].index < cards[b].index })
	return cards
}

// detectCaptureDrivers finds capture cards the kernel sees on USB but has not
// bound a driver to. Such a card has no /dev/video, so without this it would
// simply not be listed, and "no cards found" would hide the real reason. Only
// devices with a video interface count, so an unrelated USB headset is not
// taken for a capture card.
func detectCaptureDrivers(root string) captureDrivers {
	report := captureDrivers{MissingVideo: []captureMissingDriver{}, MissingSound: []captureMissingDriver{}, Loaded: []string{}}
	devices, _ := filepath.Glob(filepath.Join(root, "bus", "usb", "devices", "*"))
	sort.Strings(devices)
	for _, device := range devices {
		base := filepath.Base(device)
		if !usbPortPattern.MatchString(base) {
			continue
		}
		interfaces, _ := filepath.Glob(filepath.Join(device, base+":*"))
		// A driver takes a card's control interface and claims the rest, so the
		// card has a driver for its picture or sound when any interface of that
		// kind is bound, and lacks one only when none is.
		hasVideo, hasSound, videoBound, soundBound := false, false, false, false
		for _, iface := range interfaces {
			_, err := os.Stat(filepath.Join(iface, "driver"))
			bound := err == nil
			switch readSysValue(filepath.Join(iface, "bInterfaceClass")) {
			case "0e":
				hasVideo = true
				videoBound = videoBound || bound
			case "01":
				hasSound = true
				soundBound = soundBound || bound
			}
		}
		if !hasVideo {
			continue
		}
		videoUnbound, soundUnbound := !videoBound, hasSound && !soundBound
		_, bus := usbDeviceOf(device)
		missing := captureMissingDriver{Bus: bus, Product: readSysValue(filepath.Join(device, "product"))}
		if videoUnbound {
			report.MissingVideo = append(report.MissingVideo, missing)
		}
		if soundUnbound {
			report.MissingSound = append(report.MissingSound, missing)
		}
	}
	for _, module := range []string{"uvcvideo", "snd-usb-audio"} {
		if _, err := os.Stat(filepath.Join(root, "module", strings.ReplaceAll(module, "-", "_"))); err == nil {
			report.Loaded = append(report.Loaded, module)
		}
	}
	return report
}

// pairCaptureCards joins each video device to the sound card on the same USB
// port. A card whose port cannot be matched keeps an empty audio device, and
// every sound card left over is offered for pairing by hand, so nothing is
// paired by guesswork.
func pairCaptureCards(videos []v4l2Device, sounds []alsaCard) ([]captureCard, []captureAudioChoice) {
	claimed := map[int]bool{}
	cards := make([]captureCard, 0, len(videos))
	for _, video := range videos {
		card := captureCard{Name: video.name, Bus: video.bus, Video: video.video, AudioCard: -1, Width: 1920, Height: 1080, Framerate: 60, Modes: video.modes}
		if mode, rate, ok := bestCaptureMode(video.modes); ok {
			card.Width, card.Height, card.Framerate = mode.Width, mode.Height, rate
			card.PixelFormat = captureFormatFor(video.modes, mode.Width, mode.Height, rate)
		}
		for _, sound := range sounds {
			if video.bus != "" && sound.bus == video.bus && !claimed[sound.index] {
				card.Audio = fmt.Sprintf("hw:%s,%d", sound.id, sound.device)
				card.AudioCard = sound.index
				claimed[sound.index] = true
				break
			}
		}
		cards = append(cards, card)
	}
	var left []captureAudioChoice
	for _, sound := range sounds {
		if !claimed[sound.index] {
			left = append(left, captureAudioChoice{Card: sound.index, Audio: fmt.Sprintf("hw:%s,%d", sound.id, sound.device), Name: sound.name, Bus: sound.bus})
		}
	}
	return cards, left
}

// validCaptureCard checks one card before it is written into a capture:// line,
// where a slash, a space, or a stray character would break the address. A card
// with no sound device is refused unless the page is in debug mode, where it is
// written as a picture-only line for testing on a server with no sound driver.
func validCaptureCard(card captureCard, allowSilent bool) error {
	if !captureVideoPattern.MatchString(card.Video) {
		return fmt.Errorf("the video device must look like video0")
	}
	if card.Audio == "" && allowSilent {
		// Written without a sound device; see captureSourceText.
	} else if !captureAudioPattern.MatchString(card.Audio) {
		return captureSoundError(allowSilent)
	}
	if card.Width < 320 || card.Width > 7680 || card.Height < 240 || card.Height > 4320 {
		return fmt.Errorf("the picture size must be between 320x240 and 7680x4320")
	}
	if card.Framerate < 1 || card.Framerate > 120 {
		return fmt.Errorf("the frame rate must be between 1 and 120")
	}
	if card.PixelFormat != "" && card.PixelFormat != "mjpeg" && card.PixelFormat != "yuyv422" && card.PixelFormat != "nv12" {
		return fmt.Errorf("the picture format must be mjpeg, yuyv422 or nv12")
	}
	return nil
}

// parseUVCModes reads the picture modes a USB video card describes about
// itself: every size in every format it can send, with the frame rates it
// offers at each. The USB descriptors are readable from any container, so the
// card is asked nothing and nothing opens it. A format ah4c cannot name, such
// as a card's own H.264, is left out.
func parseUVCModes(raw []byte) []captureMode {
	var modes []captureMode
	format := ""
	for i := 0; i+2 < len(raw); {
		length := int(raw[i])
		if length < 3 || i+length > len(raw) {
			break
		}
		block := raw[i : i+length]
		i += length
		if block[1] != 0x24 {
			continue
		}
		switch block[2] {
		case 0x04: // uncompressed format; its GUID starts with a four-character code
			format = ""
			if len(block) >= 9 {
				format = capturePixelFormats[string(block[5:9])]
			}
		case 0x06: // MJPEG format
			format = "mjpeg"
		case 0x10: // frame-based formats such as H.264
			format = ""
		case 0x05, 0x07: // a frame of the format above
			if format == "" || len(block) < 26 {
				continue
			}
			mode := captureMode{Format: format, Width: int(binary.LittleEndian.Uint16(block[5:7])), Height: int(binary.LittleEndian.Uint16(block[7:9]))}
			seen := map[int]bool{}
			addRate := func(interval uint32) {
				if interval == 0 {
					return
				}
				rate := int(math.Round(1e7 / float64(interval)))
				if rate >= 1 && rate <= 120 && !seen[rate] {
					seen[rate] = true
					mode.Rates = append(mode.Rates, rate)
				}
			}
			if count := int(block[25]); count > 0 {
				for k := 0; k < count && 26+4*k+4 <= len(block); k++ {
					addRate(binary.LittleEndian.Uint32(block[26+4*k:]))
				}
			} else if len(block) >= 38 {
				// A continuous range: offer the usual rates inside it.
				fastest, slowest := binary.LittleEndian.Uint32(block[26:]), binary.LittleEndian.Uint32(block[30:])
				for _, rate := range []int{60, 50, 30, 25, 24, 20, 15, 10, 5} {
					interval := uint32(1e7 / rate)
					if interval >= fastest && interval <= slowest {
						addRate(interval)
					}
				}
			}
			sort.Sort(sort.Reverse(sort.IntSlice(mode.Rates)))
			if len(mode.Rates) > 0 {
				modes = append(modes, mode)
			}
		}
	}
	return modes
}

// captureFormatFor names the format to ask for at a size and rate. When the
// card sends that uncompressed, nothing is named, as in the Channels
// community's guide; only when it takes MJPEG to reach the rate is mjpeg named.
func captureFormatFor(modes []captureMode, width, height, rate int) string {
	format := ""
	for _, mode := range modes {
		if mode.Width != width || mode.Height != height {
			continue
		}
		for _, r := range mode.Rates {
			if r != rate {
				continue
			}
			if mode.Format != "mjpeg" {
				return ""
			}
			format = "mjpeg"
		}
	}
	return format
}

// captureDefaultRate starts a card at 30 frames a second when it offers 30:
// cheap cards list 60 but deliver about 30 and drop the rest as broken
// frames. A card without 30 starts at its fastest rate up to 60. The rate can
// still be changed on the tuner.
func captureDefaultRate(rates []int) int {
	best := 0
	for _, r := range rates {
		if r == 30 {
			return 30
		}
		if r <= 60 && r > best {
			best = r
		}
	}
	if best == 0 && len(rates) > 0 {
		best = rates[len(rates)-1]
	}
	return best
}

// bestCaptureMode picks what a card is set up with at first: full HD when the
// card offers it, otherwise its largest picture, at captureDefaultRate, so a
// card that sends full HD uncompressed at five frames a second but as MJPEG
// at thirty is set up as MJPEG at thirty.
func bestCaptureMode(modes []captureMode) (captureMode, int, bool) {
	var best captureMode
	bestRate, bestScore := 0, -1
	for _, mode := range modes {
		if len(mode.Rates) == 0 {
			continue
		}
		rate := captureDefaultRate(mode.Rates)
		score := mode.Width * mode.Height
		if mode.Width == 1920 && mode.Height == 1080 {
			score = 1 << 30
		}
		if score > bestScore || (score == bestScore && rate > bestRate) {
			best, bestRate, bestScore = mode, rate, score
		}
	}
	return best, bestRate, bestScore >= 0
}

func captureSoundError(allowSilent bool) error {
	if allowSilent {
		return fmt.Errorf("its sound device must look like hw:Video,0, or be left empty")
	}
	return fmt.Errorf("pick its sound device, which looks like hw:Video,0; a card with no sound can only be set up in Debug mode")
}

// captureChannelName names a card's channel after its video device, so the
// channel is the card wherever the card sits in the list. A name taken from the
// position would move a channel onto another card when one is removed or the
// list is read in another order, and a tuner still pointing at that channel
// would go on recording the wrong card without anyone saving anything.
func captureChannelName(card captureCard) string {
	return captureSourceName + " " + card.Video
}

// captureSourceText is the custom-channels list Channels DVR captures from: one
// channel per card, each a capture:// address naming the card's video node and
// sound device. A card with no sound device, allowed only in debug mode, gets a
// picture-only line, like the capture://v4l2/<videoX> form the Channels community
// shows; it has not been tried here.
func captureSourceText(cards []captureCard) string {
	var text strings.Builder
	text.WriteString("#EXTM3U\n")
	for _, card := range cards {
		fmt.Fprintf(&text, "\n#EXTINF:-1, channel-id=\"ah4c-capture-%s\",channel-number=\"capture%s\",%s\n", card.Video, strings.TrimPrefix(card.Video, "video"), captureChannelName(card))
		device := card.Video + "/"
		if card.Audio != "" {
			device += card.Audio + "/"
		}
		if card.PixelFormat != "" {
			// The form the Channels community uses to pick a card's format.
			fmt.Fprintf(&text, "capture://v4l2/%s?framerate=%d&video_size=%dx%d&pixel_format=%s\n", device, card.Framerate, card.Width, card.Height, card.PixelFormat)
		} else {
			fmt.Fprintf(&text, "capture://v4l2/%s?framerate=%d&width=%d&height=%d\n", device, card.Framerate, card.Width, card.Height)
		}
	}
	return text.String()
}

// captureCardsFromText reads a source ah4c wrote back into cards, so the page
// can show what Channels DVR is capturing now. It also counts capture lines it
// cannot read, which the next write would replace, so the page can say so.
func captureCardsFromText(text string) ([]captureCard, int) {
	var cards []captureCard
	unread := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		match := captureLinePattern.FindStringSubmatch(trimmed)
		if match == nil {
			if strings.HasPrefix(trimmed, "capture://") {
				unread++
			}
			continue
		}
		card := captureCard{Video: match[1], Audio: match[2], AudioCard: -1, Width: 1920, Height: 1080, Framerate: 60}
		if query, err := url.ParseQuery(match[3]); err == nil {
			if value, err := strconv.Atoi(query.Get("framerate")); err == nil {
				card.Framerate = value
			}
			if value, err := strconv.Atoi(query.Get("width")); err == nil {
				card.Width = value
			}
			if value, err := strconv.Atoi(query.Get("height")); err == nil {
				card.Height = value
			}
			if width, height, found := strings.Cut(query.Get("video_size"), "x"); found {
				if w, err := strconv.Atoi(width); err == nil {
					card.Width = w
				}
				if h, err := strconv.Atoi(height); err == nil {
					card.Height = h
				}
			}
			card.PixelFormat = query.Get("pixel_format")
		}
		cards = append(cards, card)
	}
	return cards, unread
}

// captureStreamsFromDevice builds each card's stream address from the channel
// numbers Channels DVR assigned on the AH4C Capture device, in the form the
// community guide uses: /devices/M3U-<source>/channels/<number>/stream.mpg.
// Naming the device rather than ANY keeps Channels DVR from handing the number
// to whichever other source happens to own it. The address is on the configured
// Channels DVR host, and asks for the stream as it is, with no second encode.
func captureStreamsFromDevice(device channelsDVRDevice, dvrBase string, cards []captureCard) []captureStream {
	base, err := url.Parse(dvrBase)
	if err != nil {
		return []captureStream{}
	}
	byName := map[string]string{}
	for _, channel := range device.Channels {
		number := strings.TrimSpace(channel.GuideNumber)
		if number != "" {
			byName[strings.TrimSpace(channel.GuideName)] = number
		}
	}
	streams := []captureStream{}
	for position, card := range cards {
		number, ok := byName[captureChannelName(card)]
		if !ok {
			continue
		}
		address := url.URL{Scheme: base.Scheme, Host: base.Host,
			Path:     "/devices/" + captureDeviceID + "/channels/" + number + "/stream.mpg",
			RawQuery: url.Values{"format": {"ts"}, "codec": {"copy"}}.Encode()}
		streams = append(streams, captureStream{Card: position, Video: card.Video, Name: captureChannelName(card), URL: address.String()})
	}
	return streams
}

// detectCaptureCardsHandler finds the cards plugged into the machine ah4c runs
// on, pairs each with its sound by USB port, and says which drivers are
// missing. It only reads /sys, so it costs nothing a tune would notice.
func detectCaptureCardsHandler(c *gin.Context) {
	videos := detectVideoDevices(captureSysRoot)
	sounds := detectSoundCards(captureSysRoot)
	drivers := detectCaptureDrivers(captureSysRoot)
	cards, left := pairCaptureCards(videos, sounds)
	// Every sound card is offered with its card number, so a card paired or
	// changed by hand keeps its card number.
	known := make([]captureAudioChoice, 0, len(sounds))
	for _, sound := range sounds {
		known = append(known, captureAudioChoice{Card: sound.index, Audio: fmt.Sprintf("hw:%s,%d", sound.id, sound.device), Name: sound.name, Bus: sound.bus})
	}
	if left == nil {
		left = []captureAudioChoice{}
	}
	c.JSON(http.StatusOK, gin.H{"cards": cards, "audio": left, "sound": known, "drivers": drivers})
}

// fetchChannelsDVROptional reads something from Channels DVR that may not exist
// yet. Only a 404 means it is not there; any other failure, a tune canceling
// the call among them, is an error, so "not there" is never said when nobody
// could look.
func fetchChannelsDVROptional(ctx context.Context, requestURL string, target any) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("User-Agent", "ah4c")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("Channels DVR returned %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return false, fmt.Errorf("could not read Channels DVR response: %w", err)
	}
	return true, nil
}

// otherCaptureSources names custom-channels sources that already capture from a
// card. Two sources opening the same card fight over it, so the page says so;
// a source that could not be read is counted, so its silence is not taken for
// a clean bill.
func otherCaptureSources(ctx context.Context, dvrBase string) ([]string, int, error) {
	base := strings.TrimRight(dvrBase, "/")
	devices, err := fetchChannelsDVRDevices(ctx, base+"/devices")
	if err != nil {
		return nil, 0, err
	}
	names := []string{}
	unchecked := 0
	for _, device := range devices {
		key := strings.TrimPrefix(device.DeviceID, channelsM3UDevicePrefix)
		if key == device.DeviceID || key == captureSourceKey || !usableChannelsSourceKey(key) {
			continue
		}
		var settings channelsM3USourceSettings
		if err := fetchChannelsDVRJSON(ctx, base+"/providers/m3u/sources/"+url.PathEscape(key), &settings); err != nil {
			if ctx.Err() != nil {
				return nil, 0, err
			}
			unchecked++
			continue
		}
		if strings.Contains(settings.Text, "capture://") {
			name := strings.TrimSpace(settings.Name)
			if name == "" {
				name = key
			}
			names = append(names, name)
		}
	}
	return names, unchecked, nil
}

// captureState reads the AH4C Capture source and the channels Channels DVR has
// listed for it: the cards in it and each card's stream address. It is the part
// worth asking again while Channels DVR loads a new source.
func captureState(ctx context.Context, dvrBase string) (gin.H, error) {
	base := strings.TrimRight(dvrBase, "/")
	var settings channelsM3USourceSettings
	found, err := fetchChannelsDVROptional(ctx, base+"/providers/m3u/sources/"+captureSourceKey, &settings)
	if err != nil {
		return nil, fmt.Errorf("Could not read the capture source from Channels DVR at %s: %w", dvrBase, err)
	}
	present := found && strings.Contains(settings.Text, "capture://")
	cards := []captureCard{}
	unread := 0
	streams := []captureStream{}
	if present {
		cards, unread = captureCardsFromText(settings.Text)
		var device channelsDVRDevice
		// A source Channels DVR has not loaded yet has no device; that is a
		// wait. Any other failure is said as one.
		if _, err := fetchChannelsDVROptional(ctx, base+"/devices/"+captureDeviceID, &device); err != nil {
			return nil, fmt.Errorf("Could not read the capture channels from Channels DVR at %s: %w", dvrBase, err)
		}
		streams = captureStreamsFromDevice(device, dvrBase, cards)
	}
	// channelsText is the list as Channels DVR holds it, which debug mode shows.
	return gin.H{"present": present, "source": captureSourceName, "cards": cards, "streams": streams, "unreadLines": unread, "channelsText": settings.Text}, nil
}

// captureSetup is what the page is told: the capture state, and any other
// source that captures from a card.
func captureSetup(ctx context.Context, dvrBase string) (gin.H, error) {
	setup, err := captureState(ctx, dvrBase)
	if err != nil {
		return nil, err
	}
	if err := addOtherCaptureSources(ctx, dvrBase, setup); err != nil {
		return nil, err
	}
	return setup, nil
}

func addOtherCaptureSources(ctx context.Context, dvrBase string, setup gin.H) error {
	others, unchecked, err := otherCaptureSources(ctx, dvrBase)
	if err != nil {
		return fmt.Errorf("Could not read the sources of Channels DVR at %s: %w", dvrBase, err)
	}
	setup["otherSources"] = others
	setup["uncheckedSources"] = unchecked
	return nil
}

func captureRequestAllowed(c *gin.Context) (string, bool) {
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "A tune is running, so Channels DVR was not asked about capture cards. Try again when it finishes."})
		return "", false
	}
	dvrBase, err := channelsDVRBaseURL()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Set the Channels DVR address in Settings first."})
		return "", false
	}
	return dvrBase, true
}

func findCaptureStreamsHandler(c *gin.Context) {
	dvrBase, ok := captureRequestAllowed(c)
	if !ok {
		return
	}
	ctx, release := channelsCallContext(c.Request.Context(), 15*time.Second)
	defer release()
	setup, err := captureSetup(ctx, dvrBase)
	if err != nil {
		if !configOperationsAllowed() {
			c.JSON(http.StatusConflict, gin.H{"error": "A tune started, so Channels DVR was not asked about capture cards."})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, setup)
}

// putCaptureSource writes the AH4C Capture source. The payload is the one the
// Channels DVR web admin sends for a text source, which is also what other
// tools that create capture sources send.
func putCaptureSource(ctx context.Context, dvrBase string, cards []captureCard) error {
	// Set up as the Channels community's capture guide sets it up: stream
	// format MPEG-TS, and the channel numbers taken from the list.
	payload := map[string]string{
		"name": captureSourceName, "type": "MPEG-TS", "source": "Text", "url": "",
		"text": captureSourceText(cards), "refresh": "", "limit": "", "satip": "",
		"numbering": "", "logos": "", "xmltv_url": "", "xmltv_refresh": "",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(dvrBase, "/")+"/providers/m3u/sources/"+captureSourceKey, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "ah4c")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("Could not connect to Channels DVR at %s: %w", dvrBase, err)
	}
	defer response.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(detail))
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("Channels DVR returned %s: %s", response.Status, message)
	}
	// Channels DVR keeps capturing with the old line until the source is read
	// again, so a changed frame rate or format would otherwise not apply.
	if err := refreshChannelsM3USource(ctx, http.DefaultClient, dvrBase, captureSourceKey); err != nil {
		return fmt.Errorf("The capture source was written, but Channels DVR did not reload it: %w", err)
	}
	return nil
}

func putCaptureSourceHandler(c *gin.Context) {
	var request struct {
		Cards []captureCard `json:"cards"`
		Debug bool          `json:"debug"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCaptureRequestBytes)
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(request.Cards) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Add at least one capture card."})
		return
	}
	seen := map[string]int{}
	for position, card := range request.Cards {
		if err := validCaptureCard(card, request.Debug); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Card %d: %v.", position+1, err), "card": position})
			return
		}
		// A card can be opened once, so two channels on one device would fail.
		if earlier, taken := seen[card.Video]; taken {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Cards %d and %d both use %s. Each card needs its own video device.", earlier+1, position+1, card.Video), "card": position})
			return
		}
		seen[card.Video] = position
	}
	dvrBase, ok := captureRequestAllowed(c)
	if !ok {
		return
	}
	ctx, release := channelsCallContext(c.Request.Context(), 20*time.Second)
	defer release()
	if err := putCaptureSource(ctx, dvrBase, request.Cards); err != nil {
		if !configOperationsAllowed() {
			c.JSON(http.StatusConflict, gin.H{"error": "A tune started while the capture source was being written, so it may or may not have reached Channels DVR. Try again when the tune finishes."})
			return
		}
		logger("[CAPTURE] Channels DVR would not take the capture source: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	logger("[CAPTURE] wrote %s with %d cards to Channels DVR at %s", captureSourceName, len(request.Cards), dvrBase)
	if request.Debug {
		logger("[CAPTURE] debug: the source text sent was:\n%s", captureSourceText(request.Cards))
	}
	// Channels DVR loads a new source in the background, so its channels can
	// take a moment to reach the export. Look a few times rather than once.
	var setup gin.H
	var lookup error
	for attempt := 0; attempt < 8 && ctx.Err() == nil; attempt++ {
		found, err := captureState(ctx, dvrBase)
		if err == nil {
			setup, lookup = found, nil
			if streams, _ := found["streams"].([]captureStream); len(streams) >= len(request.Cards) {
				break
			}
		} else {
			lookup = err
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if setup == nil {
		message := "The capture source was written, but Channels DVR has not listed its channels yet. Press Load from Channels DVR in a moment."
		if lookup != nil {
			message = "The capture source was written, but its channels could not be read back: " + lookup.Error() + ". Press Load from Channels DVR in a moment."
		}
		reply := gin.H{"written": true, "present": true, "source": captureSourceName, "cards": request.Cards, "streams": []captureStream{},
			"otherSources": []string{}, "uncheckedSources": 0, "unreadLines": 0, "message": message}
		if request.Debug {
			reply["sentText"] = captureSourceText(request.Cards)
		}
		c.JSON(http.StatusOK, reply)
		return
	}
	// The rest of Channels DVR is scanned once, after the wait, not on every look.
	if err := addOtherCaptureSources(ctx, dvrBase, setup); err != nil {
		setup["otherSources"] = []string{}
		setup["uncheckedSources"] = 0
		setup["message"] = "Other sources in Channels DVR could not be checked for the same cards: " + err.Error() + "."
	}
	setup["written"] = true
	// Debug mode shows the exact list sent, beside what Channels DVR now holds.
	if request.Debug {
		setup["sentText"] = captureSourceText(request.Cards)
	}
	c.JSON(http.StatusOK, setup)
}

// previewCheckHandler says why a tuner's preview is not playing, since the
// browser's player only learns that the stream failed. It asks the encoder once
// and looks only at its answer, never waiting for video, and only while the
// tuner is idle. It gives way to a tune at once: tune() holds tunerLock from the
// moment it starts until it has its stream, so the check lets go of the encoder
// as soon as it sees that lock taken, before the tune reaches the encoder.
func previewCheckHandler(c *gin.Context) {
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
		return
	}
	tunerLock.Lock()
	if index >= len(tuners) {
		tunerLock.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
		return
	}
	address := tuners[index].url
	active := tuners[index].active
	tunerLock.Unlock()
	if strings.TrimSpace(address) == "" {
		c.JSON(http.StatusOK, gin.H{"ok": false, "definite": true, "reason": "This tuner has no encoder URL, so there is nothing for the preview to open."})
		return
	}
	busy := gin.H{"ok": false, "busy": true, "reason": "A tune is running or starting, so ah4c does not open the encoder to test it."}
	if active || tunesPending() {
		c.JSON(http.StatusOK, busy)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	var yielded atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A lock held by anything, a status read included, counts: the
				// check is worth far less than any tune, so it yields to doubt.
				if tunesPending() || !tunerLock.TryLock() {
					yielded.Store(true)
					cancel()
					return
				}
				// A held tune lets the lock go before it opens its encoder, so
				// the tuner being claimed is watched for as well as the lock.
				claimed := index < len(tuners) && tuners[index].active
				tunerLock.Unlock()
				if claimed {
					yielded.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "definite": true, "reason": "The encoder URL is not a valid address: " + err.Error()})
		return
	}
	response, err := http.DefaultClient.Do(request)
	if yielded.Load() {
		if response != nil {
			response.Body.Close()
		}
		c.JSON(http.StatusOK, busy)
		return
	}
	if err != nil {
		// A device that is asleep or still starting looks like this too, so the
		// page keeps trying and shows this only as a hint.
		c.JSON(http.StatusOK, gin.H{"ok": false, "reason": "The encoder has not answered yet: " + err.Error()})
		return
	}
	defer response.Body.Close()
	capture := strings.Contains(address, "/devices/"+captureDeviceID+"/channels/")
	if response.StatusCode == http.StatusOK && capture {
		// Channels DVR answers 200 before it opens the card, then ends the
		// stream at once when it cannot, so for a card the first byte is the
		// answer. The watcher above still ends this the moment a tune starts.
		first := make([]byte, 1)
		n, readErr := io.ReadFull(response.Body, first)
		switch {
		case yielded.Load():
			c.JSON(http.StatusOK, busy)
		case n > 0:
			c.JSON(http.StatusOK, gin.H{"ok": true, "reason": "Channels DVR is sending the card's picture, so a retry should play."})
		case errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF):
			c.JSON(http.StatusOK, gin.H{"ok": false, "definite": true, "reason": "Channels DVR could not open the capture card. Give Channels DVR the card's devices, listed under USB capture cards in Settings, then restart it."})
		default:
			c.JSON(http.StatusOK, gin.H{"ok": false, "reason": "Channels DVR has not sent the card's picture yet."})
		}
		return
	}
	if response.StatusCode == http.StatusOK {
		c.JSON(http.StatusOK, gin.H{"ok": true, "reason": "The encoder answered, so a retry should play."})
		return
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 300))
	reason := fmt.Sprintf("The encoder answered %s", response.Status)
	if text := strings.TrimSpace(strings.SplitN(string(detail), "\n", 2)[0]); text != "" {
		reason += ": " + text
	}
	reason += "."
	if capture {
		reason += " Channels DVR could not capture from the card. Check that it has the card's devices, listed under USB capture cards in Settings, and that the computer has the card's drivers."
	}
	c.JSON(http.StatusOK, gin.H{"ok": false, "definite": true, "reason": reason})
}

// A capture tuner's picture is Channels DVR's re-encode of the card, and on
// some Intel systems the graphics driver prints a warning into that same
// output about once a frame. The text lands between the 188-byte packets of
// the stream. Channels DVR's own player throws such bytes away; the browser
// player on these pages cannot find its place again and stalls, then the page
// restarts it. So the preview of a capture tuner is passed through
// copyAlignedTS, which forwards only whole packets and skips anything else.
// copyAlignedTS copies MPEG-TS from src to dst with the driver's text taken
// out. The driver buffers its warnings and writes them 4 KB at a time, which
// lands a run of plain text in the middle of a packet. Where a packet does not
// line up with the next, the run of text inside it is found and cut out, and
// the packet is whole again, so no picture is lost. Anything that still does
// not line up is skipped up to the next packet. flush runs after each write so
// the browser gets the picture as it comes.
func copyAlignedTS(dst io.Writer, src io.Reader, flush func()) error {
	const (
		packet = tsPacketSize
		// Room to find a whole run of text and check two packets after it.
		window = 3*packet + 16*1024
	)
	buf := make([]byte, 0, 4*window)
	chunk := make([]byte, 32*1024)
	out := make([]byte, 0, 64*1024)
	eof := false
	for {
		if !eof {
			n, err := src.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err == io.EOF {
				eof = true
			} else if err != nil {
				return err
			}
		}
		out = out[:0]
		for len(buf) >= packet && (eof || len(buf) >= window) {
			if buf[0] == 0x47 && (len(buf) < 2*packet || buf[packet] == 0x47) {
				out = append(out, buf[:packet]...)
				buf = buf[packet:]
				continue
			}
			if buf[0] == 0x47 {
				if repaired, ok := cutDriverText(buf); ok {
					buf = repaired
					continue
				}
			}
			next := bytes.IndexByte(buf[1:], 0x47)
			if next < 0 {
				buf = buf[:0]
				break
			}
			buf = buf[next+1:]
		}
		if len(out) > 0 {
			if _, err := dst.Write(out); err != nil {
				return err
			}
			if flush != nil {
				flush()
			}
		}
		if eof {
			return nil
		}
		// Keep the unread tail at the front so a long stream reuses memory.
		buf = append(buf[:0:0], buf...)
	}
}

// cutDriverText looks for a run of plain text inside the packet at the start
// of buf and returns buf without it, if the three packets after it then line
// up. The packet's own bytes on either side of the text can happen to be
// printable too, so the exact start and end are found by trying the few
// positions near each edge of the run and keeping the cut that lines up.
func cutDriverText(buf []byte) ([]byte, bool) {
	const (
		packet   = tsPacketSize
		shortest = 64
		slack    = 16
	)
	printable := func(b byte) bool { return b == '\n' || (b >= 0x20 && b <= 0x7e) }
	runStart := -1
	// The text can also start right where the next packet should, leaving
	// this packet whole.
	for i := 1; i <= packet && i < len(buf); i++ {
		if !printable(buf[i]) {
			continue
		}
		end := i
		for end < len(buf) && printable(buf[end]) {
			end++
		}
		if end-i >= shortest {
			runStart = i
			break
		}
		i = end
	}
	if runStart < 0 {
		return nil, false
	}
	runEnd := runStart
	for runEnd < len(buf) && printable(buf[runEnd]) {
		runEnd++
	}
	// Lining up fixes only the length of the cut, not where it starts: the
	// packet's own bytes next to the text are often printable, and a cut one
	// byte early keeps a byte of text and loses a byte of picture. Text that
	// repeats one line, as a driver's warning does, settles it: the cut that
	// takes out exactly that repetition is the right one.
	line := repeatedLine(buf[runStart:runEnd])
	first := []byte(nil)
	for start := runStart; start <= runStart+slack && start <= packet; start++ {
		for end := runEnd; end >= runEnd-slack && end-start >= shortest; end-- {
			cut := end - start
			if 3*packet+cut >= len(buf) {
				continue
			}
			if buf[packet+cut] != 0x47 || buf[2*packet+cut] != 0x47 || buf[3*packet+cut] != 0x47 {
				continue
			}
			repaired := append(append(make([]byte, 0, len(buf)-cut), buf[:start]...), buf[end:]...)
			if line == nil || isRepetitionOf(buf[start:end], line) {
				return repaired, true
			}
			if first == nil {
				first = repaired
			}
		}
	}
	if first != nil {
		return first, true
	}
	return nil, false
}

// repeatedLine returns the line a run of text is made of, taken between its
// first two line ends, or nil when the run holds fewer than two.
func repeatedLine(run []byte) []byte {
	a := bytes.IndexByte(run, '\n')
	if a < 0 {
		return nil
	}
	b := bytes.IndexByte(run[a+1:], '\n')
	if b < 0 {
		return nil
	}
	return run[a+1 : a+1+b+1]
}

// isRepetitionOf reports whether seg is a stretch of line repeated, starting
// anywhere inside a line and ending anywhere inside one.
func isRepetitionOf(seg, line []byte) bool {
	k := bytes.IndexByte(seg, '\n')
	if k < 0 || k+1 > len(line) || !bytes.Equal(seg[:k+1], line[len(line)-(k+1):]) {
		return false
	}
	for rest := seg[k+1:]; len(rest) > 0; {
		n := min(len(rest), len(line))
		if !bytes.Equal(rest[:n], line[:n]) {
			return false
		}
		rest = rest[n:]
	}
	return true
}

// capturePreviewHandler shows a capture tuner's picture with only whole
// packets, for the Activity and Settings previews. Like the preview check it
// gives way to a tune at once: it refuses while one runs, and the moment one
// starts, or the tuner lock is taken, it lets go of the stream.
func capturePreviewHandler(c *gin.Context) {
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
		return
	}
	tunerLock.Lock()
	if index >= len(tuners) {
		tunerLock.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
		return
	}
	address := tuners[index].url
	active := tuners[index].active
	tunerLock.Unlock()
	if !strings.Contains(address, "/devices/"+captureDeviceID+"/channels/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this tuner does not record a capture card"})
		return
	}
	if active || tunesPending() {
		c.JSON(http.StatusConflict, gin.H{"error": "A tune is running or starting, so the preview waits."})
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tunesPending() || !tunerLock.TryLock() {
					cancel()
					return
				}
				claimed := index < len(tuners) && tuners[index].active
				tunerLock.Unlock()
				if claimed {
					cancel()
					return
				}
			}
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("Channels DVR returned %s", response.Status)})
		return
	}
	c.Header("Content-Type", "video/mp2t")
	c.Writer.WriteHeaderNow()
	_ = copyAlignedTS(c.Writer, response.Body, c.Writer.Flush)
}
