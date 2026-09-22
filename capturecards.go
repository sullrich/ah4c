package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
	// A pasted device listing is a few kilobytes; this bounds a paste gone wrong.
	maxCaptureRequestBytes int64 = 256 << 10
)

var (
	captureVideoPattern = regexp.MustCompile(`^video[0-9]+$`)
	captureAudioPattern = regexp.MustCompile(`^(plug)?hw:[A-Za-z0-9_]+,[0-9]+$`)
	asoundCardPattern   = regexp.MustCompile(`^\s*([0-9]+)\s+\[([^\]]+?)\s*\]:\s*\S+\s+-\s+(.*)$`)
	asoundBusPattern    = regexp.MustCompile(`\bat\s+(usb-[^,\s]+)`)
	arecordCardPattern  = regexp.MustCompile(`^card\s+([0-9]+):\s+(\S+)\s+\[([^\]]*)\],\s+device\s+([0-9]+):`)
	captureLinePattern  = regexp.MustCompile(`^capture://v4l2/([^/]+)/([^/]+)/\?(.*)$`)
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
}

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
}

type alsaCard struct {
	index  int
	id     string
	name   string
	bus    string
	device int
}

// captureMissingDriver is a USB capture device the kernel has no driver bound
// for, as the listing command reports it.
type captureMissingDriver struct {
	Bus     string `json:"bus"`
	Product string `json:"product"`
}

// captureDrivers is what the listing command found about drivers: devices the
// kernel sees on USB but cannot use, and which drivers are installed at all.
type captureDrivers struct {
	MissingVideo []captureMissingDriver `json:"missingVideo"`
	MissingSound []captureMissingDriver `json:"missingSound"`
	Installed    []string               `json:"installed"`
	Loaded       []string               `json:"loaded"`
}

// parseCaptureMarkers reads the "# ah4c" lines the listing command adds. A
// card with no driver has no /dev/video, so without them it would simply not
// be listed, and "no cards found" would hide the real reason.
func parseCaptureMarkers(text string) captureDrivers {
	report := captureDrivers{MissingVideo: []captureMissingDriver{}, MissingSound: []captureMissingDriver{}, Installed: []string{}, Loaded: []string{}}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[0] != "#" || fields[1] != "ah4c" {
			continue
		}
		switch {
		case fields[2] == "no-driver" && len(fields) >= 5:
			missing := captureMissingDriver{Bus: fields[4], Product: strings.Join(fields[5:], " ")}
			if fields[3] == "video" {
				report.MissingVideo = append(report.MissingVideo, missing)
			} else if fields[3] == "sound" {
				report.MissingSound = append(report.MissingSound, missing)
			}
		case fields[2] == "module-installed" && len(fields) >= 4:
			report.Installed = append(report.Installed, fields[3])
		case fields[2] == "module-loaded" && len(fields) >= 4:
			report.Loaded = append(report.Loaded, fields[3])
		}
	}
	return report
}

func registerCaptureCardRoutes(r *gin.Engine) {
	r.POST("/api/capture/read", readCaptureCardsHandler)
	r.GET("/api/tuner/:index/preview-check", previewCheckHandler)
	r.GET("/api/capture/channels", findCaptureStreamsHandler)
	r.PUT("/api/capture/channels", putCaptureSourceHandler)
}

// parseV4L2Devices reads `v4l2-ctl --list-devices`. Each device is a heading
// that ends with its bus in parentheses, followed by its nodes; the first
// /dev/video node under a heading is the one that captures.
func parseV4L2Devices(text string) []v4l2Device {
	var devices []v4l2Device
	var current *v4l2Device
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if line == strings.TrimLeft(line, " \t") && strings.HasSuffix(trimmed, ":") {
			heading := strings.TrimSuffix(trimmed, ":")
			device := v4l2Device{name: heading}
			if open := strings.LastIndex(heading, "("); open >= 0 && strings.HasSuffix(heading, ")") {
				device.bus = heading[open+1 : len(heading)-1]
				device.name = strings.TrimSpace(heading[:open])
			}
			// "USB3.0 Video: USB3.0 Video" names the card twice; once is enough.
			if first, rest, found := strings.Cut(device.name, ": "); found && first == rest {
				device.name = first
			}
			devices = append(devices, device)
			current = &devices[len(devices)-1]
			continue
		}
		if current != nil && current.video == "" && strings.HasPrefix(trimmed, "/dev/video") {
			current.video = strings.TrimPrefix(trimmed, "/dev/")
		}
	}
	kept := devices[:0]
	for _, device := range devices {
		if device.video != "" {
			kept = append(kept, device)
		}
	}
	return kept
}

// parseAlsaCards reads `cat /proc/asound/cards`, whose second line for each
// card says which USB port it hangs off, or `arecord -l`, which names the
// capture device but not the port.
func parseAlsaCards(text string) []alsaCard {
	var cards []alsaCard
	seen := map[int]bool{}
	// The line naming a card's USB port belongs to the heading directly above
	// it and to no other, or a heading that failed to read would hand its port
	// to the card before it, and that card would be paired with the wrong sound.
	headingAbove := -1
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		above := headingAbove
		headingAbove = -1
		if match := asoundCardPattern.FindStringSubmatch(line); match != nil {
			index, _ := strconv.Atoi(match[1])
			if !seen[index] {
				seen[index] = true
				cards = append(cards, alsaCard{index: index, id: match[2], name: strings.TrimSpace(match[3])})
				headingAbove = len(cards) - 1
			}
			continue
		}
		if match := asoundBusPattern.FindStringSubmatch(line); match != nil {
			if above >= 0 {
				cards[above].bus = match[1]
			}
			continue
		}
		if match := arecordCardPattern.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			index, _ := strconv.Atoi(match[1])
			device, _ := strconv.Atoi(match[4])
			if seen[index] {
				continue
			}
			seen[index] = true
			cards = append(cards, alsaCard{index: index, id: match[2], name: strings.TrimSpace(match[3]), device: device})
		}
	}
	return cards
}

// pairCaptureCards joins each video device to the sound card on the same USB
// port. A card whose port cannot be matched keeps an empty audio device, and
// every sound card left over is offered for pairing by hand, so nothing is
// paired by guesswork.
func pairCaptureCards(videos []v4l2Device, sounds []alsaCard) ([]captureCard, []captureAudioChoice) {
	claimed := map[int]bool{}
	cards := make([]captureCard, 0, len(videos))
	for _, video := range videos {
		card := captureCard{Name: video.name, Bus: video.bus, Video: video.video, AudioCard: -1, Width: 1920, Height: 1080, Framerate: 60}
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
// where a slash, a space, or a stray character would break the address.
func validCaptureCard(card captureCard) error {
	if !captureVideoPattern.MatchString(card.Video) {
		return fmt.Errorf("the video device must look like video0, as v4l2-ctl lists it")
	}
	if !captureAudioPattern.MatchString(card.Audio) {
		return fmt.Errorf("the audio device must look like hw:Video,0, as arecord -l lists it")
	}
	if card.Width < 320 || card.Width > 7680 || card.Height < 240 || card.Height > 4320 {
		return fmt.Errorf("the picture size must be between 320x240 and 7680x4320")
	}
	if card.Framerate < 1 || card.Framerate > 120 {
		return fmt.Errorf("the frame rate must be between 1 and 120")
	}
	return nil
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
// sound device.
func captureSourceText(cards []captureCard) string {
	var text strings.Builder
	text.WriteString("#EXTM3U\n")
	for _, card := range cards {
		fmt.Fprintf(&text, "\n#EXTINF:-1 channel-id=\"ah4c-capture-%s\",%s\n", card.Video, captureChannelName(card))
		fmt.Fprintf(&text, "capture://v4l2/%s/%s/?framerate=%d&width=%d&height=%d\n", card.Video, card.Audio, card.Framerate, card.Width, card.Height)
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

func readCaptureCardsHandler(c *gin.Context) {
	var request struct {
		Devices string `json:"devices"`
		Sound   string `json:"sound"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCaptureRequestBytes)
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Paste the two listings as text."})
		return
	}
	// The listing command prints the video devices, then "# ah4c sound", then
	// the sound cards, so one paste carries both.
	devices, sound := request.Devices, request.Sound
	if before, after, found := strings.Cut(devices, "# ah4c sound"); found {
		devices = before
		if strings.TrimSpace(sound) == "" {
			sound = after
		}
	}
	drivers := parseCaptureMarkers(request.Devices)
	videos := parseV4L2Devices(devices)
	if len(videos) == 0 && len(drivers.MissingVideo) == 0 && len(drivers.MissingSound) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No video devices were found in the first listing. Paste everything the listing command, or v4l2-ctl --list-devices, prints."})
		return
	}
	sounds := parseAlsaCards(sound)
	cards, left := pairCaptureCards(videos, sounds)
	// Every sound card is offered with its card number, so a card paired or
	// changed by hand still lists the right Proxmox devices to pass through.
	known := make([]captureAudioChoice, 0, len(sounds))
	for _, sound := range sounds {
		known = append(known, captureAudioChoice{Card: sound.index, Audio: fmt.Sprintf("hw:%s,%d", sound.id, sound.device), Name: sound.name, Bus: sound.bus})
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
	return gin.H{"present": present, "source": captureSourceName, "cards": cards, "streams": streams, "unreadLines": unread}, nil
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
	payload := map[string]string{
		"name": captureSourceName, "type": "HLS", "source": "Text", "url": "",
		"text": captureSourceText(cards), "refresh": "", "limit": "", "satip": "",
		"numbering": "ignore", "logos": "", "xmltv_url": "", "xmltv_refresh": "",
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
	return nil
}

func putCaptureSourceHandler(c *gin.Context) {
	var request struct {
		Cards []captureCard `json:"cards"`
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
		if err := validCaptureCard(card); err != nil {
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
		c.JSON(http.StatusOK, gin.H{"written": true, "present": true, "source": captureSourceName, "cards": request.Cards, "streams": []captureStream{},
			"otherSources": []string{}, "uncheckedSources": 0, "unreadLines": 0, "message": message})
		return
	}
	// The rest of Channels DVR is scanned once, after the wait, not on every look.
	if err := addOtherCaptureSources(ctx, dvrBase, setup); err != nil {
		setup["otherSources"] = []string{}
		setup["uncheckedSources"] = 0
		setup["message"] = "Other sources in Channels DVR could not be checked for the same cards: " + err.Error() + "."
	}
	setup["written"] = true
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
	if strings.Contains(address, "/devices/"+captureDeviceID+"/channels/") {
		reason += " Channels DVR could not capture from the card. Check that it sees the card, with ls -la /dev/video* /dev/snd inside its container, and that the server has the card's drivers."
	}
	c.JSON(http.StatusOK, gin.H{"ok": false, "definite": true, "reason": reason})
}
