package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// channelsM3USourceSettings is one custom-channels source as Channels DVR
// returns it from /providers/m3u/sources/<key>. Only the fields that say which
// list the source reads and what to call it are taken.
type channelsM3USourceSettings struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Text is the list itself for a source typed into Channels DVR rather
	// than read from an address, which is how a capture card is set up.
	Text string `json:"text"`
}

// channelsM3USourceMatch is a source of this DVR that reads one of our lists.
// The key is the path segment Channels DVR answers to, which is not always the
// name it shows: a source can be called anything.
type channelsM3USourceMatch struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

const maxM3UUploadBytes int64 = 16 << 20

// maxM3UParseBytes bounds the health check. Anything larger than an upload is
// allowed to be is not a channel list, and is reported unchecked rather than
// read.
const maxM3UParseBytes int64 = maxM3UUploadBytes

// m3uParseResult is what the health check made of one channel list.
type m3uParseResult struct {
	File    string `json:"file"`
	OK      bool   `json:"ok"`
	Checked bool   `json:"checked"`
	Error   string `json:"error,omitempty"`
}

func registerM3UIntegrationRoutes(r *gin.Engine) {
	r.POST("/api/m3u/add-to-channels", addM3UToChannelsHandler)
	r.POST("/api/m3u/reload-in-channels", reloadM3UInChannelsHandler)
	r.GET("/api/m3u/health", m3uHealthHandler)
	r.POST("/api/m3u/files", uploadM3UFileHandler)
	r.DELETE("/api/m3u/files/:file", deleteM3UFileHandler)
}

// m3uTemplateError parses one channel list the way the /m3u/:channel route
// does, and returns the parser's own message when it will not parse. gin's
// LoadHTMLGlob builds the set with
// template.New("").Delims("{{", "}}").Funcs(engine.FuncMap).ParseGlob(pattern)
// and parses each file under its base name; main.go never calls SetFuncMap, so
// that FuncMap is empty. Because the whole folder is one template set, a single
// file that will not parse takes every channel list down with it.
func m3uTemplateError(name string, content []byte) string {
	parsed, err := template.New(name).Delims("{{", "}}").Parse(string(content))
	if err != nil {
		return err.Error()
	}
	// Every list shares one template set under gin, so a list that defines a
	// template under another list's name would quietly replace that list.
	for _, defined := range parsed.Templates() {
		if defined.Name() != name {
			return fmt.Sprintf("defines a template named %q, which would replace another channel list", defined.Name())
		}
	}
	return ""
}

// checkM3UFolder looks at every entry gin's LoadHTMLGlob("m3u/*.m3u") would,
// the same way: the glob matches directories and links too, and gin reads each
// match through the link and fails on a directory. A folder with no matches at
// all is also a failure for gin, so the caller is told when the list is empty.
func checkM3UFolder(dir string) ([]m3uParseResult, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.m3u"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	results := []m3uParseResult{}
	for _, path := range matches {
		name := filepath.Base(path)
		info, err := os.Stat(path)
		if err != nil {
			results = append(results, m3uParseResult{File: name, Error: fmt.Sprintf("This entry could not be read: %v", err)})
			continue
		}
		if info.IsDir() {
			results = append(results, m3uParseResult{File: name, Checked: true, Error: "This is a folder, not a file. Every channel list address fails while it is here."})
			continue
		}
		if info.Size() > maxM3UParseBytes {
			results = append(results, m3uParseResult{File: name, Error: fmt.Sprintf("This file is larger than %s, so it was not checked.", byteCount(maxM3UParseBytes))})
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			results = append(results, m3uParseResult{File: name, Error: fmt.Sprintf("This file could not be read: %v", err)})
			continue
		}
		message := m3uTemplateError(name, content)
		results = append(results, m3uParseResult{File: name, OK: message == "", Checked: true, Error: message})
	}
	return results, nil
}

func m3uHealthHandler(c *gin.Context) {
	results, err := checkM3UFolder("m3u")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not read the M3U folder: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"files": results, "empty": len(results) == 0})
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
	name, err = validManagedM3UFile(name)
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
	// A list that will not parse stops every channel-list address from loading,
	// so it never reaches the folder. The staged file is removed on the way out.
	staged, err := os.ReadFile(temporaryName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not read the staged upload: %v", err)})
		return
	}
	if message := m3uTemplateError(name, staged); message != "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      fmt.Sprintf("%s was not saved because AH4C cannot read it as a channel list: %s. AH4C fills in its own address where a list asks for it, so while a file with broken template text is in the m3u folder, no channel-list address loads at all. Correct that text and upload the file again.", name, message),
			"file":       name,
			"parseError": message,
		})
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
	name, err := validManagedM3UFile(c.Param("file"))
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

	ctx, release := channelsCallContext(c.Request.Context(), 12*time.Second)
	defer release()
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

func validManagedM3UFile(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || filepath.Base(value) != value || !strings.HasSuffix(value, ".m3u") {
		return "", fmt.Errorf("M3U filenames must use a plain filename ending in lowercase .m3u")
	}
	return value, nil
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

// Channels DVR lists a custom-channels source among its devices under this
// prefix, so "M3U-DirecTV" is the device of the source keyed "DirecTV".
const channelsM3UDevicePrefix = "M3U-"

// Bounded, because a reply that never ends must not become memory that never
// stops, but large enough for every source's lineup on a full DVR.
const channelsDeviceListLimit int64 = 32 << 20

// channelsCallContext bounds a management call to Channels DVR and cancels it
// once a tune is visible. The tune owns the machine while it is in flight; a
// call made on someone's behalf gives way rather than competing with it. The
// watcher reads the tuner state, which a starting tune holds locked until its
// command returns, so the cancel lands within a tick of the tune becoming
// visible and not within a tick of it being asked for.
func channelsCallContext(parent context.Context, timeout time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	stop := make(chan struct{})
	var once sync.Once
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
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	// Released once however many times it is called, because a close that runs
	// twice takes the whole process down and every tune with it.
	return ctx, func() {
		once.Do(func() {
			close(stop)
			cancel()
		})
	}
}

// reloadM3UInChannelsHandler asks Channels DVR to read one channel list again.
// Channels DVR keeps its own copy of every list and re-reads it on a schedule
// of its own, so an edit saved here is not in the guide until the source is
// reloaded. This is the call the Reload M3U button in its admin makes.
//
// Every source found is accounted for in the reply: the ones now reading the
// list again, the ones that would not, and the ones that could not even be
// looked at. A source left out of the answer is a source quietly serving
// yesterday's list, which is the failure this endpoint exists to end.
func reloadM3UInChannelsHandler(c *gin.Context) {
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
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "A tune is running, so Channels DVR was not asked to read the list again."})
		return
	}
	dvrBase, err := channelsDVRBaseURL()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Set the Channels DVR address in Settings to have it read a saved list again."})
		return
	}
	// Every list in the folder is one template set, so one list that will not
	// parse stops this one loading too. Channels DVR would be handed an error,
	// and the page would say it is reading the list again when it cannot.
	if message := m3uFolderBlocksReload(m3uReloadFolder); message != "" {
		c.JSON(http.StatusConflict, gin.H{"error": message})
		return
	}
	ctx, release := channelsCallContext(c.Request.Context(), 12*time.Second)
	defer release()
	sources, unchecked, err := channelsM3USourcesServingList(ctx, dvrBase, file)
	if err != nil {
		if !configOperationsAllowed() {
			c.JSON(http.StatusConflict, gin.H{"error": "A tune started, so Channels DVR was not asked to read the list again."})
			return
		}
		logger("[M3U] %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	reloaded := []string{}
	failed := []string{}
	reason := ""
	stopped := false
	for index, source := range sources {
		// A context that is already finished cannot carry a request, so the
		// round ends here rather than posting into it and calling the answer a
		// refusal from Channels DVR.
		if ctx.Err() != nil {
			stopped = true
			for _, left := range sources[index:] {
				failed = append(failed, left.Name)
			}
			break
		}
		if err := refreshChannelsM3USource(ctx, http.DefaultClient, dvrBase, source.Key); err != nil {
			if ctx.Err() != nil {
				// The machine was taken back mid-way. The sources not reached
				// are named rather than forgotten, so nobody is left believing
				// a list was reloaded that never was.
				stopped = true
				for _, left := range sources[index:] {
					failed = append(failed, left.Name)
				}
				break
			}
			logger("[M3U] Channels DVR would not reload %s for %s: %v", source.Name, file, err)
			failed = append(failed, source.Name)
			if reason == "" {
				reason = err.Error()
			}
			continue
		}
		reloaded = append(reloaded, source.Name)
	}
	if stopped {
		// Why the rest were left alone outranks whatever one source said
		// before it, because it is the part nobody can act on otherwise.
		if configOperationsAllowed() {
			reason = "Channels DVR took too long, so the rest of the sources were left as they were."
		} else {
			reason = "A tune started, so the rest of the sources were left as they were."
		}
	}
	status := "ok"
	switch {
	case len(reloaded) == 0 && len(failed) == 0 && len(unchecked) == 0:
		status = "none"
	case len(failed) > 0 || len(unchecked) > 0:
		status = "partial"
	}
	code := http.StatusOK
	if len(reloaded) == 0 && len(failed) > 0 {
		code = http.StatusBadGateway
		if stopped {
			code = http.StatusConflict
		}
	}
	if len(reloaded) > 0 {
		logger("[M3U] asked Channels DVR at %s to read %s again for %s", dvrBase, strings.Join(reloaded, ", "), file)
	}
	if len(unchecked) > 0 {
		logger("[M3U] %s in Channels DVR could not be looked at for %s: %s", countOfSources(len(unchecked)), file, strings.Join(unchecked, ", "))
	}
	body := gin.H{"status": status, "file": file, "sources": reloaded, "failed": failed, "unchecked": unchecked}
	if reason != "" {
		body["error"] = reason
	}
	c.JSON(code, body)
}

// m3uReloadFolder is where the lists Channels DVR fetches live, the folder the
// /m3u/:channel route loads as one template set.
var m3uReloadFolder = "m3u"

// m3uFolderBlocksReload names the lists that would stop Channels DVR from
// reading any list at all, or returns "" when every one parses. It is the same
// check the Channel M3Us page shows, run before anyone is asked to fetch.
func m3uFolderBlocksReload(dir string) string {
	results, err := checkM3UFolder(dir)
	if err != nil {
		return fmt.Sprintf("Channels DVR was not asked to read the list again, because the m3u folder could not be checked: %v.", err)
	}
	broken := []string{}
	for _, result := range results {
		if result.Checked && !result.OK {
			broken = append(broken, result.File)
		}
	}
	if len(broken) == 0 {
		return ""
	}
	still := "they are"
	if len(broken) == 1 {
		still = "it is"
	}
	return fmt.Sprintf("Channels DVR was not asked to read the list again, because %s will not load as a channel list, and while %s in the m3u folder no channel-list address loads at all. The Channel M3Us page says what is wrong.",
		strings.Join(broken, ", "), still)
}

func countOfSources(total int) string {
	if total == 1 {
		return "1 source"
	}
	return fmt.Sprintf("%d sources", total)
}

// channelsM3USourcesServingList finds the custom-channels sources of this DVR
// that read one of our channel lists. A source can be named anything, and the
// path segment it answers to is not always what it is called, so each candidate
// device is resolved to its own settings and judged by the address it reads.
//
// A source whose settings will not answer is returned as unchecked, never
// dropped: it may be the very source that reads this list, and "nothing needed
// reloading" would then be a lie told in the color of success.
func channelsM3USourcesServingList(ctx context.Context, dvrBase, file string) ([]channelsM3USourceMatch, []string, error) {
	base := strings.TrimRight(dvrBase, "/")
	devices, err := fetchChannelsDVRDevices(ctx, base+"/devices")
	if err != nil {
		return nil, nil, fmt.Errorf("Could not read the sources of Channels DVR at %s: %w", dvrBase, err)
	}
	sources := []channelsM3USourceMatch{}
	unchecked := []string{}
	for _, device := range devices {
		if !strings.HasPrefix(device.DeviceID, channelsM3UDevicePrefix) {
			continue
		}
		key := strings.TrimPrefix(device.DeviceID, channelsM3UDevicePrefix)
		if !usableChannelsSourceKey(key) {
			unchecked = append(unchecked, strings.TrimSpace(device.DeviceID))
			continue
		}
		var settings channelsM3USourceSettings
		if err := fetchChannelsDVRJSON(ctx, base+"/providers/m3u/sources/"+url.PathEscape(key), &settings); err != nil {
			// A tune that arrives mid-search ends the search rather than
			// quietly shortening it.
			if ctx.Err() != nil {
				return nil, nil, fmt.Errorf("Could not read the sources of Channels DVR at %s: %w", dvrBase, err)
			}
			logger("[M3U] could not read the Channels DVR source %s: %v", key, err)
			unchecked = append(unchecked, key)
			continue
		}
		if !channelsSourceServesList(settings.URL, file) {
			continue
		}
		name := strings.TrimSpace(settings.Name)
		if name == "" {
			name = key
		}
		sources = append(sources, channelsM3USourceMatch{Key: key, Name: name})
	}
	return sources, unchecked, nil
}

// usableChannelsSourceKey keeps a key that cannot be put in a URL at all out of
// one. A slash is fine: url.PathEscape carries it as %2F and Channels DVR
// answers to it, so a source named for two things is still reachable. Channels
// DVR supplies these keys itself, so this guards against the unexpected rather
// than against an attacker, and a key it refuses is reported as unchecked
// rather than passed over in silence.
func usableChannelsSourceKey(key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	for _, char := range key {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

// fetchChannelsDVRDevices reads the device list, which carries every source's
// whole channel lineup and so runs to megabytes on a DVR with a large list. It
// has a cap of its own because the shared one is a megabyte, which a single
// full lineup can already pass.
func fetchChannelsDVRDevices(ctx context.Context, requestURL string) ([]channelsDVRDevice, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "ah4c")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Channels DVR returned %s", response.Status)
	}
	var devices []channelsDVRDevice
	if err := json.NewDecoder(io.LimitReader(response.Body, channelsDeviceListLimit)).Decode(&devices); err != nil {
		return nil, fmt.Errorf("could not read Channels DVR response: %w", err)
	}
	return devices, nil
}

// channelsSourceServesList reports whether a Channels DVR source reads one of
// our channel lists. Only the path is compared: the address typed into Channels
// DVR is often another name for this host than IPADDRESS, and a proxy in front
// of AH4C can add a prefix of its own.
func channelsSourceServesList(sourceURL, file string) bool {
	if strings.TrimSpace(file) == "" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || parsed.Path == "" {
		return false
	}
	return strings.HasSuffix(path.Clean(parsed.Path), "/m3u/"+file)
}

// refreshChannelsM3USource asks Channels DVR to read one custom-channels source
// again. A reply here means the request was accepted; the re-read itself runs
// in the background over there.
func refreshChannelsM3USource(ctx context.Context, client *http.Client, dvrBase, key string) error {
	requestURL := strings.TrimRight(dvrBase, "/") + "/providers/m3u/sources/" + url.PathEscape(key) + "/refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, nil)
	if err != nil {
		return err
	}
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
