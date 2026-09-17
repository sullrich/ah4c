package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func prerollAPITestSetup(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	oldRoot := prerollAPIRoot
	prerollAPIRoot = root
	configAPIMu.Lock()
	oldRestart := configRestartNeeded
	oldKeys := configRestartKeys
	configRestartNeeded = false
	configRestartKeys = map[string]bool{}
	configAPIMu.Unlock()
	t.Cleanup(func() {
		prerollAPIRoot = oldRoot
		configAPIMu.Lock()
		configRestartNeeded = oldRestart
		configRestartKeys = oldKeys
		configAPIMu.Unlock()
	})
	router := gin.New()
	registerPrerollConfigRoutes(router)
	return router, root
}

func prerollUploadRequest(t *testing.T, name string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/config/preroll", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestPrerollAPIUploadsReplacesAndDeletes(t *testing.T) {
	router, root := prerollAPITestSetup(t)

	empty := httptest.NewRecorder()
	router.ServeHTTP(empty, httptest.NewRequest(http.MethodGet, "/api/config/preroll", nil))
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"present":false`) {
		t.Fatalf("empty status = %d %s", empty.Code, empty.Body.String())
	}

	uploaded := httptest.NewRecorder()
	router.ServeHTTP(uploaded, prerollUploadRequest(t, "family-photo.PNG", []byte("first")))
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("first upload = %d %s", uploaded.Code, uploaded.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(root, "preroll.png")); err != nil || string(got) != "first" {
		t.Fatalf("stored first upload = %q, %v", got, err)
	}

	replaced := httptest.NewRecorder()
	router.ServeHTTP(replaced, prerollUploadRequest(t, "clip.MP4", []byte("second")))
	if replaced.Code != http.StatusCreated {
		t.Fatalf("replacement upload = %d %s", replaced.Code, replaced.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "preroll.png")); !os.IsNotExist(err) {
		t.Fatalf("old pre-roll remains: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "preroll.mp4")); err != nil || string(got) != "second" {
		t.Fatalf("stored replacement = %q, %v", got, err)
	}

	deleted := httptest.NewRecorder()
	router.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/config/preroll", nil))
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"present":false`) {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	configAPIMu.Lock()
	restart := configRestartNeeded && configRestartKeys["pre-roll"]
	configAPIMu.Unlock()
	if !restart {
		t.Fatal("pre-roll change did not request a restart")
	}
}

func TestPrerollAPIReportsSingleFileMountAsHostManaged(t *testing.T) {
	router, _ := prerollAPITestSetup(t)
	file := filepath.Join(t.TempDir(), "host-preroll.mp4")
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	prerollAPIRoot = file

	statusRecorder := httptest.NewRecorder()
	router.ServeHTTP(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/config/preroll", nil))
	var status prerollFileStatus
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if statusRecorder.Code != http.StatusOK || !status.Present || status.Managed || status.Directory {
		t.Fatalf("single-file status = %d %#v", statusRecorder.Code, status)
	}

	upload := httptest.NewRecorder()
	router.ServeHTTP(upload, prerollUploadRequest(t, "replacement.mp4", []byte("new")))
	if upload.Code != http.StatusConflict || !strings.Contains(upload.Body.String(), "single host file") {
		t.Fatalf("single-file upload = %d %s", upload.Code, upload.Body.String())
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "video" {
		t.Fatalf("host file changed = %q, %v", got, err)
	}
}

func TestSafePrerollExtension(t *testing.T) {
	for name, want := range map[string]string{
		"clip.MP4":       ".mp4",
		"photo.jpeg":     ".jpeg",
		"no-extension":   ".media",
		"unsafe.bad-ext": ".media",
	} {
		if got := safePrerollExtension(name); got != want {
			t.Errorf("safePrerollExtension(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPrerollAPIAndStartupUseTheSameFileSelection(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.mp4", "preroll.jpg", "zulu.ts", ".hidden.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	startup, count, err := pickPrerollFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	api, apiCount, err := selectedPrerollFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if startup != api || filepath.Base(api) != "preroll.jpg" || count != 3 || apiCount != count {
		t.Fatalf("startup=%q/%d api=%q/%d", startup, count, api, apiCount)
	}
}
