package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestAppleTVPairingAPICollectsPINAndPreservesSavedPairings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	directory := t.TempDir()
	executable := filepath.Join(directory, "atvremote-test")
	script := `#!/bin/sh
storage=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --storage-filename) storage="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'Enter PIN on screen: '
IFS= read -r pin
[ "$pin" = "1234" ] || exit 3
printf '\npaired=true\n' >> "$storage"
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, ".pyatv.conf")
	if err := os.WriteFile(config, []byte("existing=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldExecutable, oldConfig := appleTVRemoteExecutable, appleTVConfigPath
	appleTVRemoteExecutable, appleTVConfigPath = executable, config
	resetAppleTVPairingForTest(t)
	t.Cleanup(func() {
		appleTVRemoteExecutable, appleTVConfigPath = oldExecutable, oldConfig
	})

	router := gin.New()
	registerAppleTVPairingRoutes(router)
	start := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/api/config/apple-tv-pairing/start", strings.NewReader(`{"target":"living-room"}`))
	startRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(start, startRequest)
	if start.Code != http.StatusOK {
		t.Fatalf("start pairing = %d %s", start.Code, start.Body.String())
	}
	var started struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil || started.SessionID == "" {
		t.Fatalf("start response = %s, %v", start.Body.String(), err)
	}

	finish := httptest.NewRecorder()
	finishRequest := httptest.NewRequest(http.MethodPost, "/api/config/apple-tv-pairing/pin", strings.NewReader(`{"sessionId":"`+started.SessionID+`","pin":"1234"}`))
	finishRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(finish, finishRequest)
	if finish.Code != http.StatusOK || !strings.Contains(finish.Body.String(), `"state":"ready"`) {
		t.Fatalf("finish pairing = %d %s", finish.Code, finish.Body.String())
	}
	saved, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "existing=true") || !strings.Contains(string(saved), "paired=true") {
		t.Fatalf("saved pairing did not preserve and add credentials: %q", saved)
	}
	if info, err := os.Stat(config); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved pairing mode = %v, %v", info, err)
	}
}

func TestAppleTVPairingAPIRejectsIncompletePIN(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	registerAppleTVPairingRoutes(router)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/config/apple-tv-pairing/pin", strings.NewReader(`{"sessionId":"missing","pin":"12"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "four numbers") {
		t.Fatalf("short PIN = %d %s", response.Code, response.Body.String())
	}
}

func resetAppleTVPairingForTest(t *testing.T) {
	t.Helper()
	appleTVPairingMu.Lock()
	previous := appleTVPairingCurrent
	appleTVPairingCurrent = nil
	appleTVPairingMu.Unlock()
	if previous != nil && !previous.isFinished() {
		previous.stop()
		select {
		case <-previous.done:
		case <-time.After(time.Second):
			t.Fatal("previous Apple TV pairing did not stop")
		}
	}
	t.Cleanup(func() {
		appleTVPairingMu.Lock()
		current := appleTVPairingCurrent
		appleTVPairingCurrent = nil
		appleTVPairingMu.Unlock()
		if current != nil && !current.isFinished() {
			current.stop()
			select {
			case <-current.done:
			case <-time.After(time.Second):
			}
		}
	})
}
