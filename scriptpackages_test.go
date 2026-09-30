package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeScriptsGitHub serves one script folder the way the GitHub contents API
// and raw download URLs do.
func fakeScriptsGitHub(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/raw/"); ok {
			body, found := files[name]
			if !found {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(body))
			return
		}
		entries := []githubContentsEntry{}
		for name := range files {
			entries = append(entries, githubContentsEntry{Name: name, Type: "file", DownloadURL: server.URL + "/raw/" + name})
		}
		json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(server.Close)
	return server
}

func writeScriptFolder(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func readScript(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var newScripts = map[string]string{
	"prebmitune.sh":  "pre v2\n",
	"bmitune.sh":     "tune v2\n",
	"stopbmitune.sh": "stop v2\n",
}

func alwaysAllowed() bool { return true }

func TestInstallScriptPackageKeepsExistingFilesByDefault(t *testing.T) {
	server := fakeScriptsGitHub(t, newScripts)
	root := t.TempDir()
	target := filepath.Join(root, "firetv", "app")
	writeScriptFolder(t, target, map[string]string{"bmitune.sh": "tune v1\n"})

	backup, err := installScriptPackageFrom(context.Background(), "scripts/firetv/app", false, root, server.URL, server.Client(), alwaysAllowed)
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Fatalf("backup = %q, want none", backup)
	}
	if got := readScript(t, filepath.Join(target, "bmitune.sh")); got != "tune v1\n" {
		t.Fatalf("existing bmitune.sh was changed to %q", got)
	}
	if got := readScript(t, filepath.Join(target, "prebmitune.sh")); got != "pre v2\n" {
		t.Fatalf("missing prebmitune.sh was not added: %q", got)
	}
}

func TestInstallScriptPackageReplaceBacksUpAndKeepsUserFiles(t *testing.T) {
	server := fakeScriptsGitHub(t, newScripts)
	root := t.TempDir()
	target := filepath.Join(root, "firetv", "app")
	writeScriptFolder(t, target, map[string]string{
		"prebmitune.sh":  "pre v2\n",
		"bmitune.sh":     "tune v1\n",
		"stopbmitune.sh": "stop v1\n",
		"notes.txt":      "mine\n",
	})

	backup, err := installScriptPackageFrom(context.Background(), "scripts/firetv/app", true, root, server.URL, server.Client(), alwaysAllowed)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range newScripts {
		if got := readScript(t, filepath.Join(target, name)); got != body {
			t.Fatalf("%s = %q, want %q", name, got, body)
		}
	}
	if got := readScript(t, filepath.Join(target, "notes.txt")); got != "mine\n" {
		t.Fatalf("user file changed to %q", got)
	}
	if backup == "" || filepath.Dir(backup) != filepath.Dir(target) || !strings.HasPrefix(filepath.Base(backup), ".app.backup-") {
		t.Fatalf("backup = %q, want a hidden .app.backup-* folder beside the target", backup)
	}
	if info, err := os.Stat(backup); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0755 {
		t.Fatalf("backup folder mode = %v, want 0755", info.Mode().Perm())
	}
	if got := readScript(t, filepath.Join(backup, "bmitune.sh")); got != "tune v1\n" {
		t.Fatalf("backup bmitune.sh = %q", got)
	}
	if _, err := os.Stat(filepath.Join(backup, "prebmitune.sh")); !os.IsNotExist(err) {
		t.Fatalf("unchanged prebmitune.sh was backed up (err %v)", err)
	}
	if validScriptPathPart(filepath.Base(backup)) {
		t.Fatalf("backup folder %q would be listed as a script package", filepath.Base(backup))
	}
	leftovers, _ := filepath.Glob(filepath.Join(target, ".*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left in the folder: %v", leftovers)
	}
}

func TestReplaceScriptFilesSkipsBackupWhenNothingDiffers(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "app")
	writeScriptFolder(t, source, newScripts)
	writeScriptFolder(t, target, newScripts)

	backup, err := replaceScriptFiles(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Fatalf("backup = %q, want none when every file already matches", backup)
	}
}

func TestInstallScriptPackageReplaceWaitsForTunes(t *testing.T) {
	server := fakeScriptsGitHub(t, newScripts)
	root := t.TempDir()
	target := filepath.Join(root, "firetv", "app")
	writeScriptFolder(t, target, map[string]string{"bmitune.sh": "tune v1\n"})

	_, err := installScriptPackageFrom(context.Background(), "scripts/firetv/app", true, root, server.URL, server.Client(), func() bool { return false })
	if err != errScriptInstallTune {
		t.Fatalf("err = %v, want errScriptInstallTune", err)
	}
	if got := readScript(t, filepath.Join(target, "bmitune.sh")); got != "tune v1\n" {
		t.Fatalf("bmitune.sh changed during a tune: %q", got)
	}
}

func TestReplaceScriptFilesChangesNothingWhenOneFileCannotBeReplaced(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "app")
	writeScriptFolder(t, source, newScripts)
	writeScriptFolder(t, target, map[string]string{"bmitune.sh": "tune v1\n", "prebmitune.sh": "pre v1\n"})
	// A user who points stopbmitune.sh somewhere else keeps it; the replace
	// must then leave the whole folder as it was, not half of it.
	if err := os.Symlink("/bin/true", filepath.Join(target, "stopbmitune.sh")); err != nil {
		t.Fatal(err)
	}

	if _, err := replaceScriptFiles(source, target); err == nil {
		t.Fatal("replace succeeded over a symbolic link")
	}
	if got := readScript(t, filepath.Join(target, "bmitune.sh")); got != "tune v1\n" {
		t.Fatalf("bmitune.sh was replaced although the folder could not be fully updated: %q", got)
	}
	if got := readScript(t, filepath.Join(target, "prebmitune.sh")); got != "pre v1\n" {
		t.Fatalf("prebmitune.sh was replaced although the folder could not be fully updated: %q", got)
	}
}
