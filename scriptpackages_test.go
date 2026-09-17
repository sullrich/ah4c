package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallScriptPackageDownloadsOnlySelectedPackage(t *testing.T) {
	files := map[string]string{
		"bmitune.sh":     "#!/bin/sh\necho tune\n",
		"prebmitune.sh":  "#!/bin/sh\necho prepare\n",
		"stopbmitune.sh": "#!/bin/sh\necho stop\n",
		"common.sh":      "helper=true\n",
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/contents/scripts/firetv/hulu" {
			entries := make([]githubContentsEntry, 0, len(files))
			for name := range files {
				entries = append(entries, githubContentsEntry{Name: name, Type: "file", DownloadURL: server.URL + "/raw/" + name})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		name := filepath.Base(r.URL.Path)
		if data, ok := files[name]; ok {
			_, _ = w.Write([]byte(data))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := t.TempDir()
	err := installScriptPackageFrom(context.Background(), "scripts/firetv/hulu", root, server.URL+"/contents", server.Client(), func() bool { return true })
	if err != nil {
		t.Fatalf("installScriptPackageFrom: %v", err)
	}
	for name, want := range files {
		path := filepath.Join(root, "firetv", "hulu", name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	info, err := os.Stat(filepath.Join(root, "firetv", "hulu", "bmitune.sh"))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("bmitune.sh is not executable: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "allente")); !os.IsNotExist(err) {
		t.Fatalf("an unselected package was created: %v", err)
	}
}

func TestInstallOneLevelScriptPackage(t *testing.T) {
	files := map[string]string{
		"bmitune.sh": "#!/bin/sh\n", "prebmitune.sh": "#!/bin/sh\n", "stopbmitune.sh": "#!/bin/sh\n",
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/contents/scripts/david" {
			entries := make([]githubContentsEntry, 0, len(files))
			for name := range files {
				entries = append(entries, githubContentsEntry{Name: name, Type: "file", DownloadURL: server.URL + "/raw/" + name})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		if data, ok := files[filepath.Base(r.URL.Path)]; ok {
			_, _ = w.Write([]byte(data))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := t.TempDir()
	if err := installScriptPackageFrom(context.Background(), "scripts/david", root, server.URL+"/contents", server.Client(), func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !scriptPackageComplete(filepath.Join(root, "david")) {
		t.Fatal("one-level package was not installed")
	}
}

func TestInstallScriptPackagePreservesWorkingPackageOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentsEntry{{Name: "bmitune.sh", Type: "file", DownloadURL: "http://127.0.0.1:1/unreachable"}})
	}))
	defer server.Close()

	root := t.TempDir()
	target := filepath.Join(root, "firetv", "hulu")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredStreamerFiles {
		if err := os.WriteFile(filepath.Join(target, name), []byte("working "+name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installScriptPackageFrom(context.Background(), "scripts/firetv/hulu", root, server.URL, server.Client(), func() bool { return true }); err == nil {
		t.Fatal("expected the incomplete download to fail")
	}
	for _, name := range requiredStreamerFiles {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(got) != "working "+name {
			t.Fatalf("working %s was not preserved: got=%q err=%v", name, got, err)
		}
	}
}

func TestInstallScriptPackageStopsBeforeGitHubWhenTuneIsActive(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	err := installScriptPackageFrom(context.Background(), "scripts/firetv/hulu", t.TempDir(), server.URL, server.Client(), func() bool { return false })
	if err != errScriptInstallTune {
		t.Fatalf("error = %v, want %v", err, errScriptInstallTune)
	}
	if called {
		t.Fatal("GitHub was contacted while a tune was active")
	}
}

func TestRepairScriptPackageTransactions(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "firetv")
	backup := filepath.Join(parent, ".hulu.backup")
	stage := filepath.Join(parent, ".netflix.update.abcd")
	for _, directory := range []string{backup, stage} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range requiredStreamerFiles {
		if err := os.WriteFile(filepath.Join(backup, name), []byte(name), 0755); err != nil {
			t.Fatal(err)
		}
	}

	repairScriptPackageTransactions(root)
	if !scriptPackageComplete(filepath.Join(parent, "hulu")) {
		t.Fatal("complete backup was not restored")
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup still exists: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stale update stage still exists: %v", err)
	}
}

func TestRepairOneLevelScriptPackageTransaction(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(root, ".david.backup")
	if err := os.MkdirAll(backup, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredStreamerFiles {
		if err := os.WriteFile(filepath.Join(backup, name), []byte(name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	repairScriptPackageTransactions(root)
	if !scriptPackageComplete(filepath.Join(root, "david")) {
		t.Fatal("one-level backup was not restored")
	}
}
