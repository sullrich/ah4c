package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestFirstRunRequiresAddressesButNotStreamerOrTuners(t *testing.T) {
	t.Setenv("STREAMER_APP", "")
	t.Setenv("NUMBER_TUNERS", "")
	t.Setenv("IPADDRESS", "")
	t.Setenv("CHANNELSIP", "")
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})

	initial := httptest.NewRecorder()
	r.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"wizard":true`) {
		t.Fatalf("initial config = %d %s", initial.Code, initial.Body.String())
	}

	incomplete := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{},"tuners":[],"extra":{}}`))
	incomplete.Header.Set("Content-Type", "application/json")
	incompleteSaved := httptest.NewRecorder()
	r.ServeHTTP(incompleteSaved, incomplete)
	if incompleteSaved.Code != http.StatusOK || !strings.Contains(incompleteSaved.Body.String(), `"wizard":true`) {
		t.Fatalf("incomplete setup save = %d %s", incompleteSaved.Code, incompleteSaved.Body.String())
	}

	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{"IPADDRESS":"ah4c:7654","CHANNELSIP":"channels:8089"},"tuners":[],"extra":{}}`))
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
	if settings.Vars["IPADDRESS"] != "ah4c:7654" || settings.Vars["CHANNELSIP"] != "channels:8089" || settings.Vars["STREAMER_APP"] != "" || len(settings.Tuners) != 0 {
		t.Fatalf("unexpected empty setup: %#v", settings)
	}
}

func TestExistingEnvironmentConfigurationSkipsWizard(t *testing.T) {
	t.Setenv("IPADDRESS", "ah4c:7654")
	t.Setenv("CHANNELSIP", "channels:8089")
	t.Setenv("STREAMER_APP", "./scripts/firetv/hulu/")
	t.Setenv("NUMBER_TUNERS", "1")
	t.Setenv("ENCODER1_URL", "http://encoder/stream")
	t.Setenv("CMD1", "")
	r := configAPITestSetup(t, emptySettings(), map[string]bool{"STREAMER_APP": true, "NUMBER_TUNERS": true, "ENCODER1_URL": true})

	config := httptest.NewRecorder()
	r.ServeHTTP(config, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if config.Code != http.StatusOK || strings.Contains(config.Body.String(), `"wizard":true`) {
		t.Fatalf("environment-only config was sent to wizard: %d %s", config.Code, config.Body.String())
	}
	home := httptest.NewRecorder()
	r.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "/", nil))
	if home.Code == http.StatusFound {
		t.Fatalf("environment-only home redirected to %s", home.Header().Get("Location"))
	}
}

func TestEnvironmentSetupRequiresValidProxyAndChannelsAddresses(t *testing.T) {
	values := map[string]string{}
	lookup := func(key string) string { return values[key] }
	if environmentSetupReady(lookup) {
		t.Fatal("empty setup was ready")
	}
	values["IPADDRESS"] = "ah4c:7654"
	if environmentSetupReady(lookup) {
		t.Fatal("setup without Channels DVR was ready")
	}
	values["CHANNELSIP"] = "channels:8089"
	if !environmentSetupReady(lookup) {
		t.Fatal("setup with both addresses was not ready")
	}
	values["CHANNELSIP"] = "http://channels/path"
	if environmentSetupReady(lookup) {
		t.Fatal("setup with an invalid Channels DVR address was ready")
	}
}

func TestConfigAPICanonicalizesStreamerSelection(t *testing.T) {
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"vars":{"STREAMER_APP":" ./scripts/firetv/hulu/ "},"extra":{}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("canonical streamer save = %d %s", w.Code, w.Body.String())
	}
	settings, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Vars["STREAMER_APP"] != "scripts/firetv/hulu" {
		t.Fatalf("saved STREAMER_APP = %q", settings.Vars["STREAMER_APP"])
	}
}

func TestValidateCatalogValueMatchesAcceptedConfigurationFormats(t *testing.T) {
	specs := specByKey()
	valid := map[string][]string{
		"IPADDRESS":         {"ah4c", "ah4c:7654", "http://192.168.200.40:7655", "https://ah4c.example"},
		"CHANNELSIP":        {"channels", "channels:8080", "channels:8089", "channels:8090", "http://channels:9999"},
		"ALERT_SMTP_SERVER": {"smtp.example.com", "smtp.example.com:587"},
		"ALERT_AUTH_SERVER": {"smtp.example.com", "smtp.example.com:587"},
		"ALERT_EMAIL_FROM":  {"ah4c@example.com", "AH4C Alerts <ah4c@example.com>"},
		"ALERT_EMAIL_TO":    {"viewer@example.com"},
		"ALERT_WEBHOOK_URL": {"https://alerts.example/hook?reason=$reason"},
		"FASTCHANNELS_URL":  {"http://fastchannels:8000"},
		"PLAYBACK_DELAY":    {"30", "30s", "1m30s", "24h"},
		"KEEP_WATCHING":     {"true", "4h", "240m", "defined-by-the-script"},
		"LIVETV_ATTEMPTS":   {"3", "defined-by-the-script"},
		"SPEED_MODE":        {"TRUE", "1", "defined-by-the-script"},
		"UPDATE_SCRIPTS":    {"true", "TRUE", "False"},
	}
	for key, values := range valid {
		for _, value := range values {
			if err := validateCatalogValue(specs[key], value); err != nil {
				t.Errorf("validateCatalogValue(%s, %q): %v", key, value, err)
			}
		}
		if err := validateCatalogValue(specs[key], ""); err != nil {
			t.Errorf("validateCatalogValue(%s, blank): %v", key, err)
		}
	}

	invalid := map[string][]string{
		"IPADDRESS":         {"http://ah4c/path", "ftp://ah4c", "http://ah4c:99999"},
		"CHANNELSIP":        {"http://user:pass@channels", "channels/path"},
		"ALERT_SMTP_SERVER": {"http://smtp.example.com"},
		"ALERT_EMAIL_TO":    {"not-an-email"},
		"ALERT_WEBHOOK_URL": {"ftp://alerts.example/hook"},
		"PLAYBACK_DELAY":    {"later", "-1", "-1s"},
		"UPDATE_SCRIPTS":    {"yes"},
	}
	for key, values := range invalid {
		for _, value := range values {
			if err := validateCatalogValue(specs[key], value); err == nil {
				t.Errorf("validateCatalogValue(%s, %q) unexpectedly passed", key, value)
			}
		}
	}
}

func TestValidatedSettingsNormalizesBooleanAndChecksTunerFields(t *testing.T) {
	request := configSaveRequest{
		Vars: map[string]string{"UPDATE_SCRIPTS": "TRUE"},
		Tuners: &[]TunerSpec{{
			TunerIP: "encoder-box:5555", EncoderURL: "http://encoder-box:8090/stream",
			CMD: "ffmpeg -i input", TEECMD: "tee /tmp/output",
		}},
	}
	settings, err := validatedSettingsRequest(request, emptySettings())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Vars["UPDATE_SCRIPTS"] != "true" {
		t.Fatalf("UPDATE_SCRIPTS = %q", settings.Vars["UPDATE_SCRIPTS"])
	}
	if len(settings.Tuners) != 1 || settings.Tuners[0].TunerIP != "encoder-box:5555" {
		t.Fatalf("tuners = %#v", settings.Tuners)
	}

	bad := request
	bad.Tuners = &[]TunerSpec{{CMD: "first\nsecond"}}
	if _, err := validatedSettingsRequest(bad, emptySettings()); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("multiline command error = %v", err)
	}
}

func TestRestartSafelyRejectsPendingTune(t *testing.T) {
	r := configAPITestSetup(t, emptySettings(), map[string]bool{})
	tuneMu.Lock()
	oldPending := append([]time.Time(nil), tunePending...)
	tunePending = []time.Time{time.Now()}
	tuneMu.Unlock()
	t.Cleanup(func() {
		tuneMu.Lock()
		tunePending = oldPending
		tuneMu.Unlock()
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/config/restart", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "starting or active") {
		t.Fatalf("pending-tune restart = %d %s", w.Code, w.Body.String())
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

func localScriptUploadRequest(t *testing.T, device, app string, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("device", device); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("app", app); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/config/local-scripts", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestLocalScriptUploadCreatesCompletePackageAtomically(t *testing.T) {
	root := t.TempDir()
	oldRoot := localScriptsRootOverride
	localScriptsRootOverride = root
	t.Cleanup(func() { localScriptsRootOverride = oldRoot })
	router := configAPITestSetup(t, emptySettings(), map[string]bool{})
	files := map[string]string{
		"bmitune.sh":     "#!/bin/sh\n",
		"prebmitune.sh":  "#!/bin/sh\n",
		"stopbmitune.sh": "#!/bin/sh\n",
		"helper.json":    "{}\n",
	}

	created := httptest.NewRecorder()
	router.ServeHTTP(created, localScriptUploadRequest(t, "my-device", "my-app", files))
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"selection":"scripts/my-device/my-app"`) {
		t.Fatalf("script upload = %d %s", created.Code, created.Body.String())
	}
	packageDir := filepath.Join(root, "my-device", "my-app")
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(packageDir, name))
		if err != nil || string(got) != content {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
		info, err := os.Stat(filepath.Join(packageDir, name))
		if err != nil || info.Mode().Perm()&0100 == 0 {
			t.Fatalf("%s is not executable: %v, %v", name, info, err)
		}
	}

	again := httptest.NewRecorder()
	router.ServeHTTP(again, localScriptUploadRequest(t, "my-device", "my-app", files))
	if again.Code != http.StatusConflict {
		t.Fatalf("duplicate script upload = %d %s", again.Code, again.Body.String())
	}
}

func TestLocalScriptUploadRejectsIncompletePackage(t *testing.T) {
	root := t.TempDir()
	oldRoot := localScriptsRootOverride
	localScriptsRootOverride = root
	t.Cleanup(func() { localScriptsRootOverride = oldRoot })
	router := configAPITestSetup(t, emptySettings(), map[string]bool{})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, localScriptUploadRequest(t, "my-device", "my-app", map[string]string{"bmitune.sh": "#!/bin/sh\n"}))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "prebmitune.sh") || !strings.Contains(response.Body.String(), "stopbmitune.sh") {
		t.Fatalf("incomplete script upload = %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "my-device", "my-app")); !os.IsNotExist(err) {
		t.Fatalf("incomplete package was created: %v", err)
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

func TestADBAuthorizationPendingRecognizesConnectAndStateMessages(t *testing.T) {
	for _, output := range []string{
		"unauthorized",
		"failed to authenticate to 192.168.1.20:5555",
		"device authorization is pending",
	} {
		if !adbAuthorizationPending(output) {
			t.Errorf("adbAuthorizationPending(%q) = false", output)
		}
	}
	if adbAuthorizationPending("connected to 192.168.1.20:5555", "device") {
		t.Fatal("authorized ADB connection was reported as pending")
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
