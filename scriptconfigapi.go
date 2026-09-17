package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	localScriptFileLimit  = 8 << 20
	localScriptTotalLimit = 32 << 20
)

var (
	localScriptsRootOverride string
	scriptConfigMu           sync.Mutex
)

func registerScriptConfigRoutes(r *gin.Engine) {
	r.GET("/api/config/streamers", streamersConfigHandler)
	r.GET("/api/config/local-scripts", localScriptsConfigHandler)
	r.POST("/api/config/local-scripts", uploadLocalScriptsConfigHandler)
}

func streamersConfigHandler(c *gin.Context) {
	local := discoverLocalStreamers()
	remote := loadStreamerCache()
	if c.Query("remote") == "true" {
		queried, err := queryUpstreamStreamers()
		if err != nil {
			if c.Query("details") == "true" {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "local": local, "remote": remote})
			} else {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "streamers": mergeStreamers(local, remote)})
			}
			return
		}
		remote = queried
	}
	if c.Query("details") == "true" {
		c.JSON(http.StatusOK, gin.H{"local": local, "remote": remote})
		return
	}
	c.JSON(http.StatusOK, mergeStreamers(local, remote))
}

type localScriptEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type localScriptDirectory struct {
	Path       string             `json:"path"`
	Parent     string             `json:"parent"`
	Selectable bool               `json:"selectable"`
	Selection  string             `json:"selection,omitempty"`
	Missing    []string           `json:"missing,omitempty"`
	Entries    []localScriptEntry `json:"entries"`
}

func localScriptsConfigHandler(c *gin.Context) {
	root, err := localScriptsRoot()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not locate the local scripts folder"})
		return
	}
	directory, err := readLocalScriptDirectory(root, c.Query("path"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, directory)
}

func localScriptsRoot() (string, error) {
	if localScriptsRootOverride != "" {
		return filepath.Abs(localScriptsRootOverride)
	}
	return filepath.Abs("scripts")
}

func uploadLocalScriptsConfigHandler(c *gin.Context) {
	scriptConfigMu.Lock()
	defer scriptConfigMu.Unlock()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, localScriptTotalLimit+(1<<20))
	if err := c.Request.ParseMultipartForm(localScriptTotalLimit); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "The script upload is too large or could not be read."})
		return
	}
	defer c.Request.MultipartForm.RemoveAll()
	root, err := localScriptsRoot()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not locate the local scripts folder"})
		return
	}
	selection, err := createLocalScriptPackage(root, c.PostForm("device"), c.PostForm("app"), c.Request.MultipartForm.File["files"])
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrExist) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"selection": selection, "message": "Scripts uploaded."})
}

func createLocalScriptPackage(root, device, app string, files []*multipart.FileHeader) (string, error) {
	device = strings.TrimSpace(device)
	app = strings.TrimSpace(app)
	if !validScriptPathPart(device) || (app != "" && !validScriptPathPart(app)) {
		return "", fmt.Errorf("package, device and app names may use letters, numbers, dots, underscores and hyphens")
	}
	if len(files) == 0 {
		return "", fmt.Errorf("choose the script files to upload")
	}
	seen := make(map[string]bool, len(files))
	total := int64(0)
	for _, file := range files {
		name := file.Filename
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return "", fmt.Errorf("script filename %q is not safe", name)
		}
		if seen[name] {
			return "", fmt.Errorf("script filename %q was selected more than once", name)
		}
		seen[name] = true
		if file.Size < 0 || file.Size > localScriptFileLimit {
			return "", fmt.Errorf("%s is larger than the 8 MB per-file limit", name)
		}
		total += file.Size
		if total > localScriptTotalLimit {
			return "", fmt.Errorf("the selected scripts are larger than the 32 MB upload limit")
		}
	}
	var missing []string
	for _, required := range requiredStreamerFiles {
		if !seen[required] {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("the package is missing required files: %s", strings.Join(missing, ", "))
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", fmt.Errorf("could not create the local scripts folder: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("could not open the local scripts folder: %w", err)
	}
	parent := rootReal
	packageName := device
	selection := "scripts/" + device
	if app != "" {
		deviceDir := filepath.Join(rootReal, device)
		if err := os.MkdirAll(deviceDir, 0755); err != nil {
			return "", fmt.Errorf("could not create the device folder: %w", err)
		}
		deviceReal, err := filepath.EvalSymlinks(deviceDir)
		if err != nil || filepath.Dir(deviceReal) != rootReal {
			return "", fmt.Errorf("the device folder is outside the local scripts folder")
		}
		parent = deviceReal
		packageName = app
		selection += "/" + app
	}
	target := filepath.Join(parent, packageName)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%s already exists: %w", selection, os.ErrExist)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("could not check the destination folder: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, "."+packageName+"-upload-")
	if err != nil {
		return "", fmt.Errorf("could not prepare the script upload: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	for _, header := range files {
		source, err := header.Open()
		if err != nil {
			return "", fmt.Errorf("could not read %s: %w", header.Filename, err)
		}
		destination, err := os.OpenFile(filepath.Join(temporary, header.Filename), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			source.Close()
			return "", fmt.Errorf("could not create %s: %w", header.Filename, err)
		}
		written, copyErr := io.Copy(destination, io.LimitReader(source, localScriptFileLimit+1))
		closeErr := destination.Close()
		source.Close()
		if copyErr != nil || closeErr != nil {
			return "", fmt.Errorf("could not save %s", header.Filename)
		}
		if written > localScriptFileLimit {
			return "", fmt.Errorf("%s is larger than the 8 MB per-file limit", header.Filename)
		}
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", fmt.Errorf("could not finish the script package: %w", err)
	}
	committed = true
	return selection, nil
}

func readLocalScriptDirectory(root, relative string) (localScriptDirectory, error) {
	result := localScriptDirectory{Entries: []localScriptEntry{}}
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if clean == "." {
		clean = ""
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return result, fmt.Errorf("that folder is outside the local scripts folder")
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, clean)
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return result, fmt.Errorf("the local scripts folder is not available")
	}
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("that local scripts folder does not exist")
		}
		return result, fmt.Errorf("could not read the local scripts folder: %w", err)
	}
	rootPrefix := rootReal + string(filepath.Separator)
	if targetReal != rootReal && !strings.HasPrefix(targetReal, rootPrefix) {
		return result, fmt.Errorf("that folder is outside the local scripts folder")
	}
	entries, err := os.ReadDir(targetReal)
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("that local scripts folder does not exist")
		}
		return result, fmt.Errorf("could not read the local scripts folder: %w", err)
	}
	result.Path = filepath.ToSlash(clean)
	if clean != "" {
		parent := filepath.Dir(clean)
		if parent == "." {
			parent = ""
		}
		result.Parent = filepath.ToSlash(parent)
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if clean != "" && (len(parts) == 1 || len(parts) == 2) && validStreamerSelection("scripts/"+filepath.ToSlash(clean)) {
		result.Missing = missingRequiredStreamerFiles(targetReal)
		if len(result.Missing) == 0 {
			result.Selectable = true
			result.Selection = "scripts/" + filepath.ToSlash(clean)
		}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			continue
		}
		entryPath := filepath.Join(clean, entry.Name())
		result.Entries = append(result.Entries, localScriptEntry{
			Name: entry.Name(), Path: filepath.ToSlash(entryPath), Directory: entry.IsDir(),
		})
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].Directory != result.Entries[j].Directory {
			return result.Entries[i].Directory
		}
		return strings.ToLower(result.Entries[i].Name) < strings.ToLower(result.Entries[j].Name)
	})
	return result, nil
}

func missingRequiredStreamerFiles(directory string) []string {
	var missing []string
	for _, name := range requiredStreamerFiles {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() {
			missing = append(missing, name)
		}
	}
	return missing
}

func discoverStreamers() []string {
	return mergeStreamers(discoverLocalStreamers(), loadStreamerCache())
}

type githubTreeResponse struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	} `json:"tree"`
}

func queryUpstreamStreamers() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/sullrich/ah4c/git/trees/main?recursive=1", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ah4c-settings")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not query sullrich/ah4c: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sullrich/ah4c returned %s", resp.Status)
	}
	var tree githubTreeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return nil, fmt.Errorf("could not check GitHub for available scripts: %w", err)
	}
	packages := map[string]map[string]bool{}
	for _, entry := range tree.Tree {
		parts := strings.Split(filepath.ToSlash(entry.Path), "/")
		if entry.Type != "blob" || (len(parts) != 3 && len(parts) != 4) || parts[0] != "scripts" {
			continue
		}
		selectionParts := parts[:len(parts)-1]
		selection := strings.Join(selectionParts, "/")
		if !validStreamerSelection(selection) {
			continue
		}
		name := parts[len(parts)-1]
		for _, required := range requiredStreamerFiles {
			if name == required {
				if packages[selection] == nil {
					packages[selection] = map[string]bool{}
				}
				packages[selection][name] = true
			}
		}
	}
	result := make([]string, 0, len(packages))
	for path, files := range packages {
		complete := true
		for _, required := range requiredStreamerFiles {
			complete = complete && files[required]
		}
		if complete {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	if err := saveStreamerCache(result); err != nil {
		logger("[CONFIG] could not save the available script list: %v", err)
	}
	return result, nil
}

func streamerCachePath() string {
	return filepath.Join(filepath.Dir(settingsFilePath()), "streamers.json")
}

func loadStreamerCache() []string {
	b, err := os.ReadFile(streamerCachePath())
	if err != nil {
		return nil
	}
	var result []string
	if json.Unmarshal(b, &result) != nil {
		return nil
	}
	return result
}

func saveStreamerCache(streamers []string) error {
	path := streamerCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(streamers, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
