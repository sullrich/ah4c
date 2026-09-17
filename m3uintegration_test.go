package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestServerBaseURL(t *testing.T) {
	tests := map[string]string{
		"channels-dvr":          "http://channels-dvr:8089",
		"channels-dvr:9000":     "http://channels-dvr:9000",
		"https://channels:9443": "https://channels:9443",
		"192.168.1.20":          "http://192.168.1.20:8089",
	}
	for input, want := range tests {
		got, err := serverBaseURL(input, "8089")
		if err != nil {
			t.Fatalf("serverBaseURL(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("serverBaseURL(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "ftp://channels", "http://channels/path", "http://user:pass@channels"} {
		if got, err := serverBaseURL(input, "8089"); err == nil {
			t.Errorf("serverBaseURL(%q) = %q, want an error", input, got)
		}
	}
}

func TestPutChannelsM3USourceUsesChannelsCustomSourceAPI(t *testing.T) {
	var got channelsM3USource
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/providers/m3u/sources/AH4C" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := putChannelsM3USource(context.Background(), server.Client(), server.URL, "AH4C - all", "http://ah4c:7654/m3u/all.m3u")
	if err != nil {
		t.Fatalf("putChannelsM3USource: %v", err)
	}
	if got.Name != "AH4C - all" || got.Type != "MPEG-TS" || got.Source != "URL" || got.URL != "http://ah4c:7654/m3u/all.m3u" || got.Refresh != "24" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestValidM3UFile(t *testing.T) {
	if got, err := validM3UFile("all.m3u"); err != nil || got != "all.m3u" {
		t.Fatalf("validM3UFile(all.m3u) = %q, %v", got, err)
	}
	for _, value := range []string{"", "all.txt", "../all.m3u", "folder/all.m3u"} {
		if _, err := validM3UFile(value); err == nil {
			t.Errorf("validM3UFile(%q) should fail", value)
		}
	}
}

func TestRememberChannelsM3USaveFailureDoesNotMutateLiveSettings(t *testing.T) {
	oldPath := settingsPathOverride
	settingsPathOverride = filepath.Join(t.TempDir(), "settings-directory")
	if err := os.Mkdir(settingsPathOverride, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settingsPathOverride = oldPath })

	envEngineMu.Lock()
	oldSettings, oldLocked := envSettings, envLocked
	envSettings = emptySettings()
	envLocked = map[string]bool{}
	envSettings.Vars["IPADDRESS"] = "ah4c:7654"
	before := copySettings(envSettings)
	envEngineMu.Unlock()
	t.Cleanup(func() {
		envEngineMu.Lock()
		envSettings, envLocked = oldSettings, oldLocked
		envEngineMu.Unlock()
	})

	if saved, _ := rememberChannelsM3U("all.m3u"); saved {
		t.Fatal("save unexpectedly succeeded")
	}
	envEngineMu.RLock()
	after := copySettings(envSettings)
	envEngineMu.RUnlock()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("live settings changed after failed save: before=%#v after=%#v", before, after)
	}
}
