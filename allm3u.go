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
	Path string `json:"path"`
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
	for device, providers := range result {
		sort.Strings(providers)
		result[device] = providers
	}
	return result
}

func installedScriptPackages(devices map[string][]string) map[string]bool {
	result := map[string]bool{}
	for device, providers := range devices {
		for _, provider := range providers {
			selection := filepath.ToSlash(filepath.Join("scripts", device, provider))
			result[selection] = scriptPackageComplete(filepath.Join("scripts", device, provider))
		}
	}
	return result
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
			if s.IsDir() && validScriptPathPart(s.Name()) && !slices.Contains(into[d.Name()], s.Name()) {
				into[d.Name()] = append(into[d.Name()], s.Name())
			}
		}
	}
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
		installed := installedScriptPackages(devices)
		deviceNames := make([]string, 0, len(devices))
		for d := range devices {
			deviceNames = append(deviceNames, d)
		}
		sort.Strings(deviceNames)
		devicesJSON, err := json.Marshal(devices)
		if err != nil {
			c.String(http.StatusInternalServerError, "Failed to list scripts: %v", err)
			return
		}
		installedJSON, err := json.Marshal(installed)
		if err != nil {
			c.String(http.StatusInternalServerError, "Failed to list installed scripts: %v", err)
			return
		}
		page := gin.H{
			"sources":     allM3USources(),
			"devices":     deviceNames,
			"devicesJSON": template.JS(devicesJSON),
		}
		page["installedJSON"] = template.JS(installedJSON)
		page["streamerApp"] = os.Getenv("STREAMER_APP")
		c.HTML(http.StatusOK, "allm3u.html", page)
	})

	r.POST("/allm3u/install-script", func(c *gin.Context) {
		var req scriptInstallRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validStreamerSelection(req.Path) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "script package must use scripts/device/app"})
			return
		}
		if !connectionChecksAllowed() {
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
			if !validScriptPathPart(src.Device) || !validScriptPathPart(src.Provider) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid device or provider name"})
				return
			}
			selection := filepath.ToSlash(filepath.Join("scripts", src.Device, src.Provider))
			if !scriptPackageComplete(filepath.Join("scripts", src.Device, src.Provider)) {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s is not downloaded yet; use Download selected scripts first", selection)})
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
