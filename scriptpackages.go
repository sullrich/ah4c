package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	githubContentsBaseURL = "https://api.github.com/repos/sullrich/ah4c/contents"
	maxPackageFiles       = 128
	maxPackageFileBytes   = 2 << 20
	maxPackageBytes       = 8 << 20
)

var (
	errScriptInstallTune = errors.New("a tune started while scripts were being downloaded")
	scriptInstallSlots   = make(chan struct{}, 1)
)

type githubContentsEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
}

// installScriptPackage downloads one explicitly selected scripts/device or
// scripts/device/app folder. It stages every file beside the destination and swaps the complete
// directory into place, so a network failure cannot damage a working package.
func installScriptPackage(ctx context.Context, selection string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return installScriptPackageFrom(ctx, selection, "scripts", githubContentsBaseURL, http.DefaultClient, configOperationsAllowed)
}

func installScriptPackageFrom(ctx context.Context, selection, scriptsRoot, contentsBaseURL string, client *http.Client, allowed func() bool) error {
	selection = canonicalStreamerSelection(selection)
	if !validStreamerSelection(selection) {
		return fmt.Errorf("script folder must use scripts/device or scripts/device/app")
	}
	parts := strings.Split(selection, "/")
	if !allowed() {
		return errScriptInstallTune
	}
	operationCtx, cancelOperation := context.WithCancel(ctx)
	stopWatcher := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !allowed() {
					cancelOperation()
					return
				}
			case <-stopWatcher:
				return
			case <-operationCtx.Done():
				return
			}
		}
	}()
	defer close(stopWatcher)
	defer cancelOperation()
	ctx = operationCtx

	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	contentsURL := strings.TrimRight(contentsBaseURL, "/") + "/" + strings.Join(escaped, "/") + "?ref=main"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ah4c-script-installer")
	resp, err := client.Do(req)
	if err != nil {
		if !allowed() {
			return errScriptInstallTune
		}
		return fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned %s for %s", resp.Status, selection)
	}
	contents, err := readScriptBody(resp.Body, 1<<20, allowed)
	if err != nil {
		return err
	}
	var entries []githubContentsEntry
	if err := json.Unmarshal(contents, &entries); err != nil {
		return fmt.Errorf("could not read the script package list: %w", err)
	}
	if len(entries) == 0 || len(entries) > maxPackageFiles {
		return fmt.Errorf("the selected script package has an unexpected number of files")
	}

	root, err := filepath.Abs(scriptsRoot)
	if err != nil {
		return fmt.Errorf("could not locate the local scripts folder: %w", err)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("could not prepare the local scripts folder: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("could not verify the local scripts folder: %w", err)
	}
	parent := root
	packageName := parts[len(parts)-1]
	if len(parts) == 3 {
		parent = filepath.Join(root, parts[1])
		if info, statErr := os.Lstat(parent); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the selected device folder cannot be a symbolic link")
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("could not inspect the selected device folder: %w", statErr)
		}
		if err := os.MkdirAll(parent, 0755); err != nil {
			return fmt.Errorf("could not prepare the local scripts folder: %w", err)
		}
		parent, err = filepath.EvalSymlinks(parent)
		if err != nil || filepath.Dir(parent) != root {
			return fmt.Errorf("could not verify the selected device folder")
		}
	}
	target := filepath.Join(parent, packageName)
	stage, err := os.MkdirTemp(parent, "."+packageName+".update.")
	if err != nil {
		return fmt.Errorf("could not prepare the script update: %w", err)
	}
	defer os.RemoveAll(stage)

	total := int64(0)
	files := 0
	for _, entry := range entries {
		if entry.Type != "file" || entry.DownloadURL == "" || !validScriptFileName(entry.Name) {
			continue
		}
		if !allowed() {
			return errScriptInstallTune
		}
		data, err := downloadScriptFile(ctx, client, entry.DownloadURL, allowed)
		if err != nil {
			return fmt.Errorf("could not download %s: %w", entry.Name, err)
		}
		total += int64(len(data))
		if total > maxPackageBytes {
			return fmt.Errorf("the selected script package is larger than the safe download limit")
		}
		mode := os.FileMode(0644)
		if strings.HasSuffix(entry.Name, ".sh") {
			mode = 0755
		}
		if err := os.WriteFile(filepath.Join(stage, entry.Name), data, mode); err != nil {
			return fmt.Errorf("could not stage %s: %w", entry.Name, err)
		}
		files++
	}
	if files == 0 || !scriptPackageComplete(stage) {
		return fmt.Errorf("%s does not contain bmitune.sh, prebmitune.sh, and stopbmitune.sh", selection)
	}
	if !allowed() {
		return errScriptInstallTune
	}

	backup := filepath.Join(parent, "."+packageName+".backup")
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("could not remove an old script backup: %w", err)
	}
	hadTarget := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("could not preserve the current script package: %w", err)
		}
		hadTarget = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not inspect the current script package: %w", err)
	}
	if err := os.Rename(stage, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("could not activate the downloaded script package: %w", err)
	}
	if hadTarget {
		if err := os.RemoveAll(backup); err != nil {
			logger("[SCRIPTS] could not remove the old %s backup: %v", selection, err)
		}
	}
	return nil
}

// repairScriptPackageTransactions finishes or removes transaction directories
// left behind if the process stopped during a package swap. It runs at startup,
// before the listener is available and therefore before any tune can arrive.
func repairScriptPackageTransactions(scriptsRoot string) {
	devices, err := os.ReadDir(scriptsRoot)
	if err != nil {
		return
	}
	repairScriptPackageTransactionsIn(scriptsRoot, "scripts")
	for _, device := range devices {
		if !device.IsDir() || !validScriptPathPart(device.Name()) {
			continue
		}
		parent := filepath.Join(scriptsRoot, device.Name())
		repairScriptPackageTransactionsIn(parent, "scripts/"+device.Name())
	}
}

func repairScriptPackageTransactionsIn(parent, displayParent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		name := strings.TrimPrefix(entry.Name(), ".")
		if packageName, found := strings.CutSuffix(name, ".backup"); found && validScriptPathPart(packageName) {
			backup := filepath.Join(parent, entry.Name())
			target := filepath.Join(parent, packageName)
			if _, targetErr := os.Lstat(target); os.IsNotExist(targetErr) && scriptPackageComplete(backup) {
				if err := os.Rename(backup, target); err != nil {
					logger("[SCRIPTS] could not restore %s/%s: %v", displayParent, packageName, err)
				} else {
					logger("[SCRIPTS] restored %s/%s after an interrupted update", displayParent, packageName)
				}
			} else if targetErr == nil && scriptPackageComplete(target) {
				if err := os.RemoveAll(backup); err != nil {
					logger("[SCRIPTS] could not remove stale backup %s: %v", backup, err)
				}
			}
			continue
		}
		packageName, suffix, found := strings.Cut(name, ".update.")
		if found && suffix != "" && validScriptPathPart(packageName) {
			stage := filepath.Join(parent, entry.Name())
			if err := os.RemoveAll(stage); err != nil {
				logger("[SCRIPTS] could not remove stale update directory %s: %v", stage, err)
			}
		}
	}
}

func downloadScriptFile(ctx context.Context, client *http.Client, rawURL string, allowed func() bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ah4c-script-installer")
	resp, err := client.Do(req)
	if err != nil {
		if !allowed() {
			return nil, errScriptInstallTune
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}

	return readScriptBody(resp.Body, maxPackageFileBytes, allowed)
}

func readScriptBody(reader io.Reader, limit int, allowed func() bool) ([]byte, error) {
	data := make([]byte, 0, min(limit, 16*1024))
	buffer := make([]byte, 16*1024)
	for {
		if !allowed() {
			return nil, errScriptInstallTune
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if len(data)+n > limit {
				return nil, fmt.Errorf("file is larger than the safe download limit")
			}
			data = append(data, buffer[:n]...)
		}
		if readErr == io.EOF {
			return data, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func validScriptFileName(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
