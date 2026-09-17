package main

import (
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
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
	configAPIMu         sync.Mutex
	configRestartNeeded bool
	configRestartKeys   = map[string]bool{}
)

var defaultValues = map[string]string{
	"PYATV":                    "false",
	"ALERT_EMAIL_USE_SENDMAIL": "false",
	"CREATE_M3US":              "false",
	"UPDATE_SCRIPTS":           "false",
	"UPDATE_M3US":              "true",
	"SPEED_MODE":               "false",
	"NULL_FRAME_INSERTION":     "false",
	"PLAYBACK_DETECTION":       "false",
	"PLAYBACK_STATIC_TIMEOUT":  "2",
	"ENCODER_CODEC":            "h264",
	"HEARTBEAT_INTERVAL":       "180",
}

type configSaveRequest struct {
	Vars   map[string]string `json:"vars"`
	Tuners *[]TunerSpec      `json:"tuners"`
	Extra  map[string]string `json:"extra"`
}

func registerConfigRoutes(r *gin.Engine) {
	r.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/" && setupWizardNeeded() {
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
	if !configComplete(os.Getenv) {
		logger("[CONFIG] STREAMER_APP is invalid; correct it in Settings or remove it")
	}
	r.GET("/settings", func(c *gin.Context) {
		c.HTML(http.StatusOK, "settings.html", nil)
	})
	r.GET("/api/config", getConfigHandler)
	r.PUT("/api/config", putConfigHandler)
	r.POST("/api/config/restart", restartConfigHandler)
	registerScriptConfigRoutes(r)
	registerConnectionConfigRoutes(r)
	registerPrerollConfigRoutes(r)
	registerM3UIntegrationRoutes(r)
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
	if missing := missingRequiredPersistentMounts(); len(missing) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":                   "Setup cannot continue until every required storage folder is added to the container.",
			"missingPersistentMounts": persistentMountResponse(missing),
		})
		return
	}
	configAPIMu.Lock()
	defer configAPIMu.Unlock()

	envEngineMu.RLock()
	old := copySettings(envSettings)
	locked := copyBoolMap(envLocked)
	envEngineMu.RUnlock()

	rejected := lockedRequestKeys(request, old, locked)
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
	active, blocked := restartTuneConflict()
	if blocked && !force {
		c.JSON(http.StatusConflict, gin.H{"error": "a tune is starting or active", "tuners": active, "pending": tunesPending()})
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
			active, blocked := restartTuneConflict()
			if blocked {
				logger("[CONFIG] restart canceled because a tune started or became active: %v", active)
				return
			}
		}
		os.Exit(0)
	}()
}

func validScriptPathPart(value string) bool {
	if value == "" || strings.HasPrefix(value, ".") {
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

func canonicalStreamerSelection(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	for strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	return strings.TrimRight(value, "/")
}

func validStreamerSelection(value string) bool {
	parts := strings.Split(canonicalStreamerSelection(value), "/")
	if (len(parts) != 2 && len(parts) != 3) || parts[0] != "scripts" {
		return false
	}
	for _, part := range parts[1:] {
		if !validScriptPathPart(part) {
			return false
		}
	}
	return true
}

func scriptPackageComplete(directory string) bool {
	for _, name := range requiredStreamerFiles {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func configResponse() gin.H {
	envEngineMu.RLock()
	s := normalizeSettings(envSettings)
	locked := copyBoolMap(envLocked)
	dockerManaged := envDockerManaged
	envEngineMu.RUnlock()
	missingMounts := missingRequiredPersistentMounts()
	persistent := len(missingMounts) == 0
	warning := ""
	if !persistent {
		warning = "Setup cannot continue until every required storage folder is added to the container."
	}
	catalog := make([]gin.H, 0, len(varCatalog))
	for _, spec := range varCatalog {
		value, source := displayedValue(spec, s, locked)
		item := gin.H{
			"key": spec.Key, "label": spec.Label, "desc": spec.Desc, "placeholder": spec.Placeholder,
			"type": spec.Type, "applies": spec.Applies, "enum": spec.Enum, "scriptVaries": spec.ScriptVaries,
			"secret": spec.Secret, "value": value, "defaultValue": defaultValues[spec.Key], "source": source, "locked": locked[spec.Key],
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
		"missingPersistentMounts": persistentMountResponse(missingMounts),
		"restartNeeded":           configRestartNeeded, "restartRequired": restartRequiredKeys(), "wizard": setupWizardNeeded(), "wizardDisabled": setupWizardDisabled(os.Getenv), "catalog": catalog,
		"tuners": tunerResponse(s, locked), "extra": s.Extra, "extraLocked": extraLocked, "host": host,
	}
}

func persistentMountResponse(requirements []persistentMountRequirement) []gin.H {
	response := make([]gin.H, 0, len(requirements))
	for _, requirement := range requirements {
		response = append(response, gin.H{"label": requirement.Label, "path": requirement.ContainerPath})
	}
	return response
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
		items = append(items, gin.H{"number": i, "tunerIP": values["tunerIP"], "encoderURL": values["encoderURL"], "cmd": values["cmd"], "teecmd": values["teecmd"], "locked": fieldLocks})
	}
	return gin.H{"locked": allLocked, "countLocked": allLocked, "slotCount": len(list), "topologyLocked": tunerTopologyLocked(locked), "list": items}
}

func tunerTopologyLocked(locked map[string]bool) bool {
	for key, isLocked := range locked {
		if isLocked && tunerKeyPattern.MatchString(key) {
			return true
		}
	}
	return false
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

func lockedRequestKeys(request configSaveRequest, old Settings, locked map[string]bool) []string {
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
	if request.Tuners != nil {
		if locked["NUMBER_TUNERS"] {
			rejected = append(rejected, "NUMBER_TUNERS")
		} else if tunerCountChangeMovesLockedPosition(len(old.Tuners), len(*request.Tuners), locked) {
			rejected = append(rejected, "tuner count")
		}
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

func tunerCountChangeMovesLockedPosition(oldCount, newCount int, locked map[string]bool) bool {
	if oldCount == newCount {
		return false
	}
	lastLocked := -1
	limit := oldCount
	if newCount > limit {
		limit = newCount
	}
	for index := 0; index < limit; index++ {
		n := strconv.Itoa(index + 1)
		if locked["TUNER"+n+"_IP"] || locked["ENCODER"+n+"_URL"] || locked["CMD"+n] || locked["TEECMD"+n] {
			lastLocked = index
		}
	}
	return newCount < oldCount && newCount <= lastLocked
}

func restoreLockedTunerSettings(next *Settings, old Settings, locked map[string]bool) {
	for i := range next.Tuners {
		n := strconv.Itoa(i + 1)
		if locked["TUNER"+n+"_IP"] {
			next.Tuners[i].TunerIP = ""
			if i < len(old.Tuners) {
				next.Tuners[i].TunerIP = old.Tuners[i].TunerIP
			}
		}
		if locked["ENCODER"+n+"_URL"] {
			next.Tuners[i].EncoderURL = ""
			if i < len(old.Tuners) {
				next.Tuners[i].EncoderURL = old.Tuners[i].EncoderURL
			}
		}
		if locked["CMD"+n] {
			next.Tuners[i].CMD = ""
			if i < len(old.Tuners) {
				next.Tuners[i].CMD = old.Tuners[i].CMD
			}
		}
		if locked["TEECMD"+n] {
			next.Tuners[i].TEECMD = ""
			if i < len(old.Tuners) {
				next.Tuners[i].TEECMD = old.Tuners[i].TEECMD
			}
		}
	}
}

func validatedSettingsRequest(request configSaveRequest, old Settings) (Settings, error) {
	next := emptySettings()
	next.Tuners = old.Tuners
	for key, value := range old.Vars {
		if _, submitted := request.Vars[key]; !submitted {
			next.Vars[key] = value
		}
	}
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
		if key == "STREAMER_APP" {
			value = canonicalStreamerSelection(value)
		}
		if key == "IPADDRESS" || key == "CHANNELSIP" {
			defaultPort := "7654"
			if key == "CHANNELSIP" {
				defaultPort = "8089"
			}
			if value != "" {
				var err error
				value, err = serverBaseURL(value, defaultPort)
				if err != nil {
					return Settings{}, fmt.Errorf("%s: %v", key, err)
				}
			}
		}
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
		if environmentOnlyKeys[key] {
			return Settings{}, fmt.Errorf("additional variable %s must be set in the container environment", key)
		}
		if _, collision := specs[key]; collision || tunerKeyPattern.MatchString(key) {
			return Settings{}, fmt.Errorf("additional variable %s collides with a managed setting", key)
		}
		envEngineMu.RLock()
		ownedByEnvironment := envOwned[key]
		envEngineMu.RUnlock()
		if ownedByEnvironment {
			return Settings{}, fmt.Errorf("additional variable %s is owned by the container environment", key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return Settings{}, fmt.Errorf("additional variable %s contains a newline", key)
		}
		if value != "" {
			next.Extra[key] = value
		}
	}
	for index := range next.Tuners {
		tuner := &next.Tuners[index]
		tuner.TunerIP = strings.TrimSpace(tuner.TunerIP)
		tuner.EncoderURL = strings.TrimSpace(tuner.EncoderURL)
		tuner.CMD = strings.TrimSpace(tuner.CMD)
		tuner.TEECMD = strings.TrimSpace(tuner.TEECMD)
		for field, value := range map[string]string{"Tuner IP": tuner.TunerIP, "Encoder URL": tuner.EncoderURL, "CMD": tuner.CMD, "TEECMD": tuner.TEECMD} {
			if strings.ContainsAny(value, "\r\n") {
				return Settings{}, fmt.Errorf("tuner %d %s must be one line", index, field)
			}
		}
		if tuner.TunerIP != "" {
			if err := validateBareHostPort(tuner.TunerIP); err != nil {
				return Settings{}, fmt.Errorf("tuner %d Tuner IP: %v", index, err)
			}
		}
		if tuner.EncoderURL != "" {
			if err := validateHTTPURL(tuner.EncoderURL); err != nil {
				return Settings{}, fmt.Errorf("tuner %d Encoder URL: %v", index, err)
			}
		}
	}
	return next, nil
}

func validateCatalogValue(spec VarSpec, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("multiline values are not allowed")
	}
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if spec.Key == "STREAMER_APP" && value != "" && !validStreamerSelection(value) {
		return fmt.Errorf("must use scripts/package or scripts/device/app")
	}
	switch spec.Key {
	case "IPADDRESS":
		if _, err := serverBaseURL(value, "7654"); err != nil {
			return fmt.Errorf("enter an ah4c network address another computer can use, such as 192.168.1.50:7654")
		}
	case "CHANNELSIP":
		if _, err := serverBaseURL(value, "8089"); err != nil {
			return fmt.Errorf("open Channels DVR and copy its address here, such as 192.168.1.20:8089")
		}
	case "ALERT_SMTP_SERVER":
		if err := validateBareHostPort(value); err != nil {
			return fmt.Errorf("enter an SMTP hostname or IP address with an optional port")
		}
	case "ALERT_AUTH_SERVER":
		if err := validateBareHostPort(value); err != nil {
			return fmt.Errorf("enter a hostname or IP address with an optional port")
		}
	case "ALERT_EMAIL_FROM", "ALERT_EMAIL_TO":
		if err := validateEmailAddress(value); err != nil {
			return err
		}
	case "FASTCHANNELS_URL", "ALERT_WEBHOOK_URL":
		if err := validateHTTPURL(value); err != nil {
			return err
		}
	}
	if spec.ScriptVaries && spec.Key != "FASTCHANNELS_URL" {
		return nil
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

func serverBaseURL(value, defaultPort string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("server address is empty")
	}
	if !strings.Contains(value, "://") {
		if net.ParseIP(value) != nil && strings.Contains(value, ":") {
			value = "http://[" + value + "]"
		} else {
			value = "http://" + value
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid server address")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("server address must not include a path")
	}
	if err := validateURLPort(parsed); err != nil {
		return "", err
	}
	port := parsed.Port()
	if port == "" {
		port = defaultPort
	}
	return parsed.Scheme + "://" + net.JoinHostPort(parsed.Hostname(), port), nil
}

func m3uTemplateAddress(value string, templateAddsPort bool) (string, error) {
	base, err := serverBaseURL(value, "7654")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid ah4c address")
	}
	if !templateAddsPort {
		return parsed.Host, nil
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host, nil
}

func validateBareHostPort(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "://") || strings.ContainsAny(value, "/?#") {
		return fmt.Errorf("invalid host")
	}
	base, err := serverBaseURL(value, "1")
	if err != nil {
		return err
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid host")
	}
	return nil
}

func validateHTTPURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("enter a web address that starts with http:// or https://")
	}
	if err := validateURLPort(parsed); err != nil {
		return err
	}
	return nil
}

func validateEmailAddress(value string) error {
	address, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil || !strings.Contains(address.Address, "@") {
		return fmt.Errorf("must be one e-mail address, such as name@example.com")
	}
	return nil
}

func validateURLPort(parsed *url.URL) error {
	port := parsed.Port()
	if port == "" {
		return nil
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
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
			active = append(active, i)
		}
	}
	return active
}

func configOperationsAllowed() bool {
	return len(activeTunerNumbers()) == 0 && !tunesPending()
}

func restartTuneConflict() ([]int, bool) {
	active := activeTunerNumbers()
	return active, len(active) > 0 || tunesPending()
}

func setupWizardNeeded() bool {
	return setupWizardNeededFor(os.Getenv)
}

func setupWizardNeededFor(lookup func(string) string) bool {
	return !setupWizardDisabled(lookup) && !environmentSetupReady(lookup)
}

func setupWizardDisabled(lookup func(string) string) bool {
	return strings.EqualFold(strings.TrimSpace(lookup("SETUP_WIZARD")), "false")
}

func environmentSetupReady(lookup func(string) string) bool {
	proxy := strings.TrimSpace(lookup("IPADDRESS"))
	channels := strings.TrimSpace(lookup("CHANNELSIP"))
	if proxy == "" || channels == "" {
		return false
	}
	if _, err := serverBaseURL(proxy, "7654"); err != nil {
		return false
	}
	if _, err := serverBaseURL(channels, "8089"); err != nil {
		return false
	}
	return true
}

func discoverLocalStreamers() []string {
	return mergeStreamers(discoverLocalStreamersAt("scripts"), discoverLocalStreamersAt("/tmp/scripts"))
}

func discoverLocalStreamersAt(root string) []string {
	seen := map[string]bool{}
	devices, err := os.ReadDir(root)
	if err == nil {
		for _, device := range devices {
			if !device.IsDir() || !validScriptPathPart(device.Name()) {
				continue
			}
			devicePath := filepath.Join(root, device.Name())
			if scriptPackageComplete(devicePath) {
				seen[filepath.ToSlash(filepath.Join("scripts", device.Name()))] = true
			}
			providers, err := os.ReadDir(devicePath)
			if err != nil {
				continue
			}
			for _, provider := range providers {
				if !provider.IsDir() || !validScriptPathPart(provider.Name()) {
					continue
				}
				if scriptPackageComplete(filepath.Join(root, device.Name(), provider.Name())) {
					seen[filepath.ToSlash(filepath.Join("scripts", device.Name(), provider.Name()))] = true
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func mergeStreamers(groups ...[]string) []string {
	seen := map[string]bool{}
	for _, group := range groups {
		for _, value := range group {
			value = canonicalStreamerSelection(value)
			if validStreamerSelection(value) {
				seen[value] = true
			}
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
