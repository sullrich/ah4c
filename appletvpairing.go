package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const appleTVPairingWait = 20 * time.Second

var (
	appleTVConfigPath       = "/root/.android/.pyatv.conf"
	appleTVRemoteExecutable = "atvremote"
	appleTVPairingMu        sync.Mutex
	appleTVPairingCurrent   *appleTVPairingSession
	appleTVPINPattern       = regexp.MustCompile(`^[0-9]{4}$`)
)

type appleTVPairingSession struct {
	id       string
	target   string
	tempDir  string
	tempFile string
	command  *exec.Cmd
	stdin    io.WriteCloser
	cancel   context.CancelFunc

	mu       sync.Mutex
	output   strings.Builder
	prompted bool
	pinSent  bool
	finished bool
	canceled bool
	result   error
	prompt   chan struct{}
	done     chan struct{}
	promptMu sync.Once
}

type appleTVPairingStartRequest struct {
	Target string `json:"target"`
}

type appleTVPairingPINRequest struct {
	SessionID string `json:"sessionId"`
	PIN       string `json:"pin"`
}

type appleTVPairingCancelRequest struct {
	SessionID string `json:"sessionId"`
}

func registerAppleTVPairingRoutes(r *gin.Engine) {
	r.POST("/api/config/apple-tv-pairing/start", startAppleTVPairingHandler)
	r.POST("/api/config/apple-tv-pairing/pin", finishAppleTVPairingHandler)
	r.DELETE("/api/config/apple-tv-pairing", cancelAppleTVPairingHandler)
}

func startAppleTVPairingHandler(c *gin.Context) {
	var request appleTVPairingStartRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the Apple TV address first"})
		return
	}
	request.Target = strings.TrimSpace(request.Target)
	if err := validateBareHostPort(request.Target); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the Apple TV name or IP address"})
		return
	}
	if !connectionChecksAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "Apple TV pairing is unavailable while a tune is starting or active"})
		return
	}

	session, err := startAppleTVPairing(request.Target)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errAppleTVPairingBusy) {
			status = http.StatusConflict
		} else if errors.Is(err, exec.ErrNotFound) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": appleTVPairingMessage(err)})
		return
	}

	timer := time.NewTimer(appleTVPairingWait)
	defer timer.Stop()
	select {
	case <-session.prompt:
		c.JSON(http.StatusOK, gin.H{
			"sessionId": session.id,
			"target":    session.target,
			"message":   "A four-digit PIN should now be shown on the Apple TV",
		})
	case <-session.done:
		c.JSON(http.StatusBadGateway, gin.H{"error": appleTVPairingMessage(session.resultError())})
	case <-timer.C:
		session.stop()
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "The Apple TV did not show a PIN. Confirm its address, make sure it is awake, and try again"})
	case <-c.Request.Context().Done():
		session.stop()
	}
}

func finishAppleTVPairingHandler(c *gin.Context) {
	var request appleTVPairingPINRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the four-digit PIN shown on the Apple TV"})
		return
	}
	request.PIN = strings.TrimSpace(request.PIN)
	if !appleTVPINPattern.MatchString(request.PIN) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter all four numbers shown on the Apple TV"})
		return
	}
	session := currentAppleTVPairing(request.SessionID)
	if session == nil {
		c.JSON(http.StatusGone, gin.H{"error": "That pairing attempt has ended. Start pairing again"})
		return
	}
	if !connectionChecksAllowed() {
		session.stop()
		c.JSON(http.StatusConflict, gin.H{"error": "Pairing stopped because a tune started. Try again after the tune ends"})
		return
	}
	if err := session.sendPIN(request.PIN); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": appleTVPairingMessage(err)})
		return
	}

	timer := time.NewTimer(appleTVPairingWait)
	defer timer.Stop()
	select {
	case <-session.done:
		if err := session.resultError(); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": appleTVPairingMessage(err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"state": "ready", "message": "Apple TV pairing was saved"})
	case <-timer.C:
		session.stop()
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "The Apple TV did not finish pairing. Check the PIN and try again"})
	case <-c.Request.Context().Done():
	}
}

func cancelAppleTVPairingHandler(c *gin.Context) {
	var request appleTVPairingCancelRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Pairing session is missing"})
		return
	}
	session := currentAppleTVPairing(request.SessionID)
	if session != nil {
		session.stop()
	}
	c.JSON(http.StatusOK, gin.H{"canceled": true})
}

var (
	errAppleTVPairingBusy = errors.New("another Apple TV pairing is already in progress")
	errAppleTVPINRejected = errors.New("Apple TV rejected the PIN")
)

func startAppleTVPairing(target string) (*appleTVPairingSession, error) {
	if _, err := exec.LookPath(appleTVRemoteExecutable); err != nil {
		return nil, err
	}
	appleTVPairingMu.Lock()
	defer appleTVPairingMu.Unlock()
	if appleTVPairingCurrent != nil && !appleTVPairingCurrent.isFinished() {
		return nil, errAppleTVPairingBusy
	}

	configDir := filepath.Dir(appleTVConfigPath)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare pairing folder: %w", err)
	}
	tempDir, err := os.MkdirTemp(configDir, ".pyatv-pair-")
	if err != nil {
		return nil, fmt.Errorf("prepare pairing folder: %w", err)
	}
	tempFile := filepath.Join(tempDir, filepath.Base(appleTVConfigPath))
	if existing, readErr := os.ReadFile(appleTVConfigPath); readErr == nil {
		if err := os.WriteFile(tempFile, existing, 0o600); err != nil {
			_ = os.RemoveAll(tempDir)
			return nil, fmt.Errorf("prepare saved pairing: %w", err)
		}
	} else if !os.IsNotExist(readErr) {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("read saved pairing: %w", readErr)
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create pairing session: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	session := &appleTVPairingSession{
		id:       hex.EncodeToString(idBytes),
		target:   target,
		tempDir:  tempDir,
		tempFile: tempFile,
		cancel:   cancel,
		prompt:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	session.command = exec.CommandContext(ctx, appleTVRemoteExecutable,
		"--storage-filename", tempFile, "-s", target, "--protocol", "companion", "pair")
	stdin, err := session.command.StdinPipe()
	if err != nil {
		cancel()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("open pairing input: %w", err)
	}
	session.stdin = stdin
	session.command.Stdout = session
	session.command.Stderr = session
	if err := session.command.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("start pairing: %w", err)
	}
	appleTVPairingCurrent = session
	go session.wait()
	return session, nil
}

func (session *appleTVPairingSession) Write(data []byte) (int, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.output.Len() < 32<<10 {
		remaining := (32 << 10) - session.output.Len()
		if len(data) > remaining {
			session.output.Write(data[:remaining])
		} else {
			session.output.Write(data)
		}
	}
	if strings.Contains(strings.ToLower(session.output.String()), "enter pin on screen") {
		session.prompted = true
		session.promptMu.Do(func() { close(session.prompt) })
	}
	return len(data), nil
}

func (session *appleTVPairingSession) sendPIN(pin string) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.finished {
		return errors.New("pairing attempt has ended")
	}
	if !session.prompted {
		return errors.New("the Apple TV has not shown a PIN yet")
	}
	_, err := io.WriteString(session.stdin, pin+"\n")
	if err != nil {
		return fmt.Errorf("send PIN: %w", err)
	}
	session.pinSent = true
	return nil
}

func (session *appleTVPairingSession) wait() {
	err := session.command.Wait()
	_ = session.stdin.Close()

	session.mu.Lock()
	canceled := session.canceled
	pinSent := session.pinSent
	session.mu.Unlock()
	if err != nil && pinSent && !canceled {
		err = fmt.Errorf("%w: %v", errAppleTVPINRejected, err)
	}
	if err == nil && !canceled {
		info, statErr := os.Stat(session.tempFile)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			err = errors.New("pairing did not create saved credentials")
		} else if chmodErr := os.Chmod(session.tempFile, 0o600); chmodErr != nil {
			err = chmodErr
		} else if renameErr := os.Rename(session.tempFile, appleTVConfigPath); renameErr != nil {
			err = renameErr
		}
	}
	_ = os.RemoveAll(session.tempDir)
	session.cancel()
	session.mu.Lock()
	session.output.Reset()
	session.result = err
	session.finished = true
	session.mu.Unlock()
	close(session.done)
}

func (session *appleTVPairingSession) stop() {
	session.mu.Lock()
	session.canceled = true
	session.mu.Unlock()
	session.cancel()
}

func (session *appleTVPairingSession) isFinished() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.finished
}

func (session *appleTVPairingSession) resultError() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.result
}

func currentAppleTVPairing(id string) *appleTVPairingSession {
	appleTVPairingMu.Lock()
	defer appleTVPairingMu.Unlock()
	if appleTVPairingCurrent == nil || appleTVPairingCurrent.id != id {
		return nil
	}
	return appleTVPairingCurrent
}

func appleTVPairingMessage(err error) string {
	if err == nil {
		return "Apple TV pairing ended"
	}
	if errors.Is(err, errAppleTVPairingBusy) {
		return "Another Apple TV is already being paired. Finish or cancel it first"
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "Apple TV support is not installed in this container"
	}
	if errors.Is(err, errAppleTVPINRejected) {
		return "The Apple TV did not accept that PIN. Start pairing again and enter the new PIN shown on the TV"
	}
	return "Apple TV pairing did not finish. Confirm the address, make sure the Apple TV is awake, and try again"
}
