package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type addM3UToChannelsRequest struct {
	File string `json:"file"`
}

type channelsM3USource struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Source       string `json:"source"`
	URL          string `json:"url"`
	Refresh      string `json:"refresh"`
	Limit        string `json:"limit"`
	SATIP        string `json:"satip"`
	Numbering    string `json:"numbering"`
	Logos        string `json:"logos"`
	XMLTVURL     string `json:"xmltv_url"`
	XMLTVRefresh string `json:"xmltv_refresh"`
}

func registerM3UIntegrationRoutes(r *gin.Engine) {
	r.POST("/api/m3u/add-to-channels", addM3UToChannelsHandler)
}

func addM3UToChannelsHandler(c *gin.Context) {
	var request addM3UToChannelsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	file, err := validM3UFile(request.File)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, err := os.Stat(filepath.Join("m3u", file)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "that channel list is not available"})
		return
	}
	envEngineMu.RLock()
	selectionLocked := envLocked["CHANNELS_M3U"]
	lockedSelection := os.Getenv("CHANNELS_M3U")
	envEngineMu.RUnlock()
	if selectionLocked && lockedSelection != file {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("The channel-list choice is set by the environment to %s. Change CHANNELS_M3U there before choosing a different list.", lockedSelection)})
		return
	}
	if !connectionChecksAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "Wait until the current tune finishes before adding a channel list to Channels DVR."})
		return
	}
	dvrBase, err := serverBaseURL(os.Getenv("CHANNELSIP"), "8089")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Set the Channels DVR host in Settings before adding a channel list."})
		return
	}
	proxyAddress := strings.TrimSpace(os.Getenv("IPADDRESS"))
	proxyBase, err := serverBaseURL(proxyAddress, "7654")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Set the ah4c address in Settings before adding a channel list."})
		return
	}
	m3uURL := proxyBase + "/m3u/" + url.PathEscape(file)
	sourceName := "AH4C - " + strings.TrimSuffix(file, filepath.Ext(file))

	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	stopWatcher := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !connectionChecksAllowed() {
					cancel()
					return
				}
			case <-stopWatcher:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	defer close(stopWatcher)
	if err := putChannelsM3USource(ctx, http.DefaultClient, dvrBase, sourceName, m3uURL); err != nil {
		if !connectionChecksAllowed() {
			c.JSON(http.StatusConflict, gin.H{"error": "A tune started, so adding the channel list was stopped. Try again after the tune finishes.", "url": m3uURL})
			return
		}
		logger("[M3U] Channels DVR rejected %s: %v", file, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "url": m3uURL})
		return
	}
	persisted, persistMessage := rememberChannelsM3U(file)
	logger("[M3U] added %s to Channels DVR at %s", file, dvrBase)
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "file": file, "url": m3uURL, "source": sourceName,
		"dvr": dvrBase, "persisted": persisted, "message": persistMessage,
	})
}

func validM3UFile(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.Base(value) != value || !strings.HasSuffix(strings.ToLower(value), ".m3u") {
		return "", fmt.Errorf("choose an M3U channel list")
	}
	return value, nil
}

func serverBaseURL(value, defaultPort string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("server address is empty")
	}
	if !strings.Contains(value, "://") {
		if net.ParseIP(value) != nil && strings.Contains(value, ":") {
			value = "http://[" + value + "]"
		} else {
			value = "http://" + value
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid server address")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("server address must not include a path")
	}
	if err := validateURLPort(parsed); err != nil {
		return "", err
	}
	port := parsed.Port()
	if port == "" {
		port = defaultPort
	}
	return parsed.Scheme + "://" + net.JoinHostPort(parsed.Hostname(), port), nil
}

func m3uTemplateAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("ah4c address is empty")
	}
	if !strings.Contains(value, "://") {
		if err := validateBareHostPort(value); err != nil {
			return "", err
		}
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid ah4c address")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("ah4c address must not include a path")
	}
	if err := validateURLPort(parsed); err != nil {
		return "", err
	}
	return parsed.Host, nil
}

func putChannelsM3USource(ctx context.Context, client *http.Client, dvrBase, sourceName, m3uURL string) error {
	payload := channelsM3USource{
		Name: sourceName, Type: "MPEG-TS", Source: "URL", URL: m3uURL, Refresh: "24",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(dvrBase, "/")+"/providers/m3u/sources/AH4C", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ah4c")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Could not connect to Channels DVR at %s: %w", dvrBase, err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(responseBody))
		if detail == "" {
			detail = resp.Status
		}
		return fmt.Errorf("Channels DVR returned %s: %s", resp.Status, detail)
	}
	return nil
}

// rememberChannelsM3U records only a successful Channels DVR update. The
// setting gives the UI a durable, honest answer to "which list did I add?"
func rememberChannelsM3U(file string) (bool, string) {
	configAPIMu.Lock()
	defer configAPIMu.Unlock()
	envEngineMu.RLock()
	settings := copySettings(envSettings)
	locked := envLocked["CHANNELS_M3U"]
	envEngineMu.RUnlock()
	if locked {
		return false, "The saved channel-list choice is controlled by the environment."
	}
	settings.Vars["CHANNELS_M3U"] = file
	if err := saveSettings(settings); err != nil {
		logger("[M3U] Channels DVR was updated, but the selected list could not be saved: %v", err)
		return false, "Channels DVR was updated, but this choice could not be saved locally."
	}
	envEngineMu.Lock()
	envSettings = settings
	envEngineMu.Unlock()
	_ = os.Setenv("CHANNELS_M3U", file)
	return true, ""
}
