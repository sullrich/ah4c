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

// prerollExternalMessage is what the page and a refused upload both say when a
// file placed in the pre-roll folder on the server owns the pre-roll.
const prerollExternalMessage = "A pre-roll file placed in the pre-roll folder on the server is in use and always takes precedence. Remove it from the server to manage the pre-roll here."

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
	Deletable     bool   `json:"deletable"`
	External      bool   `json:"external"`
	Shadowed      string `json:"shadowed,omitempty"`
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
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": errPrerollTuneActive.Error()})
		return
	}
	choice, err := selectedPrerollFile(prerollAPIRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if choice.External {
		c.JSON(http.StatusConflict, gin.H{"error": prerollExternalMessage})
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

	filename, err := safePrerollFilename(part.FileName())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	previous, err := managedPrerollSelection(prerollAPIRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not read the current pre-roll selection: %v", err)})
		return
	}
	target := filepath.Join(prerollAPIRoot, filename)
	// The marker-named entry may be replaced only when it is the ordinary file
	// Settings put there. Anything else at that name belongs to the owner.
	if target != previous || !prerollRegularFile(target) {
		if _, err := os.Lstat(target); err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("%s already exists outside Settings; rename the upload to preserve that file", filename)})
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not inspect the pre-roll destination: %v", err)})
			return
		}
	}
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
	if err := writePrerollSelection(prerollAPIRoot, filename); err != nil {
		// Without the marker this file would count as one placed on the server
		// and lock the page, so an upload that cannot be recorded is taken back.
		if target != previous {
			_ = os.Remove(target)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("The upload could not be recorded, so it was not kept: %v", err)})
		return
	}
	// The upload being replaced is removed only while it is still the ordinary
	// file Settings put there. A link or folder at that name is the owner's.
	if previous != "" && previous != target && prerollRegularFile(previous) {
		if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("The file was uploaded, but the previous pre-roll could not be removed: %v", err)})
			return
		}
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
	// Only the file this page uploaded is ever removed. A file placed in the
	// folder on the server belongs to whoever put it there.
	managed, err := managedPrerollSelection(prerollAPIRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if managed == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "No pre-roll file was uploaded here. A file placed in the pre-roll folder on the server has to be removed there."})
		return
	}
	if _, err := os.Lstat(managed); errors.Is(err, os.ErrNotExist) {
		// The upload is already gone. Forget it, and say so without pretending
		// anything was removed or that a restart is needed.
		if err := clearPrerollSelection(prerollAPIRoot); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("The selection could not be cleared: %v", err)})
			return
		}
		logger("[PREROLL] forgot the missing upload %s", managed)
		status, _ := inspectPrerollFile(prerollAPIRoot)
		c.JSON(http.StatusOK, status)
		return
	}
	if !prerollRegularFile(managed) {
		c.JSON(http.StatusConflict, gin.H{"error": "The selected name is not an ordinary file and belongs to the server. Remove it there."})
		return
	}
	if err := os.Remove(managed); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not delete the pre-roll file: %v", err)})
		return
	}
	if err := clearPrerollSelection(prerollAPIRoot); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("The pre-roll was deleted, but the selection could not be cleared: %v", err)})
		return
	}
	markPrerollRestartNeeded()
	logger("[PREROLL] deleted %s; restart required to stop using it", managed)
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
	choice, err := selectedPrerollFile(root)
	if err != nil {
		return status, err
	}
	status.External = choice.External
	status.Shadowed = choice.Shadowed
	if choice.Path == "" {
		return status, nil
	}
	file, err := os.Stat(choice.Path)
	if err != nil {
		return status, fmt.Errorf("could not read the selected pre-roll: %w", err)
	}
	status.Present = true
	status.Name = choice.Name
	status.Size = file.Size()
	// The upload this page made can always be taken back, even when a file on
	// the server is shadowing it. Nothing else here can.
	status.Deletable = !choice.External || choice.Shadowed != ""
	if choice.External {
		status.Message = prerollExternalNotice(choice.Shadowed)
	}
	return status, nil
}

// prerollExternalNotice explains a folder a file on the server owns, and says
// which file Delete would take back when an upload is sitting behind it.
func prerollExternalNotice(shadowed string) string {
	if shadowed == "" {
		return prerollExternalMessage
	}
	return fmt.Sprintf("%s The file you uploaded here, %s, is not being used; Delete removes that one.", prerollExternalMessage, shadowed)
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

func selectedPrerollFile(dir string) (prerollChoice, error) {
	choice, err := pickPrerollFile(dir)
	if err != nil {
		return prerollChoice{}, fmt.Errorf("could not list the pre-roll folder: %w", err)
	}
	return choice, nil
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

func safePrerollFilename(name string) (string, error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") || len(name) > 255 {
		return "", fmt.Errorf("the pre-roll filename is not safe")
	}
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", fmt.Errorf("the pre-roll filename is not safe")
	}
	return name, nil
}

func managedPrerollSelection(dir string) (string, error) {
	name, err := prerollSelectedName(dir)
	if err != nil || name == "" {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func writePrerollSelection(dir, name string) error {
	temporary, err := os.CreateTemp(dir, ".preroll-selection-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.WriteString(name + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(dir, prerollSelectionName))
}

func clearPrerollSelection(dir string) error {
	err := os.Remove(filepath.Join(dir, prerollSelectionName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func copyPrerollUpload(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 256*1024)
	var total int64
	for {
		read, readErr := source.Read(buffer)
		if read > 0 {
			if !configOperationsAllowed() {
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
