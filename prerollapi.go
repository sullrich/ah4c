package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

const maxPrerollUploadBytes int64 = 2 << 30

var errPrerollTuneActive = errors.New("a tuner is active; upload canceled so it cannot interfere with the stream")

var (
	prerollAPIMu   sync.Mutex
	prerollAPIRoot = prerollMount
)

type prerollFileStatus struct {
	Present       bool   `json:"present"`
	Name          string `json:"name,omitempty"`
	Size          int64  `json:"size,omitempty"`
	Directory     bool   `json:"directory"`
	Managed       bool   `json:"managed"`
	RestartNeeded bool   `json:"restartNeeded"`
	Message       string `json:"message,omitempty"`
}

func registerPrerollConfigRoutes(r *gin.Engine) {
	r.GET("/api/config/preroll", getPrerollConfigHandler)
	r.POST("/api/config/preroll", uploadPrerollConfigHandler)
	r.DELETE("/api/config/preroll", deletePrerollConfigHandler)
}

func getPrerollConfigHandler(c *gin.Context) {
	prerollAPIMu.Lock()
	defer prerollAPIMu.Unlock()
	status, err := inspectPrerollFile(prerollAPIRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}

func uploadPrerollConfigHandler(c *gin.Context) {
	prerollAPIMu.Lock()
	defer prerollAPIMu.Unlock()

	if err := requirePrerollDirectory(prerollAPIRoot); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if !connectionChecksAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": errPrerollTuneActive.Error()})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPrerollUploadBytes+(1<<20))
	reader, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Choose a pre-roll file to upload"})
		return
	}
	part, err := prerollUploadPart(reader)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer part.Close()

	extension := safePrerollExtension(part.FileName())
	target := filepath.Join(prerollAPIRoot, "preroll"+extension)
	temporary, err := os.CreateTemp(prerollAPIRoot, ".preroll-upload-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not create the pre-roll file: %v", err)})
		return
	}
	temporaryName := temporary.Name()
	keepTemporary := false
	defer func() {
		if !keepTemporary {
			_ = os.Remove(temporaryName)
		}
	}()

	written, copyErr := copyPrerollUpload(c.Request.Context(), temporary, part)
	closeErr := temporary.Close()
	if copyErr != nil {
		status := http.StatusInternalServerError
		if strings.Contains(copyErr.Error(), "request body too large") {
			status = http.StatusRequestEntityTooLarge
		} else if errors.Is(copyErr, errPrerollTuneActive) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": fmt.Sprintf("Could not save the pre-roll file: %v", copyErr)})
		return
	}
	if closeErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not finish the pre-roll file: %v", closeErr)})
		return
	}
	if written == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "The selected file is empty"})
		return
	}
	if err := os.Chmod(temporaryName, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not set pre-roll file permissions: %v", err)})
		return
	}
	if err := os.Rename(temporaryName, target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not install the pre-roll file: %v", err)})
		return
	}
	keepTemporary = true
	if err := removeOtherManagedPrerolls(prerollAPIRoot, target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("The file was uploaded, but the previous pre-roll could not be removed: %v", err)})
		return
	}
	markPrerollRestartNeeded()
	logger("[PREROLL] uploaded %s (%s); restart required to prepare it", target, byteCount(written))
	status, _ := inspectPrerollFile(prerollAPIRoot)
	c.JSON(http.StatusCreated, status)
}

func deletePrerollConfigHandler(c *gin.Context) {
	prerollAPIMu.Lock()
	defer prerollAPIMu.Unlock()
	if err := requirePrerollDirectory(prerollAPIRoot); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	selected, _, err := selectedPrerollFile(prerollAPIRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if selected == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "No pre-roll file is stored"})
		return
	}
	if err := os.Remove(selected); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not delete the pre-roll file: %v", err)})
		return
	}
	markPrerollRestartNeeded()
	logger("[PREROLL] deleted %s; restart required to stop using it", selected)
	status, _ := inspectPrerollFile(prerollAPIRoot)
	c.JSON(http.StatusOK, status)
}

func inspectPrerollFile(root string) (prerollFileStatus, error) {
	status := prerollFileStatus{RestartNeeded: prerollRestartNeeded()}
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		status.Directory = true
		status.Managed = true
		status.Message = "The pre-roll folder is not mounted"
		return status, nil
	}
	if err != nil {
		return status, fmt.Errorf("could not read the pre-roll mount: %w", err)
	}
	if info.Mode().IsRegular() {
		status.Present = true
		status.Name = filepath.Base(root)
		status.Size = info.Size()
		status.Message = "This pre-roll is mounted as a single host file and must be managed on the host"
		return status, nil
	}
	if !info.IsDir() {
		return status, fmt.Errorf("the pre-roll mount is not a file or folder")
	}
	status.Directory = true
	status.Managed = true
	selected, _, err := selectedPrerollFile(root)
	if err != nil {
		return status, err
	}
	if selected == "" {
		return status, nil
	}
	file, err := os.Stat(selected)
	if err != nil {
		return status, fmt.Errorf("could not read the selected pre-roll: %w", err)
	}
	status.Present = true
	status.Name = filepath.Base(selected)
	status.Size = file.Size()
	return status, nil
}

func requirePrerollDirectory(root string) error {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("the pre-roll folder is not available: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("the pre-roll mount is not available: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the pre-roll is mounted as a single host file; manage that file on the host")
	}
	return nil
}

func selectedPrerollFile(dir string) (string, int, error) {
	selected, count, err := pickPrerollFile(dir)
	if err != nil {
		return "", 0, fmt.Errorf("could not list the pre-roll folder: %w", err)
	}
	return selected, count, nil
}

func prerollUploadPart(reader *multipart.Reader) (*multipart.Part, error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("choose a pre-roll file to upload")
		}
		if err != nil {
			return nil, err
		}
		if part.FormName() == "file" && strings.TrimSpace(part.FileName()) != "" {
			return part, nil
		}
		_ = part.Close()
	}
}

func safePrerollExtension(name string) string {
	extension := strings.ToLower(filepath.Ext(filepath.Base(name)))
	if len(extension) < 2 || len(extension) > 12 {
		return ".media"
	}
	for _, character := range extension[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return ".media"
		}
	}
	return extension
}

func copyPrerollUpload(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 256*1024)
	var total int64
	for {
		read, readErr := source.Read(buffer)
		if read > 0 {
			if !connectionChecksAllowed() {
				return total, errPrerollTuneActive
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			default:
			}
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func removeOtherManagedPrerolls(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if path == keep || !entry.Type().IsRegular() || !strings.HasPrefix(strings.ToLower(entry.Name()), "preroll.") {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func markPrerollRestartNeeded() {
	configAPIMu.Lock()
	configRestartNeeded = true
	configRestartKeys["pre-roll"] = true
	configAPIMu.Unlock()
}

func prerollRestartNeeded() bool {
	configAPIMu.Lock()
	defer configAPIMu.Unlock()
	return configRestartKeys["pre-roll"]
}
