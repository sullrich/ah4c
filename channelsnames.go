package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Activity names a stream the way Channels DVR does. Channels DVR can reach
// ah4c through another program's M3U — FastChannels is one — so the channel a
// tune asks for may be in no M3U ah4c holds, and even when it is, the list
// Channels DVR reads may call it something else. Channels DVR does not say
// which address a channel plays from, so every M3U source it has is read only
// to link each ah4c play URL to the channel ID Channels DVR knows it by; the
// name is the guide name Channels DVR gives that channel, never the M3U's.

const (
	channelsSourceNamesTTL   = 10 * time.Minute
	channelsSourceNamesRetry = 30 * time.Second
)

var channelsSourceNames struct {
	sync.Mutex
	names   map[string]string
	fetched time.Time
}

// channelsSourceNamesRefresh lets one refresh run at a time; the others wait
// for it and read what it found.
var channelsSourceNamesRefresh sync.Mutex

func registerChannelsNameRoutes(r *gin.Engine) {
	r.GET("/api/channels/names", getChannelsNamesHandler)
}

// tunerChannelFromStreamURL returns the channel an ah4c play URL tunes, or ""
// when the URL is not one. The last segment is the channel and the one before
// it is "tuner" or "tunerN", as in the /play/tuner:tuner/:channel route.
func tunerChannelFromStreamURL(stream string) string {
	parsed, err := url.Parse(strings.TrimSpace(stream))
	if err != nil {
		return ""
	}
	segments := strings.Split(parsed.Path, "/")
	n := len(segments)
	if n < 3 || segments[n-3] != "play" || !strings.HasPrefix(segments[n-2], "tuner") {
		return ""
	}
	return segments[n-1]
}

var extinfAttribute = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// extinfEntry reads the channel ID and title from an #EXTINF line. The title
// follows the first comma outside the quoted attributes, so a title that has
// commas of its own ("Foo Fighters, MSG, 2021") is kept whole. Channels DVR
// takes a channel's ID from channel-id, or from tvg-id when there is none.
func extinfEntry(line string) (id, title string) {
	quoted := false
	for index, char := range line {
		if char == '"' {
			quoted = !quoted
		} else if char == ',' && !quoted {
			title = strings.TrimSpace(line[index+1:])
			line = line[:index]
			break
		}
	}
	attributes := map[string]string{}
	for _, match := range extinfAttribute.FindAllStringSubmatch(line, -1) {
		attributes[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
	}
	id = attributes["channel-id"]
	if id == "" {
		id = attributes["tvg-id"]
	}
	return id, title
}

// readChannelsM3UNames adds, for every ah4c play URL in an M3U, the guide name
// Channels DVR gave that entry's channel ID, keeping the first name found for a
// channel. An entry Channels DVR has no name for is left out.
func readChannelsM3UNames(body io.Reader, guideNames, names map[string]string) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	id := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			id, _ = extinfEntry(line)
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			name := ""
			if id != "" {
				name = guideNames[id]
			}
			if channel := tunerChannelFromStreamURL(line); channel != "" && name != "" {
				if _, seen := names[channel]; !seen {
					names[channel] = name
				}
			}
			id = ""
		}
	}
	return scanner.Err()
}

func readChannelsSourceM3U(source channelsM3USourceSettings, guideNames, names map[string]string) error {
	if strings.TrimSpace(source.URL) == "" {
		return readChannelsM3UNames(strings.NewReader(source.Text), guideNames, names)
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelsRecordingRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", source.URL, response.Status)
	}
	return readChannelsM3UNames(response.Body, guideNames, names)
}

func fetchChannelsSourceNames() (map[string]string, error) {
	baseURL, err := channelsDVRBaseURL()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelsRecordingRequestTimeout)
	defer cancel()
	devices, err := fetchChannelsDVRDevices(ctx, baseURL+"/devices")
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	for _, device := range devices {
		key := strings.TrimPrefix(device.DeviceID, channelsM3UDevicePrefix)
		if !strings.HasPrefix(device.DeviceID, channelsM3UDevicePrefix) || !usableChannelsSourceKey(key) {
			continue
		}
		var source channelsM3USourceSettings
		if err := fetchChannelsDVRJSON(ctx, baseURL+"/providers/m3u/sources/"+url.PathEscape(key), &source); err != nil {
			logger("[CHANNELS] Could not read the %s source for channel names: %v", key, err)
			continue
		}
		guideNames := make(map[string]string, len(device.Channels))
		for _, channel := range device.Channels {
			guideNames[channel.ID] = channel.GuideName
		}
		if err := readChannelsSourceM3U(source, guideNames, names); err != nil {
			logger("[CHANNELS] Could not read the %s M3U for channel names: %v", key, err)
		}
	}
	return names, nil
}

// channelsNamesFor returns the names it knows for the channels asked about,
// reading Channels DVR's sources again when they are old or when a channel is
// missing and the last read was not just now.
func channelsNamesFor(channels []string) map[string]string {
	lookup := func() (map[string]string, bool) {
		channelsSourceNames.Lock()
		defer channelsSourceNames.Unlock()
		found := make(map[string]string)
		missing := false
		for _, channel := range channels {
			if name := channelsSourceNames.names[channel]; name != "" {
				found[channel] = name
			} else {
				missing = true
			}
		}
		age := time.Since(channelsSourceNames.fetched)
		return found, age > channelsSourceNamesTTL || (missing && age > channelsSourceNamesRetry)
	}
	found, stale := lookup()
	if !stale {
		return found
	}
	channelsSourceNamesRefresh.Lock()
	defer channelsSourceNamesRefresh.Unlock()
	if found, stale = lookup(); !stale {
		return found
	}
	names, err := fetchChannelsSourceNames()
	channelsSourceNames.Lock()
	channelsSourceNames.fetched = time.Now()
	if err == nil {
		channelsSourceNames.names = names
	}
	channelsSourceNames.Unlock()
	if err != nil {
		logger("[CHANNELS] Could not read channel names from Channels DVR: %v", err)
	}
	found, _ = lookup()
	return found
}

func getChannelsNamesHandler(c *gin.Context) {
	channels := c.QueryArray("channel")
	if len(channels) == 0 {
		c.JSON(http.StatusOK, gin.H{"names": gin.H{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"names": channelsNamesFor(channels)})
}
