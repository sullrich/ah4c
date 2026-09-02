package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func quietTestLogger(t *testing.T) {
	t.Helper()
	old := loggerhandle
	loggerhandle = log.New(io.Discard, "", 0)
	t.Cleanup(func() { loggerhandle = old })
}

func TestRemoveRuntimeBuild(t *testing.T) {
	quietTestLogger(t)
	t.Chdir(t.TempDir())
	dir := runtimeDirFor(rtTranscribe, "cpu")
	if dir == "" {
		t.Skip("no transcribe.cpp build on this platform")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backend"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeRuntimeBuild(rtTranscribe, "cpu"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("engine directory still exists: %v", err)
	}
}

func TestRemoveDriverPackages(t *testing.T) {
	quietTestLogger(t)
	t.Chdir(t.TempDir())
	dir := driverDir(cudaRuntime)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libcudart12_test.deb"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeDriverPackages(cudaRuntime); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("driver directory still exists: %v", err)
	}
}
