package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const maxM3UUploadBytes int64 = 16 << 20

func registerM3UIntegrationRoutes(r *gin.Engine) {
	r.POST("/api/m3u/add-to-channels", addM3UToChannelsHandler)
	r.POST("/api/m3u/files", uploadM3UFileHandler)
	r.DELETE("/api/m3u/files/:file", deleteM3UFileHandler)
}

func uploadM3UFileHandler(c *gin.Context) {
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "Wait until the current tune finishes before uploading a channel list."})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxM3UUploadBytes+(1<<20))
	header, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Choose an M3U channel-list file to upload."})
		return
	}
	name := filepath.Base(filepath.ToSlash(header.Filename))
	name, err = validM3UFile(name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := os.MkdirAll("m3u", 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not prepare the M3U folder: %v", err)})
		return
	}
	target := filepath.Join("m3u", name)
	replaced := false
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			c.JSON(http.StatusConflict, gin.H{"error": "The destination exists but is not a regular file."})
			return
		}
		replaced = true
	} else if !os.IsNotExist(statErr) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not inspect the destination: %v", statErr)})
		return
	}
	source, err := header.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Could not read the upload: %v", err)})
		return
	}
	defer source.Close()
	temporary, err := os.CreateTemp("m3u", ".m3u-upload-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not stage the upload: %v", err)})
		return
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		temporary.Close()
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	written, copyErr := io.Copy(temporary, io.LimitReader(source, maxM3UUploadBytes+1))
	if copyErr != nil || written > maxM3UUploadBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "The M3U file is larger than the 16 MB upload limit or could not be read."})
		return
	}
	if err := temporary.Sync(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not finish the upload: %v", err)})
		return
	}
	if err := temporary.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not finish the upload: %v", err)})
		return
	}
	if err := os.Chmod(temporaryName, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not set M3U file permissions: %v", err)})
		return
	}
	if err := os.Rename(temporaryName, target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not activate the uploaded M3U: %v", err)})
		return
	}
	committed = true
	logger("[M3U] uploaded %s (%s)", target, byteCount(written))
	c.JSON(http.StatusCreated, gin.H{"status": "ok", "file": name, "replaced": replaced, "size": written})
}

func deleteM3UFileHandler(c *gin.Context) {
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "Wait until the current tune finishes before deleting a channel list."})
		return
	}
	name, err := validM3UFile(c.Param("file"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	target := filepath.Join("m3u", name)
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "That channel list is not available."})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	if !info.Mode().IsRegular() {
		c.JSON(http.StatusConflict, gin.H{"error": "The selected path is not a regular M3U file."})
		return
	}
	envEngineMu.RLock()
	lockedSelection := envLocked["CHANNELS_M3U"] && os.Getenv("CHANNELS_M3U") == name
	envEngineMu.RUnlock()
	if lockedSelection {
		c.JSON(http.StatusConflict, gin.H{"error": "This channel list is selected by the container environment. Change CHANNELS_M3U before deleting it."})
		return
	}
	if err := os.Remove(target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not delete the M3U file: %v", err)})
		return
	}
	forgetRememberedM3U(name)
	logger("[M3U] deleted %s", target)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "file": name})
}

func forgetRememberedM3U(file string) {
	configAPIMu.Lock()
	defer configAPIMu.Unlock()
	envEngineMu.RLock()
	settings := copySettings(envSettings)
	locked := envLocked["CHANNELS_M3U"]
	envEngineMu.RUnlock()
	if locked || settings.Vars["CHANNELS_M3U"] != file {
		return
	}
	delete(settings.Vars, "CHANNELS_M3U")
	if err := saveSettings(settings); err != nil {
		logger("[M3U] deleted %s but could not clear its saved selection: %v", file, err)
		return
	}
	envEngineMu.Lock()
	envSettings = settings
	envEngineMu.Unlock()
	_ = os.Unsetenv("CHANNELS_M3U")
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
	if !configOperationsAllowed() {
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
				if !configOperationsAllowed() {
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
		if !configOperationsAllowed() {
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
