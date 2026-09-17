package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var connectionCheckSlots = make(chan struct{}, 4)

func registerConnectionConfigRoutes(r *gin.Engine) {
	r.POST("/api/config/check-connection", checkConfigConnectionHandler)
	registerAppleTVPairingRoutes(r)
}

type connectionCheckRequest struct {
	Index    int       `json:"index"`
	Tuner    TunerSpec `json:"tuner"`
	UsePYATV bool      `json:"usePyatv"`
}

type connectionCheckResult struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

type tunerConnectionCheck struct {
	Index   int                   `json:"index"`
	Device  connectionCheckResult `json:"device"`
	Encoder connectionCheckResult `json:"encoder"`
}

func checkConfigConnectionHandler(c *gin.Context) {
	var request connectionCheckRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "connection checks are unavailable while a tune is starting or active"})
		return
	}
	select {
	case connectionCheckSlots <- struct{}{}:
		defer func() { <-connectionCheckSlots }()
	case <-c.Request.Context().Done():
		return
	}
	if !configOperationsAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "connection checks are unavailable while a tune is starting or active"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 7*time.Second)
	defer cancel()
	deviceResult := make(chan connectionCheckResult, 1)
	encoderResult := make(chan connectionCheckResult, 1)
	go func() {
		if request.UsePYATV {
			deviceResult <- checkPYATVConnection(ctx, request.Tuner.TunerIP)
			return
		}
		deviceResult <- checkADBConnection(ctx, request.Tuner.TunerIP)
	}()
	go func() { encoderResult <- checkEncoderConnection(ctx, request.Tuner.EncoderURL) }()
	result := tunerConnectionCheck{Index: request.Index}
	result.Device = <-deviceResult
	result.Encoder = <-encoderResult
	c.JSON(http.StatusOK, result)
}

func checkADBConnection(ctx context.Context, target string) connectionCheckResult {
	target = strings.TrimSpace(target)
	if target == "" {
		return connectionCheckResult{State: "empty", Message: "Enter a device address"}
	}
	address, err := addressWithDefaultPort(target, "5555")
	if err != nil {
		return connectionCheckResult{State: "error", Message: "Invalid device address"}
	}
	if err := dialConnection(ctx, address); err != nil {
		return connectionCheckResult{State: "error", Message: "Device address is unreachable"}
	}
	if ok, message := adbKeysAvailable(); !ok {
		return connectionCheckResult{State: "error", Message: message}
	}
	if _, err := exec.LookPath("adb"); err != nil {
		return connectionCheckResult{State: "error", Message: "ADB is not installed"}
	}
	if !configOperationsAllowed() {
		return connectionCheckResult{State: "skipped", Message: "Stopped because a tune started"}
	}
	connectOutput, _ := boundedCommand(ctx, 2500*time.Millisecond, "adb", "connect", address)
	state, err := boundedCommand(ctx, 2*time.Second, "adb", "-s", address, "get-state")
	state = strings.TrimSpace(state)
	if err != nil || state != "device" {
		if adbAuthorizationPending(connectOutput, state) {
			// A fresh ADB handshake makes Android show its authorization dialog.
			// Return immediately so the browser never waits for the person to find
			// the remote and approve it.
			_, _ = boundedCommand(ctx, time.Second, "adb", "disconnect", address)
			_, _ = boundedCommand(ctx, 2500*time.Millisecond, "adb", "connect", address)
			return connectionCheckResult{State: "authorize", Message: "Look at the Android device and approve Allow USB debugging, then check again"}
		}
		if strings.Contains(strings.ToLower(state), "offline") {
			return connectionCheckResult{State: "error", Message: "ADB reports the device is offline"}
		}
		return connectionCheckResult{State: "error", Message: "ADB is not connected and authorized"}
	}
	if !configOperationsAllowed() {
		return connectionCheckResult{State: "skipped", Message: "Stopped because a tune started"}
	}
	if _, err := boundedCommand(ctx, 2*time.Second, "adb", "-s", address, "shell", "true"); err != nil {
		return connectionCheckResult{State: "error", Message: "ADB connected, but commands do not work"}
	}
	return connectionCheckResult{State: "ready", Message: "ADB connected and authorized"}
}

func checkPYATVConnection(ctx context.Context, target string) connectionCheckResult {
	target = strings.TrimSpace(target)
	if target == "" {
		return connectionCheckResult{State: "empty", Message: "Enter an Apple TV address"}
	}
	config := appleTVConfigPath
	if info, err := os.Stat(config); err != nil || !info.Mode().IsRegular() {
		return connectionCheckResult{State: "pair", Message: "Pair this Apple TV to continue"}
	}
	if _, err := exec.LookPath(appleTVRemoteExecutable); err != nil {
		return connectionCheckResult{State: "error", Message: "pyatv is not installed"}
	}
	if !configOperationsAllowed() {
		return connectionCheckResult{State: "skipped", Message: "Stopped because a tune started"}
	}
	if _, err := boundedCommand(ctx, 4*time.Second, appleTVRemoteExecutable, "--storage-filename", config, "-s", target, "device_info"); err != nil {
		return connectionCheckResult{State: "pair", Message: "The Apple TV did not accept the saved pairing. Confirm its address or pair it again"}
	}
	return connectionCheckResult{State: "ready", Message: "Apple TV is reachable and paired"}
}

func adbAuthorizationPending(outputs ...string) bool {
	combined := strings.ToLower(strings.Join(outputs, " "))
	return strings.Contains(combined, "unauthorized") || strings.Contains(combined, "authenticate") || strings.Contains(combined, "authorization")
}

func checkEncoderConnection(ctx context.Context, rawURL string) connectionCheckResult {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return connectionCheckResult{State: "empty", Message: "Enter the encoder stream address"}
	}
	address, err := encoderNetworkAddress(rawURL)
	if err != nil {
		return connectionCheckResult{State: "error", Message: "Enter the full encoder stream address, starting with http:// or https://"}
	}
	if err := dialConnection(ctx, address); err != nil {
		return connectionCheckResult{State: "error", Message: "The encoder address was understood, but ah4c could not reach it"}
	}
	return connectionCheckResult{State: "ready", Message: "The encoder is reachable"}
}

func addressWithDefaultPort(value, defaultPort string) (string, error) {
	if host, port, err := net.SplitHostPort(value); err == nil {
		if host == "" || port == "" {
			return "", fmt.Errorf("host and port are required")
		}
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(value, ":") && net.ParseIP(value) == nil {
		return "", fmt.Errorf("invalid host and port")
	}
	if value == "" {
		return "", fmt.Errorf("host is required")
	}
	return net.JoinHostPort(value, defaultPort), nil
}

func encoderNetworkAddress(rawURL string) (string, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("the encoder stream address must start with http:// or https://")
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("the encoder stream address must start with http:// or https://")
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

func dialConnection(ctx context.Context, address string) error {
	dialCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

func boundedCommand(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, name, args...).CombinedOutput()
	return string(output), err
}

func adbKeysAvailable() (bool, string) {
	privateKey := "/root/.android/adbkey"
	publicKey := privateKey + ".pub"
	if custom := strings.TrimSpace(os.Getenv("ADB_VENDOR_KEYS")); custom != "" {
		privateKey = strings.Split(custom, string(os.PathListSeparator))[0]
		publicKey = privateKey + ".pub"
	}
	privateInfo, privateErr := os.Stat(privateKey)
	publicInfo, publicErr := os.Stat(publicKey)
	if privateErr != nil || publicErr != nil || !privateInfo.Mode().IsRegular() || !publicInfo.Mode().IsRegular() {
		return false, "ADB keypair is missing"
	}
	return true, ""
}
