package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func configAPITestSetup(t *testing.T, settings Settings, locked map[string]bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldPath := settingsPathOverride
	settingsPathOverride = t.TempDir() + "/settings.json"
	t.Cleanup(func() { settingsPathOverride = oldPath })
	envEngineMu.Lock()
	oldSettings, oldLocked, oldDocker := envSettings, envLocked, envDockerManaged
	envSettings, envLocked, envDockerManaged = normalizeSettings(settings), locked, false
	envEngineMu.Unlock()
	oldRestart := configRestartNeeded
	oldRestartKeys := configRestartKeys
	configRestartNeeded = false
	configRestartKeys = map[string]bool{}
	t.Cleanup(func() {
		envEngineMu.Lock()
		envSettings, envLocked, envDockerManaged = oldSettings, oldLocked, oldDocker
		envEngineMu.Unlock()
		configRestartNeeded = oldRestart
		configRestartKeys = oldRestartKeys
	})
	r := gin.New()
	registerConfigRoutes(r)
	return r
}

func TestConfigAPIRejectsLockedKey(t *testing.T) {
	r := configAPITestSetup(t, emptySettings(), map[string]bool{"IPADDRESS": true})
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{"IPADDRESS":"new:7654"},"extra":{}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "IPADDRESS") {
		t.Fatalf("locked save = %d %s", w.Code, w.Body.String())
	}
}

func TestConfigAPIRejectsInvalidStreamerPath(t *testing.T) {
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{"STREAMER_APP":"../../tmp/script"},"extra":{}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "scripts/device/app") {
		t.Fatalf("invalid streamer path = %d %s", w.Code, w.Body.String())
	}
}

func TestFirstRunCanFinishWithoutStreamerOrTuners(t *testing.T) {
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})

	initial := httptest.NewRecorder()
	r.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"wizard":true`) {
		t.Fatalf("initial config = %d %s", initial.Code, initial.Body.String())
	}

	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{},"tuners":[],"extra":{}}`))
	req.Header.Set("Content-Type", "application/json")
	saved := httptest.NewRecorder()
	r.ServeHTTP(saved, req)
	if saved.Code != http.StatusOK || strings.Contains(saved.Body.String(), `"wizard":true`) {
		t.Fatalf("empty setup save = %d %s", saved.Code, saved.Body.String())
	}
	settings, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Vars["STREAMER_APP"] != "" || len(settings.Tuners) != 0 {
		t.Fatalf("unexpected empty setup: %#v", settings)
	}
}

func TestMissingStreamerOnlyBlocksTuneRequests(t *testing.T) {
	t.Setenv("STREAMER_APP", "")
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})

	settingsAPI := httptest.NewRecorder()
	r.ServeHTTP(settingsAPI, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if settingsAPI.Code != http.StatusOK {
		t.Fatalf("settings API was blocked: %d %s", settingsAPI.Code, settingsAPI.Body.String())
	}

	m3u := httptest.NewRecorder()
	r.ServeHTTP(m3u, httptest.NewRequest(http.MethodGet, "/m3u/example.m3u", nil))
	if m3u.Code == http.StatusServiceUnavailable {
		t.Fatalf("M3U access was blocked: %d %s", m3u.Code, m3u.Body.String())
	}

	tune := httptest.NewRecorder()
	r.ServeHTTP(tune, httptest.NewRequest(http.MethodGet, "/play/tuner1/123", nil))
	if tune.Code != http.StatusServiceUnavailable || !strings.Contains(tune.Body.String(), "choose one in Settings") {
		t.Fatalf("tune without streamer = %d %s", tune.Code, tune.Body.String())
	}
}

func TestConfigAPIMaskedSecretIsUnchanged(t *testing.T) {
	s := emptySettings()
	s.Vars["ALERT_EMAIL_PASS"] = "original-secret"
	r := configAPITestSetup(t, s, map[string]bool{})
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{"ALERT_EMAIL_PASS":"•••"},"extra":{}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("secret save = %d %s", w.Code, w.Body.String())
	}
	saved, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Vars["ALERT_EMAIL_PASS"] != "original-secret" {
		t.Fatalf("secret changed to %q", saved.Vars["ALERT_EMAIL_PASS"])
	}
	if strings.Contains(w.Body.String(), "original-secret") {
		t.Fatal("secret leaked in API response")
	}
}

func TestConfigAPIRestartNeededLifecycle(t *testing.T) {
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"vars":{"PLAYBACK_DETECTION":"true"},"extra":{}}`, false},
		{`{"vars":{"PLAYBACK_DETECTION":"true","STREAMER_APP":"scripts/firetv/hulu"},"extra":{}}`, true},
	} {
		req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("save = %d %s", w.Code, w.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if got, _ := payload["restartNeeded"].(bool); got != tc.want {
			t.Fatalf("restartNeeded = %v, want %v", got, tc.want)
		}
	}
}

func TestConfigAPITunersMaterializeOnRestart(t *testing.T) {
	tuners := []TunerSpec{{TunerIP: "box", EncoderURL: "http://encoder/stream"}}
	s := Settings{Version: 1, Vars: map[string]string{}, Tuners: tuners, Extra: map[string]string{}}
	sets, _ := materializePlan(nil, s)
	if sets["NUMBER_TUNERS"] != "1" || sets["TUNER1_IP"] != "box" || sets["ENCODER1_URL"] != "http://encoder/stream" {
		t.Fatalf("unexpected tuner materialization: %#v", sets)
	}
}

func TestReadLocalScriptDirectory(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "my-device", "my-app")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bmitune.sh", "prebmitune.sh", "stopbmitune.sh", "helper.sh"} {
		if err := os.WriteFile(filepath.Join(packageDir, name), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	rootListing, err := readLocalScriptDirectory(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rootListing.Entries) != 1 || !rootListing.Entries[0].Directory || rootListing.Entries[0].Path != "my-device" {
		t.Fatalf("root listing = %#v", rootListing)
	}
	packageListing, err := readLocalScriptDirectory(root, "my-device/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if !packageListing.Selectable || packageListing.Selection != "scripts/my-device/my-app" || packageListing.Parent != "my-device" {
		t.Fatalf("package listing = %#v", packageListing)
	}
	if len(packageListing.Entries) != 4 || packageListing.Entries[0].Name != "bmitune.sh" {
		t.Fatalf("package entries = %#v", packageListing.Entries)
	}
}

func TestReadLocalScriptDirectoryReportsMissingEntryPoints(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "my-device", "my-app")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "bmitune.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	listing, err := readLocalScriptDirectory(root, "my-device/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if listing.Selectable || strings.Join(listing.Missing, ",") != "prebmitune.sh,stopbmitune.sh" {
		t.Fatalf("incomplete package listing = %#v", listing)
	}
}

func TestReadLocalScriptDirectoryRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../", "outside"} {
		if _, err := readLocalScriptDirectory(root, path); err == nil {
			t.Fatalf("readLocalScriptDirectory(%q) accepted an escape", path)
		}
	}
}

func TestDiscoverLocalStreamersOnlyListsCompletePackages(t *testing.T) {
	root := t.TempDir()
	complete := filepath.Join(root, "my-device", "my-app")
	incomplete := filepath.Join(root, "other-device", "other-app")
	for _, directory := range []string{complete, incomplete} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range requiredStreamerFiles {
		if err := os.WriteFile(filepath.Join(complete, name), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(incomplete, "bmitune.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got := discoverLocalStreamersAt(root)
	if len(got) != 1 || got[0] != "scripts/my-device/my-app" {
		t.Fatalf("discoverLocalStreamersAt() = %#v", got)
	}
}

func TestConnectionAddressParsing(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"living-room", "living-room:5555"},
		{"living-room:6000", "living-room:6000"},
		{"2001:db8::1", "[2001:db8::1]:5555"},
	} {
		got, err := addressWithDefaultPort(tc.value, "5555")
		if err != nil || got != tc.want {
			t.Fatalf("addressWithDefaultPort(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"http://encoder/stream", "encoder:80"},
		{"https://encoder.example:8443/live", "encoder.example:8443"},
	} {
		got, err := encoderNetworkAddress(tc.value)
		if err != nil || got != tc.want {
			t.Fatalf("encoderNetworkAddress(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
}

func TestCheckEncoderConnectionDoesNotOpenHTTPStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	result := checkEncoderConnection(context.Background(), "http://"+listener.Addr().String()+"/stream")
	if result.State != "ready" {
		t.Fatalf("encoder result = %#v", result)
	}
	conn := <-accepted
	defer conn.Close()
	buffer := make([]byte, 1)
	if n, _ := conn.Read(buffer); n != 0 {
		t.Fatalf("connection check wrote %d bytes to the encoder", n)
	}
}
