package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// allM3UMarker is written into the header of every file this page generates,
// so a generated file never offers itself back as a source on the next run.
const allM3UMarker = "# ah4c-allm3u-generated"

type allM3USource struct {
	File     string `json:"file"`
	Device   string `json:"device"`
	Provider string `json:"provider"`
}

type allM3URequest struct {
	Output  string         `json:"output"`
	Sources []allM3USource `json:"sources"`
}

type scriptInstallRequest struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// allM3UScriptSources lists the device/provider folders the page offers, the
// way Settings does: local folders on this server, the ones included with
// ah4c, and the ones found on GitHub by the last check. Each is a
// scripts/device/provider path, or scripts/name for a script folder directly
// under scripts; all/all is left out because it is the dispatcher that reads
// the finished list, not a place to send a channel.
func allM3UScriptSources() map[string][]string {
	pick := func(paths []string) []string {
		result := []string{}
		for _, path := range paths {
			parts := strings.Split(path, "/")
			if (len(parts) == 2 || len(parts) == 3) && parts[0] == "scripts" && parts[1] != "all" {
				result = append(result, path)
			}
		}
		sort.Strings(result)
		return result
	}
	local := localTopLevelScriptFolders()
	for device, providers := range scriptDeviceProviders() {
		for _, provider := range providers {
			local = append(local, "scripts/"+device+"/"+provider)
		}
	}
	return map[string][]string{"local": pick(local), "bundled": pick(discoverBundledStreamers()), "github": pick(loadStreamerCache())}
}

// localTopLevelScriptFolders lists script folders directly under ./scripts,
// such as scripts/mine, which Settings also offers as a streaming app.
func localTopLevelScriptFolders() []string {
	var result []string
	entries, err := os.ReadDir("scripts")
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() && validScriptPathPart(entry.Name()) && entry.Name() != "all" && holdsTuneScript(filepath.Join("scripts", entry.Name())) {
			result = append(result, "scripts/"+entry.Name())
		}
	}
	return result
}

// installBundledScriptPackage puts a folder included with ah4c into ./scripts.
// Like every other install, a folder already there only gains missing files.
func installBundledScriptPackage(selection string) error {
	source := filepath.Join(bundledScriptsRoot, strings.TrimPrefix(selection, "scripts/"))
	if !scriptPackageComplete(source) {
		return fmt.Errorf("%s is not included with this version of ah4c", selection)
	}
	root, err := localScriptsRoot()
	if err != nil {
		return fmt.Errorf("could not locate the local scripts folder: %w", err)
	}
	target := filepath.Join(root, strings.TrimPrefix(selection, "scripts/"))
	if err := os.MkdirAll(target, 0755); err != nil {
		return fmt.Errorf("could not prepare %s: %w", selection, err)
	}
	return addMissingScriptFiles(source, target)
}

// scriptDeviceProviders scans scripts/<device>/<provider> and returns the
// device/provider pairs the "all/all" dispatcher (scripts/all/all/bmitune.sh)
// can route to from the live scripts bind mount.
func scriptDeviceProviders() map[string][]string {
	result := map[string][]string{}
	mergeScriptDeviceProviders(result, "scripts")
	for device, providers := range result {
		sort.Strings(providers)
		result[device] = providers
	}
	return result
}

// allM3UDeviceProviders adds the cached list from the last explicit "Check for
// scripts" action. Listing a package never downloads it; the page marks which
// packages are local and offers an explicit, selected-package download.
func allM3UDeviceProviders() map[string][]string {
	result := scriptDeviceProviders()
	for _, selection := range discoverStreamers() {
		parts := strings.Split(selection, "/")
		if len(parts) != 3 || parts[0] != "scripts" {
			continue
		}
		if !slices.Contains(result[parts[1]], parts[2]) {
			result[parts[1]] = append(result[parts[1]], parts[2])
		}
	}
	// all/all is the dispatcher that reads this list, not a place to send a
	// channel: a line pointing at it would dispatch to itself.
	delete(result, "all")
	for device, providers := range result {
		sort.Strings(providers)
		result[device] = providers
	}
	return result
}

func scriptPackageStatuses(devices map[string][]string) map[string]string {
	return scriptPackageStatusesAt("scripts", devices)
}

func scriptPackageStatusesAt(root string, devices map[string][]string) map[string]string {
	result := map[string]string{}
	for device, providers := range devices {
		for _, provider := range providers {
			selection := filepath.ToSlash(filepath.Join("scripts", device, provider))
			path := filepath.Join(root, device, provider)
			switch {
			case scriptPackageComplete(path):
				result[selection] = "ready"
			case scriptPackageDirectoryExists(path):
				result[selection] = "incomplete"
			default:
				result[selection] = "remote"
			}
		}
	}
	return result
}

func scriptPackageDirectoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// mergeScriptDeviceProviders adds every device/provider pair found under
// root into into, without duplicating one already added from another root.
func mergeScriptDeviceProviders(into map[string][]string, root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range entries {
		if !d.IsDir() || !validScriptPathPart(d.Name()) {
			continue
		}
		subEntries, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, s := range subEntries {
			if s.IsDir() && validScriptPathPart(s.Name()) && holdsTuneScript(filepath.Join(root, d.Name(), s.Name())) && !slices.Contains(into[d.Name()], s.Name()) {
				into[d.Name()] = append(into[d.Name()], s.Name())
			}
		}
	}
}

// holdsTuneScript is true for a folder with at least one of the three entry
// points. Any other folder under scripts, such as one a user keeps captions
// or notes in, is not a streaming app. A folder missing only some of them is
// still listed, so the page can say it is incomplete.
func holdsTuneScript(dir string) bool {
	for _, name := range requiredStreamerFiles {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// allM3USources lists m3u/*.m3u files that are not themselves a previous
// output of this page, so a combined file never gets folded into itself.
func allM3USources() []string {
	entries, err := os.ReadDir("m3u")
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range entries {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".m3u") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("m3u", f.Name()))
		if err == nil && strings.Contains(string(data[:min(len(data), 4096)]), allM3UMarker) {
			continue
		}
		files = append(files, f.Name())
	}
	sort.Strings(files)
	return files
}

// registerAllM3URoutes wires up the "Create all.m3u" page. It is called from
// serveLive, right before the listener starts, which keeps every new route
// out of main.go: that file's diff against upstream stays purely additive
// and carried by the ui-refactor PR.
func registerAllM3URoutes(r *gin.Engine) {
	r.GET("/allm3u", func(c *gin.Context) {
		r.LoadHTMLGlob("html/*")
		devices := allM3UDeviceProviders()
		packageStatuses := scriptPackageStatuses(devices)
		for _, selection := range localTopLevelScriptFolders() {
			packageStatuses[selection] = "incomplete"
			if scriptPackageComplete(selection) {
				packageStatuses[selection] = "ready"
			}
		}
		deviceNames := make([]string, 0, len(devices))
		for d := range devices {
			deviceNames = append(deviceNames, d)
		}
		sort.Strings(deviceNames)
		devicesJSON, err := json.Marshal(allM3UScriptSources())
		if err != nil {
			c.String(http.StatusInternalServerError, "Failed to list scripts: %v", err)
			return
		}
		packageStatusesJSON, err := json.Marshal(packageStatuses)
		if err != nil {
			c.String(http.StatusInternalServerError, "Failed to read local script status: %v", err)
			return
		}
		page := gin.H{
			"sources":     allM3USources(),
			"devices":     deviceNames,
			"devicesJSON": template.JS(devicesJSON),
		}
		page["packageStatusesJSON"] = template.JS(packageStatusesJSON)
		page["streamerApp"] = os.Getenv("STREAMER_APP")
		c.HTML(http.StatusOK, "allm3u.html", page)
	})

	r.POST("/allm3u/install-script", func(c *gin.Context) {
		var req scriptInstallRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validStreamerSelection(req.Path) || strings.HasPrefix(req.Path+"/", "scripts/all/") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "script package must use scripts/device or scripts/device/app"})
			return
		}
		if req.Source == "bundled" {
			scriptUploadMu.Lock()
			err := installBundledScriptPackage(req.Path)
			scriptUploadMu.Unlock()
			if err != nil {
				logger("[SCRIPTS] could not copy included package %s: %v", req.Path, err)
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			logger("[SCRIPTS] copied included package %s", req.Path)
			c.JSON(http.StatusOK, gin.H{"status": "ok", "path": req.Path})
			return
		}
		if !configOperationsAllowed() {
			c.JSON(http.StatusConflict, gin.H{"error": "script downloads wait until no tune is starting or active"})
			return
		}
		select {
		case scriptInstallSlots <- struct{}{}:
			defer func() { <-scriptInstallSlots }()
		case <-c.Request.Context().Done():
			return
		}
		if err := installScriptPackage(c.Request.Context(), req.Path); err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errScriptInstallTune) {
				status = http.StatusConflict
			}
			logger("[SCRIPTS] could not install selected package %s: %v", req.Path, err)
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		logger("[SCRIPTS] downloaded selected package %s", req.Path)
		c.JSON(http.StatusOK, gin.H{"status": "ok", "path": req.Path})
	})

	r.POST("/allm3u/generate", func(c *gin.Context) {
		var req allM3URequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(req.Sources) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "select at least one source M3U"})
			return
		}
		outFile := filepath.Base(strings.TrimSpace(req.Output))
		if outFile == "" || outFile == "." || !strings.HasSuffix(outFile, ".m3u") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "output filename must end in .m3u"})
			return
		}

		var body strings.Builder
		body.WriteString("#EXTM3U\n")
		fmt.Fprintf(&body, "%s (%d source m3u(s)) - edits here are lost on regenerate\n\n", allM3UMarker, len(req.Sources))

		total := 0
		for _, src := range req.Sources {
			file := filepath.Base(src.File)
			if !strings.HasSuffix(file, ".m3u") {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("not an m3u file: %s", src.File)})
				return
			}
			// An empty provider is a script folder directly under scripts.
			if !validScriptPathPart(src.Device) || (src.Provider != "" && !validScriptPathPart(src.Provider)) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid device or provider name"})
				return
			}
			selection := filepath.ToSlash(filepath.Join("scripts", src.Device, src.Provider))
			packagePath := filepath.Join("scripts", src.Device, src.Provider)
			if !scriptPackageComplete(packagePath) {
				if scriptPackageDirectoryExists(packagePath) {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s is a local folder but is missing bmitune.sh, prebmitune.sh, or stopbmitune.sh", selection)})
				} else {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s is not stored locally yet; use Download selected scripts first", selection)})
				}
				return
			}
			data, err := os.ReadFile(filepath.Join("m3u", file))
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("could not read %s: %v", file, err)})
				return
			}

			prefix := src.Device + "~" + src.Provider + "~"
			fmt.Fprintf(&body, "# --- %s (device=%s, provider=%s) ---\n", file, src.Device, src.Provider)
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimRight(line, "\r")
				if strings.TrimSpace(line) == "#EXTM3U" {
					continue
				}
				if strings.Contains(line, "/play/tuner/") {
					line = strings.Replace(line, "/play/tuner/", "/play/tuner/"+prefix, 1)
					total++
				}
				body.WriteString(line)
				body.WriteString("\n")
			}
			body.WriteString("\n")
		}

		if err := os.WriteFile(filepath.Join("m3u", outFile), []byte(body.String()), 0644); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		logger("[ALLM3U] Generated m3u/%s from %d source m3u(s), %d channels", outFile, len(req.Sources), total)
		c.JSON(http.StatusOK, gin.H{"status": "ok", "file": outFile, "channels": total})
	})
}
