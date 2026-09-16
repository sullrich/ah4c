package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const secretMask = "•••"

var requiredStreamerFiles = []string{"bmitune.sh", "prebmitune.sh", "stopbmitune.sh"}

var (
	configAPIMu          sync.Mutex
	configRestartNeeded  bool
	configRestartKeys    = map[string]bool{}
	connectionCheckSlots = make(chan struct{}, 4)
)

var defaultValues = map[string]string{
	"PYATV":                     "false",
	"ALERT_EMAIL_USE_SENDMAIL":  "false",
	"CREATE_M3US":               "false",
	"UPDATE_SCRIPTS":            "false",
	"UPDATE_M3US":               "true",
	"SPEED_MODE":                "false",
	"NULL_FRAME_INSERTION":      "false",
	"PLAYBACK_DETECTION":        "false",
	"PLAYBACK_STATIC_TIMEOUT":   "2",
	"ENCODER_CODEC":             "h264",
	"HEARTBEAT_INTERVAL":        "180",
	"ALLOW_DEBUG_VIDEO_PREVIEW": "false",
	"CC_WATCHDOG":               "false",
}

type configSaveRequest struct {
	Vars   map[string]string `json:"vars"`
	Tuners *[]TunerSpec      `json:"tuners"`
	Extra  map[string]string `json:"extra"`
}

func registerConfigRoutes(r *gin.Engine) {
	r.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/" && !settingsFileExists() {
			c.Redirect(http.StatusFound, "/settings?wizard=1")
			c.Abort()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/play/") && !streamerTuneReady(os.Getenv) {
			message := "No streamer script is selected; choose one in Settings before tuning"
			if strings.TrimSpace(os.Getenv("STREAMER_APP")) != "" {
				message = "STREAMER_APP is set to an invalid script path; correct it in Settings before tuning"
			}
			c.String(http.StatusServiceUnavailable, message)
			c.Abort()
			return
		}
		c.Next()
	})
	registerM3UIntegrationRoutes(r)
	registerPrerollConfigRoutes(r)
	if !configComplete(os.Getenv) {
		logger("[CONFIG] STREAMER_APP is invalid; correct it in Settings or remove it")
	}
	r.GET("/settings", func(c *gin.Context) {
		c.HTML(http.StatusOK, "settings.html", nil)
	})
	r.GET("/api/config", getConfigHandler)
	r.PUT("/api/config", putConfigHandler)
	r.POST("/api/config/restart", restartConfigHandler)
	r.GET("/api/config/streamers", streamersConfigHandler)
	r.GET("/api/config/local-scripts", localScriptsConfigHandler)
	r.POST("/api/config/check-connection", checkConfigConnectionHandler)
}

func getConfigHandler(c *gin.Context) {
	configAPIMu.Lock()
	response := configResponse()
	configAPIMu.Unlock()
	c.JSON(http.StatusOK, response)
}

func putConfigHandler(c *gin.Context) {
	var request configSaveRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	configAPIMu.Lock()
	defer configAPIMu.Unlock()

	envEngineMu.RLock()
	old := normalizeSettings(envSettings)
	locked := copyBoolMap(envLocked)
	envEngineMu.RUnlock()

	rejected := lockedRequestKeys(request, locked)
	if len(rejected) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "settings are owned by the environment", "locked": rejected})
		return
	}
	next, err := validatedSettingsRequest(request, old)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for key := range locked {
		if value, ok := old.Vars[key]; ok {
			next.Vars[key] = value
		}
		if value, ok := old.Extra[key]; ok {
			next.Extra[key] = value
		}
	}
	restoreLockedTunerSettings(&next, old, locked)
	changed := changedSettings(old, next)
	if err := saveSettings(next); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("could not save settings: %v", err)})
		return
	}

	specs := specByKey()
	for _, key := range changed {
		spec, cataloged := specs[key]
		if cataloged && spec.Applies == applyRestart {
			configRestartNeeded = true
			configRestartKeys[key] = true
		}
		if cataloged && spec.Applies == applyLive {
			if value, ok := next.Vars[key]; ok {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
		if strings.HasPrefix(key, "extra:") {
			extraKey := strings.TrimPrefix(key, "extra:")
			if value, ok := next.Extra[extraKey]; ok {
				_ = os.Setenv(extraKey, value)
			} else {
				_ = os.Unsetenv(extraKey)
			}
		}
	}
	if tunerSettingsChanged(old.Tuners, next.Tuners) {
		configRestartNeeded = true
		configRestartKeys["tuners"] = true
	}
	envEngineMu.Lock()
	envSettings = next
	envEngineMu.Unlock()
	logger("[CONFIG] saved settings.json; changed: %s", strings.Join(changed, ", "))
	response := configResponse()
	response["changed"] = changed
	c.JSON(http.StatusOK, response)
}

func restartConfigHandler(c *gin.Context) {
	force := c.Query("force") == "true"
	active := activeTunerNumbers()
	if len(active) > 0 && !force {
		c.JSON(http.StatusConflict, gin.H{"error": "a tuner is active", "tuners": active})
		return
	}
	envEngineMu.RLock()
	dockerManaged := envDockerManaged
	envEngineMu.RUnlock()
	if !dockerManaged {
		c.JSON(http.StatusOK, gin.H{"restart": "manual"})
		return
	}
	logger("[CONFIG] restarting to apply settings")
	c.JSON(http.StatusOK, gin.H{"restart": "scheduled"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		if !force {
			tunerLock.Lock()
			for i := range tuners {
				if tuners[i].active {
					tunerLock.Unlock()
					logger("[CONFIG] restart canceled because tuner %d became active", i+1)
					return
				}
			}
		}
		os.Exit(0)
	}()
}

func streamersConfigHandler(c *gin.Context) {
	streamers := discoverStreamers()
	if c.Query("remote") == "true" {
		remote, err := queryUpstreamStreamers()
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "streamers": streamers})
			return
		}
		streamers = mergeStreamers(streamers, remote)
	}
	c.JSON(http.StatusOK, streamers)
}

type localScriptEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type localScriptDirectory struct {
	Path       string             `json:"path"`
	Parent     string             `json:"parent"`
	Selectable bool               `json:"selectable"`
	Selection  string             `json:"selection,omitempty"`
	Missing    []string           `json:"missing,omitempty"`
	Entries    []localScriptEntry `json:"entries"`
}

func localScriptsConfigHandler(c *gin.Context) {
	root, err := filepath.Abs("scripts")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not locate the local scripts folder"})
		return
	}
	directory, err := readLocalScriptDirectory(root, c.Query("path"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, directory)
}

func readLocalScriptDirectory(root, relative string) (localScriptDirectory, error) {
	result := localScriptDirectory{Entries: []localScriptEntry{}}
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if clean == "." {
		clean = ""
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return result, fmt.Errorf("that folder is outside the local scripts folder")
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, clean)
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return result, fmt.Errorf("the local scripts folder is not available")
	}
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("that local scripts folder does not exist")
		}
		return result, fmt.Errorf("could not read the local scripts folder: %w", err)
	}
	rootPrefix := rootReal + string(filepath.Separator)
	if targetReal != rootReal && !strings.HasPrefix(targetReal, rootPrefix) {
		return result, fmt.Errorf("that folder is outside the local scripts folder")
	}
	entries, err := os.ReadDir(targetReal)
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("that local scripts folder does not exist")
		}
		return result, fmt.Errorf("could not read the local scripts folder: %w", err)
	}
	result.Path = filepath.ToSlash(clean)
	if clean != "" {
		parent := filepath.Dir(clean)
		if parent == "." {
			parent = ""
		}
		result.Parent = filepath.ToSlash(parent)
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if clean != "" && len(parts) == 2 && validScriptPathPart(parts[0]) && validScriptPathPart(parts[1]) {
		result.Missing = missingRequiredStreamerFiles(targetReal)
		if len(result.Missing) == 0 {
			result.Selectable = true
			result.Selection = "scripts/" + filepath.ToSlash(clean)
		}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			continue
		}
		entryPath := filepath.Join(clean, entry.Name())
		result.Entries = append(result.Entries, localScriptEntry{
			Name: entry.Name(), Path: filepath.ToSlash(entryPath), Directory: entry.IsDir(),
		})
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].Directory != result.Entries[j].Directory {
			return result.Entries[i].Directory
		}
		return strings.ToLower(result.Entries[i].Name) < strings.ToLower(result.Entries[j].Name)
	})
	return result, nil
}

func validScriptPathPart(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validStreamerSelection(value string) bool {
	parts := strings.Split(filepath.ToSlash(strings.TrimSpace(value)), "/")
	return len(parts) == 3 && parts[0] == "scripts" && validScriptPathPart(parts[1]) && validScriptPathPart(parts[2])
}

func scriptPackageComplete(directory string) bool {
	return len(missingRequiredStreamerFiles(directory)) == 0
}

func missingRequiredStreamerFiles(directory string) []string {
	var missing []string
	for _, name := range requiredStreamerFiles {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() {
			missing = append(missing, name)
		}
	}
	return missing
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
	if !connectionChecksAllowed() {
		c.JSON(http.StatusConflict, gin.H{"error": "connection checks are unavailable while a tune is starting or active"})
		return
	}
	select {
	case connectionCheckSlots <- struct{}{}:
		defer func() { <-connectionCheckSlots }()
	case <-c.Request.Context().Done():
		return
	}
	if !connectionChecksAllowed() {
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

func connectionChecksAllowed() bool {
	return len(activeTunerNumbers()) == 0 && !tunesPending()
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
	if !connectionChecksAllowed() {
		return connectionCheckResult{State: "skipped", Message: "Stopped because a tune started"}
	}
	_, _ = boundedCommand(ctx, 2500*time.Millisecond, "adb", "connect", address)
	state, err := boundedCommand(ctx, 2*time.Second, "adb", "-s", address, "get-state")
	state = strings.TrimSpace(state)
	if err != nil || state != "device" {
		if strings.Contains(strings.ToLower(state), "unauthorized") {
			return connectionCheckResult{State: "error", Message: "ADB is not authorized on the device"}
		}
		if strings.Contains(strings.ToLower(state), "offline") {
			return connectionCheckResult{State: "error", Message: "ADB reports the device is offline"}
		}
		return connectionCheckResult{State: "error", Message: "ADB is not connected and authorized"}
	}
	if !connectionChecksAllowed() {
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
	config := "/root/.android/.pyatv.conf"
	if info, err := os.Stat(config); err != nil || !info.Mode().IsRegular() {
		return connectionCheckResult{State: "error", Message: "Apple TV pairing file is missing"}
	}
	if _, err := exec.LookPath("atvremote"); err != nil {
		return connectionCheckResult{State: "error", Message: "pyatv is not installed"}
	}
	if !connectionChecksAllowed() {
		return connectionCheckResult{State: "skipped", Message: "Stopped because a tune started"}
	}
	if _, err := boundedCommand(ctx, 4*time.Second, "atvremote", "--storage-filename", config, "-s", target, "device_info"); err != nil {
		return connectionCheckResult{State: "error", Message: "Apple TV is not reachable or paired"}
	}
	return connectionCheckResult{State: "ready", Message: "Apple TV is reachable and paired"}
}

func checkEncoderConnection(ctx context.Context, rawURL string) connectionCheckResult {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return connectionCheckResult{State: "empty", Message: "Enter an encoder URL"}
	}
	address, err := encoderNetworkAddress(rawURL)
	if err != nil {
		return connectionCheckResult{State: "error", Message: "Invalid encoder URL"}
	}
	if err := dialConnection(ctx, address); err != nil {
		return connectionCheckResult{State: "error", Message: "Encoder host and port are unreachable"}
	}
	return connectionCheckResult{State: "ready", Message: "Encoder host and port are reachable"}
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
		return "", fmt.Errorf("invalid URL")
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("unsupported URL scheme")
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

func configResponse() gin.H {
	envEngineMu.RLock()
	s := normalizeSettings(envSettings)
	locked := copyBoolMap(envLocked)
	dockerManaged := envDockerManaged
	envEngineMu.RUnlock()
	persistent, dir := configDirPersistent()
	warning := ""
	if !persistent {
		warning = fmt.Sprintf("%s is not persistent. Add ${HOST_DIR}/ah4c/config:/opt/config to your compose volumes before relying on saved settings.", dir)
	}
	catalog := make([]gin.H, 0, len(varCatalog))
	for _, spec := range varCatalog {
		value, source := displayedValue(spec, s, locked)
		item := gin.H{
			"key": spec.Key, "label": spec.Label, "desc": spec.Desc, "placeholder": spec.Placeholder,
			"type": spec.Type, "applies": spec.Applies, "enum": spec.Enum, "scriptVaries": spec.ScriptVaries,
			"secret": spec.Secret, "value": value, "source": source, "locked": locked[spec.Key],
		}
		if spec.Secret {
			item["isSet"] = value != ""
			if value != "" {
				item["value"] = secretMask
			}
		}
		catalog = append(catalog, item)
	}
	host := make([]gin.H, 0, len(hostCatalog))
	for _, spec := range hostCatalog {
		host = append(host, gin.H{"key": spec.Key, "label": spec.Label, "value": os.Getenv(spec.Key)})
	}
	extraLocked := map[string]bool{}
	for key := range s.Extra {
		extraLocked[key] = locked[key]
	}
	return gin.H{
		"persistent": persistent, "persistWarning": warning, "dockerManaged": dockerManaged,
		"restartNeeded": configRestartNeeded, "restartRequired": restartRequiredKeys(), "wizard": !settingsFileExists(), "catalog": catalog,
		"tuners": tunerResponse(s, locked), "extra": s.Extra, "extraLocked": extraLocked, "host": host,
	}
}

func restartRequiredKeys() []string {
	result := make([]string, 0, len(configRestartKeys))
	for key := range configRestartKeys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func displayedValue(spec VarSpec, s Settings, locked map[string]bool) (string, string) {
	if locked[spec.Key] {
		return os.Getenv(spec.Key), "env"
	}
	if value, ok := s.Vars[spec.Key]; ok {
		return value, "config"
	}
	if value := os.Getenv(spec.Key); value != "" {
		if value == defaultValues[spec.Key] {
			return value, "default"
		}
		return value, "env"
	}
	return defaultValues[spec.Key], "default"
}

func tunerResponse(s Settings, locked map[string]bool) gin.H {
	allLocked := locked["NUMBER_TUNERS"]
	list := s.Tuners
	if allLocked || (len(list) == 0 && currentEnvTuners() > 0) {
		list = tunersFromEnvironment()
	}
	items := make([]gin.H, 0, len(list))
	for i, tuner := range list {
		n := strconv.Itoa(i + 1)
		values := map[string]string{
			"tunerIP": tuner.TunerIP, "encoderURL": tuner.EncoderURL, "cmd": tuner.CMD, "teecmd": tuner.TEECMD,
		}
		fieldLocks := map[string]bool{
			"tunerIP":    allLocked || locked["TUNER"+n+"_IP"],
			"encoderURL": allLocked || locked["ENCODER"+n+"_URL"],
			"cmd":        allLocked || locked["CMD"+n],
			"teecmd":     allLocked || locked["TEECMD"+n],
		}
		if fieldLocks["tunerIP"] {
			values["tunerIP"] = os.Getenv("TUNER" + n + "_IP")
		}
		if fieldLocks["encoderURL"] {
			values["encoderURL"] = os.Getenv("ENCODER" + n + "_URL")
		}
		if fieldLocks["cmd"] {
			values["cmd"] = os.Getenv("CMD" + n)
		}
		if fieldLocks["teecmd"] {
			values["teecmd"] = os.Getenv("TEECMD" + n)
		}
		items = append(items, gin.H{"tunerIP": values["tunerIP"], "encoderURL": values["encoderURL"], "cmd": values["cmd"], "teecmd": values["teecmd"], "locked": fieldLocks})
	}
	return gin.H{"locked": allLocked, "list": items}
}

func currentEnvTuners() int {
	n, _ := strconv.Atoi(os.Getenv("NUMBER_TUNERS"))
	if n < 0 {
		return 0
	}
	return n
}

func tunersFromEnvironment() []TunerSpec {
	n := currentEnvTuners()
	list := make([]TunerSpec, 0, n)
	for i := 1; i <= n; i++ {
		suffix := strconv.Itoa(i)
		list = append(list, TunerSpec{
			TunerIP: os.Getenv("TUNER" + suffix + "_IP"), EncoderURL: os.Getenv("ENCODER" + suffix + "_URL"),
			CMD: os.Getenv("CMD" + suffix), TEECMD: os.Getenv("TEECMD" + suffix),
		})
	}
	return list
}

func specByKey() map[string]VarSpec {
	result := make(map[string]VarSpec, len(varCatalog))
	for _, spec := range varCatalog {
		result[spec.Key] = spec
	}
	return result
}

func copyBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func lockedRequestKeys(request configSaveRequest, locked map[string]bool) []string {
	var rejected []string
	for key := range request.Vars {
		if locked[key] {
			rejected = append(rejected, key)
		}
	}
	for key := range request.Extra {
		if locked[key] {
			rejected = append(rejected, key)
		}
	}
	if request.Tuners != nil && locked["NUMBER_TUNERS"] {
		rejected = append(rejected, "NUMBER_TUNERS")
	} else if request.Tuners != nil {
		for i, tuner := range *request.Tuners {
			n := strconv.Itoa(i + 1)
			for key, value := range map[string]string{
				"TUNER" + n + "_IP": tuner.TunerIP, "ENCODER" + n + "_URL": tuner.EncoderURL,
				"CMD" + n: tuner.CMD, "TEECMD" + n: tuner.TEECMD,
			} {
				if locked[key] && value != os.Getenv(key) {
					rejected = append(rejected, key)
				}
			}
		}
	}
	sort.Strings(rejected)
	return rejected
}

func restoreLockedTunerSettings(next *Settings, old Settings, locked map[string]bool) {
	for i := range next.Tuners {
		if i >= len(old.Tuners) {
			continue
		}
		n := strconv.Itoa(i + 1)
		if locked["TUNER"+n+"_IP"] {
			next.Tuners[i].TunerIP = old.Tuners[i].TunerIP
		}
		if locked["ENCODER"+n+"_URL"] {
			next.Tuners[i].EncoderURL = old.Tuners[i].EncoderURL
		}
		if locked["CMD"+n] {
			next.Tuners[i].CMD = old.Tuners[i].CMD
		}
		if locked["TEECMD"+n] {
			next.Tuners[i].TEECMD = old.Tuners[i].TEECMD
		}
	}
}

func validatedSettingsRequest(request configSaveRequest, old Settings) (Settings, error) {
	next := emptySettings()
	next.Tuners = old.Tuners
	if request.Tuners != nil {
		next.Tuners = append([]TunerSpec(nil), (*request.Tuners)...)
	}
	specs := specByKey()
	for key, value := range request.Vars {
		spec, ok := specs[key]
		if !ok {
			return Settings{}, fmt.Errorf("unknown setting %s", key)
		}
		if spec.Secret && value == secretMask {
			if oldValue, exists := old.Vars[key]; exists {
				next.Vars[key] = oldValue
			}
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if err := validateCatalogValue(spec, value); err != nil {
			return Settings{}, fmt.Errorf("%s: %v", key, err)
		}
		if spec.Type == varBool {
			value = strings.ToLower(value)
		}
		if _, existed := old.Vars[key]; !existed && defaultValues[key] == value {
			continue
		}
		next.Vars[key] = value
	}
	for key, value := range request.Extra {
		if !envKeyPattern.MatchString(key) {
			return Settings{}, fmt.Errorf("invalid additional variable name %q", key)
		}
		if _, collision := specs[key]; collision || tunerKeyPattern.MatchString(key) {
			return Settings{}, fmt.Errorf("additional variable %s collides with a managed setting", key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return Settings{}, fmt.Errorf("additional variable %s contains a newline", key)
		}
		if value != "" {
			next.Extra[key] = value
		}
	}
	return next, nil
}

func validateCatalogValue(spec VarSpec, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("multiline values are not allowed")
	}
	if spec.Key == "STREAMER_APP" && value != "" && !validStreamerSelection(value) {
		return fmt.Errorf("must use scripts/device/app")
	}
	if spec.Key == "CHANNELS_M3U" && value != "" {
		if _, err := validM3UFile(value); err != nil {
			return err
		}
	}
	switch spec.Type {
	case varBool:
		if !strings.EqualFold(value, "true") && !strings.EqualFold(value, "false") {
			return fmt.Errorf("must be true or false")
		}
	case varDuration:
		if _, err := parseHoldDuration(value); err != nil {
			return err
		}
	case varSeconds, varInt:
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("must be an integer")
		}
	case varEnum:
		if len(spec.Enum) > 0 {
			matched := false
			for _, candidate := range spec.Enum {
				matched = matched || strings.EqualFold(value, candidate)
			}
			if !matched {
				return fmt.Errorf("must be one of %s", strings.Join(spec.Enum, ", "))
			}
		}
	}
	return nil
}

func changedSettings(old, next Settings) []string {
	var changed []string
	keys := map[string]bool{}
	for key := range old.Vars {
		keys[key] = true
	}
	for key := range next.Vars {
		keys[key] = true
	}
	for key := range keys {
		if old.Vars[key] != next.Vars[key] {
			changed = append(changed, key)
		}
	}
	keys = map[string]bool{}
	for key := range old.Extra {
		keys[key] = true
	}
	for key := range next.Extra {
		keys[key] = true
	}
	for key := range keys {
		if old.Extra[key] != next.Extra[key] {
			changed = append(changed, "extra:"+key)
		}
	}
	if tunerSettingsChanged(old.Tuners, next.Tuners) {
		changed = append(changed, "tuners")
	}
	sort.Strings(changed)
	return changed
}

func tunerSettingsChanged(a, b []TunerSpec) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

func activeTunerNumbers() []int {
	tunerLock.Lock()
	defer tunerLock.Unlock()
	var active []int
	for i := range tuners {
		if tuners[i].active {
			active = append(active, i+1)
		}
	}
	return active
}

func discoverStreamers() []string {
	seen := map[string]bool{}
	root := "scripts"
	devices, err := os.ReadDir(root)
	if err == nil {
		for _, device := range devices {
			if !device.IsDir() {
				continue
			}
			providers, err := os.ReadDir(filepath.Join(root, device.Name()))
			if err != nil {
				continue
			}
			for _, provider := range providers {
				if !provider.IsDir() {
					continue
				}
				if scriptPackageComplete(filepath.Join(root, device.Name(), provider.Name())) {
					seen[filepath.ToSlash(filepath.Join("scripts", device.Name(), provider.Name()))] = true
				}
			}
		}
	}
	for _, value := range loadStreamerCache() {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

type githubTreeResponse struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	} `json:"tree"`
}

func queryUpstreamStreamers() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/sullrich/ah4c/git/trees/main?recursive=1", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ah4c-settings")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not query sullrich/ah4c: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sullrich/ah4c returned %s", resp.Status)
	}
	var tree githubTreeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return nil, fmt.Errorf("could not check GitHub for available scripts: %w", err)
	}
	packages := map[string]map[string]bool{}
	for _, entry := range tree.Tree {
		parts := strings.Split(filepath.ToSlash(entry.Path), "/")
		if entry.Type != "blob" || len(parts) != 4 || parts[0] != "scripts" {
			continue
		}
		name := parts[3]
		for _, required := range requiredStreamerFiles {
			if name == required {
				path := strings.Join(parts[:3], "/")
				if packages[path] == nil {
					packages[path] = map[string]bool{}
				}
				packages[path][name] = true
			}
		}
	}
	result := make([]string, 0, len(packages))
	for path, files := range packages {
		complete := true
		for _, required := range requiredStreamerFiles {
			complete = complete && files[required]
		}
		if complete {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	if err := saveStreamerCache(result); err != nil {
		logger("[CONFIG] could not save the available script list: %v", err)
	}
	return result, nil
}

func streamerCachePath() string {
	return filepath.Join(filepath.Dir(settingsFilePath()), "streamers.json")
}

func loadStreamerCache() []string {
	b, err := os.ReadFile(streamerCachePath())
	if err != nil {
		return nil
	}
	var result []string
	if json.Unmarshal(b, &result) != nil {
		return nil
	}
	return result
}

func saveStreamerCache(streamers []string) error {
	path := streamerCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(streamers, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mergeStreamers(groups ...[]string) []string {
	seen := map[string]bool{}
	for _, group := range groups {
		for _, value := range group {
			seen[value] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func maskedEnviron() []string {
	secret := map[string]bool{}
	for _, spec := range varCatalog {
		secret[spec.Key] = spec.Secret
	}
	env := os.Environ()
	for i, item := range env {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 && secret[parts[0]] && parts[1] != "" {
			env[i] = parts[0] + "=" + secretMask
		}
	}
	sort.Strings(env)
	return env
}

func maskedEnvValue(key string) string {
	if os.Getenv(key) == "" {
		return ""
	}
	return secretMask
}
