package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

type varType string
type applyClass string

const (
	varBool     varType = "bool"
	varDuration varType = "duration"
	varSeconds  varType = "seconds"
	varInt      varType = "int"
	varEnum     varType = "enum"
	varPassword varType = "password"
	varString   varType = "string"
	varURL      varType = "url"
	varPath     varType = "path"

	applyLive    applyClass = "live"
	applyRestart applyClass = "restart"
	applyHost    applyClass = "host"
)

type VarSpec struct {
	Key, Label, Desc, Placeholder string
	Type                          varType
	Applies                       applyClass
	Secret, ScriptVaries          bool
	Enum                          []string
}

type TunerSpec struct {
	TunerIP    string `json:"tunerIP"`
	EncoderURL string `json:"encoderURL"`
	CMD        string `json:"cmd"`
	TEECMD     string `json:"teecmd"`
}

type Settings struct {
	Version int               `json:"version"`
	Vars    map[string]string `json:"vars"`
	Tuners  []TunerSpec       `json:"tuners"`
	Extra   map[string]string `json:"extra"`
}

var varCatalog = []VarSpec{
	{Key: "IPADDRESS", Label: "This ah4c address", Desc: "The network address other devices use to reach ah4c.", Placeholder: "192.168.1.50:7654", Type: varString, Applies: applyLive},
	{Key: "CHANNELSIP", Label: "Channels DVR address", Desc: "Open Channels DVR in a browser and copy the server address and port from the address bar.", Placeholder: "192.168.1.20:8089", Type: varString, Applies: applyLive},
	{Key: "STREAMER_APP", Label: "Streaming app scripts", Desc: "Choose the scripts ah4c should use to open your streaming app, switch channels, and stop playback. You do not need to read or edit the scripts.", Placeholder: "scripts/my-package", Type: varEnum, Applies: applyRestart},
	{Key: "PYATV", Label: "Use pyatv", Desc: "Use Apple TV tuners through pyatv instead of adb-based tuners. Changing this requires a restart.", Type: varBool, Applies: applyRestart},
	{Key: "FASTCHANNELS_URL", Label: "FastChannels URL", Desc: "Base URL of the FastChannels container used by its ah4c tuning integration.", Placeholder: "http://fastchannels:8000", Type: varURL, Applies: applyLive, ScriptVaries: true},
	{Key: "ALERT_SMTP_SERVER", Label: "SMTP server", Desc: "SMTP server and port used for failure alerts.", Placeholder: "smtp.gmail.com:587", Type: varString, Applies: applyLive},
	{Key: "ALERT_AUTH_SERVER", Label: "SMTP auth server", Desc: "Authentication server used for alert e-mail.", Placeholder: "smtp.gmail.com", Type: varString, Applies: applyLive},
	{Key: "ALERT_EMAIL_FROM", Label: "Alert e-mail from", Desc: "Sender address for ah4c failure alerts.", Placeholder: "ah4c@example.com", Type: varString, Applies: applyLive},
	{Key: "ALERT_EMAIL_PASS", Label: "Alert e-mail password", Desc: "App-specific password used to send alert e-mail.", Type: varPassword, Applies: applyLive, Secret: true},
	{Key: "ALERT_EMAIL_TO", Label: "Alert e-mail to", Desc: "Address that receives ah4c failure alerts.", Placeholder: "you@example.com", Type: varString, Applies: applyLive},
	{Key: "ALERT_EMAIL_USE_SENDMAIL", Label: "Use sendmail", Desc: "Send alert e-mail with the local sendmail command instead of SMTP.", Type: varBool, Applies: applyLive},
	{Key: "ALERT_WEBHOOK_URL", Label: "Alert webhook URL", Desc: "URL to GET when tuning fails; $reason is replaced with the encoded failure message.", Type: varURL, Applies: applyLive},
	{Key: "LIVETV_ATTEMPTS", Label: "Live TV attempts", Desc: "Maximum attempts at finding a channel with Fire TV Live Guide tuning.", Placeholder: "3", Type: varInt, Applies: applyLive, ScriptVaries: true},
	{Key: "CREATE_M3US", Label: "Create device M3Us", Desc: "Create device-specific M3Us for Amazon Prime Premium channels at startup.", Type: varBool, Applies: applyRestart},
	{Key: "UPDATE_SCRIPTS", Label: "Refresh bundled scripts", Desc: "Replace the selected scripts with the copy bundled in the container image when ah4c starts.", Type: varBool, Applies: applyRestart},
	{Key: "UPDATE_M3US", Label: "Update sample M3Us", Desc: "Replace bundled sample M3Us at startup.", Type: varBool, Applies: applyRestart},
	{Key: "USER_SCRIPT", Label: "Custom startup script", Desc: "Path to a custom script run alongside ah4c at container startup.", Type: varPath, Applies: applyRestart},
	{Key: "TZ", Label: "Time zone", Desc: "Local time zone used for logs and scheduled behavior.", Placeholder: "America/New_York", Type: varString, Applies: applyRestart},
	{Key: "SPEED_MODE", Label: "Speed mode", Desc: "Keep supported streaming apps open between tuning cycles.", Type: varBool, Applies: applyLive, ScriptVaries: true},
	{Key: "KEEP_WATCHING", Label: "Keep-watching interval", Desc: "Delay before supported scripts resend a deeplink or keypress to prevent inactivity prompts.", Placeholder: "4h or 240m", Type: varDuration, Applies: applyLive, ScriptVaries: true},
	{Key: "AUTOCROP_CHANNELS", Label: "Autocrop channels", Desc: "Space-separated channel numbers whose four-sided black borders should be cropped by a LinkPi encoder.", Type: varString, Applies: applyLive, ScriptVaries: true},
	{Key: "LINKPI_HOSTNAME", Label: "LinkPi hostname", Desc: "Hostname or IP address of the LinkPi encoder web API.", Type: varString, Applies: applyLive, ScriptVaries: true},
	{Key: "LINKPI_USERNAME", Label: "LinkPi username", Desc: "Username for the LinkPi encoder web API.", Type: varString, Applies: applyLive, ScriptVaries: true},
	{Key: "LINKPI_PASSWORD", Label: "LinkPi password", Desc: "Password for the LinkPi encoder web API.", Type: varPassword, Applies: applyLive, Secret: true, ScriptVaries: true},
	{Key: "NULL_FRAME_INSERTION", Label: "Null frame insertion", Desc: "Fill encoder stalls with MPEG-TS NULL packets so the DVR does not see a zero-byte gap.", Type: varBool, Applies: applyLive},
	{Key: "PLAYBACK_DETECTION", Label: "Playback detection", Desc: "Hold the stream until the device reports audio and a moving picture, then start on a keyframe.", Type: varBool, Applies: applyLive},
	{Key: "PLAYBACK_STATIC_TIMEOUT", Label: "Static-player timeout", Desc: "Seconds the prior player may remain before playback detection falls back to motion alone.", Placeholder: "2", Type: varSeconds, Applies: applyLive},
	{Key: "PLAYBACK_DELAY", Label: "Playback delay", Desc: "Hold each tune before handing the DVR the program; accepts bare seconds or a duration such as 30s, 2m or 1h.", Placeholder: "30s", Type: varDuration, Applies: applyRestart},
	{Key: "ENCODER_CODEC", Label: "Encoder codec", Desc: "Video codec emitted by the encoder; filler and pre-roll are prepared to match it.", Type: varEnum, Applies: applyRestart, Enum: []string{"h264", "h265"}},
	{Key: "HEARTBEAT_INTERVAL", Label: "Heartbeat interval", Desc: "Seconds between supported scripts' keepalive keyevents; 0 disables them.", Placeholder: "180", Type: varSeconds, Applies: applyLive, ScriptVaries: true},
}

var environmentOnlyKeys = map[string]bool{
	"AH4C_ENV_LOCKED":            true,
	"AH4C_ENV_OWNED":             true,
	"ALLOW_DEBUG_VIDEO_PREVIEW":  true,
	"CC_WATCHDOG":                true,
	"CONTAINER_NAME":             true,
	"DOCKER_RUNTIME":             true,
	"DOMAIN":                     true,
	"GPU_DEVICE":                 true,
	"HOST_DIR":                   true,
	"HOST_PORT":                  true,
	"HOSTNAME":                   true,
	"NVIDIA_DRIVER_CAPABILITIES": true,
	"NVIDIA_VISIBLE_DEVICES":     true,
	"PREROLL_FILE":               true,
	"SETUP_WIZARD":               true,
	"TAG":                        true,
}

var hostCatalog = []VarSpec{
	{Key: "TAG", Label: "Image tag", Applies: applyHost},
	{Key: "CONTAINER_NAME", Label: "Container name", Applies: applyHost},
	{Key: "HOSTNAME", Label: "Container hostname", Applies: applyHost},
	{Key: "DOMAIN", Label: "DNS search domain", Applies: applyHost},
	{Key: "DOCKER_RUNTIME", Label: "Docker runtime", Applies: applyHost},
	{Key: "GPU_DEVICE", Label: "GPU device", Applies: applyHost},
	{Key: "HOST_PORT", Label: "Host port", Applies: applyHost},
	{Key: "HOST_DIR", Label: "Host data directory", Applies: applyHost},
	{Key: "PREROLL_FILE", Label: "Pre-roll host path", Applies: applyHost},
	{Key: "NVIDIA_VISIBLE_DEVICES", Label: "NVIDIA devices", Applies: applyHost},
	{Key: "NVIDIA_DRIVER_CAPABILITIES", Label: "NVIDIA driver capabilities", Applies: applyHost},
}

var (
	envKeyPattern        = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	tunerKeyPattern      = regexp.MustCompile(`^(NUMBER_TUNERS|TUNER[0-9]+_IP|ENCODER[0-9]+_URL|CMD[0-9]+|TEECMD[0-9]+)$`)
	envEngineMu          sync.RWMutex
	envSettings          = emptySettings()
	envLocked            = map[string]bool{}
	envDockerManaged     bool
	envOwned             = map[string]bool{}
	settingsPathOverride string
	startupEnvironment   = environMap(os.Environ())
)

func emptySettings() Settings {
	return Settings{Version: 1, Vars: map[string]string{}, Tuners: []TunerSpec{}, Extra: map[string]string{}}
}

func settingsFilePath() string {
	if settingsPathOverride != "" {
		return settingsPathOverride
	}
	if wd, err := os.Getwd(); err == nil && filepath.Clean(wd) == "/opt" {
		return "/opt/config/settings.json"
	}
	return filepath.Join("config", "settings.json")
}

func normalizeSettings(s Settings) Settings {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Vars == nil {
		s.Vars = map[string]string{}
	}
	if s.Tuners == nil {
		s.Tuners = []TunerSpec{}
	}
	if s.Extra == nil {
		s.Extra = map[string]string{}
	}
	return s
}

func copySettings(s Settings) Settings {
	s = normalizeSettings(s)
	copyOf := Settings{
		Version: s.Version,
		Vars:    make(map[string]string, len(s.Vars)),
		Tuners:  append([]TunerSpec(nil), s.Tuners...),
		Extra:   make(map[string]string, len(s.Extra)),
	}
	for key, value := range s.Vars {
		copyOf.Vars[key] = value
	}
	for key, value := range s.Extra {
		copyOf.Extra[key] = value
	}
	return copyOf
}

func loadSettingsWithWarnings() (Settings, []string, error) {
	b, err := os.ReadFile(settingsFilePath())
	if os.IsNotExist(err) {
		return emptySettings(), nil, nil
	}
	if err != nil {
		return emptySettings(), nil, err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return emptySettings(), nil, err
	}
	if s.Version != 0 && s.Version != 1 {
		return emptySettings(), nil, fmt.Errorf("unsupported settings version %d", s.Version)
	}
	s, warnings := sanitizeLoadedSettings(s)
	return s, warnings, nil
}

func sanitizeLoadedSettings(s Settings) (Settings, []string) {
	s = copySettings(s)
	specs := specByKey()
	var warnings []string
	for key, value := range s.Vars {
		if environmentOnlyKeys[key] {
			delete(s.Vars, key)
			warnings = append(warnings, fmt.Sprintf("ignored %s from settings.json because it is environment-only", key))
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			delete(s.Vars, key)
			warnings = append(warnings, fmt.Sprintf("ignored %s because multiline values are not allowed", key))
			continue
		}
		if spec, managed := specs[key]; managed {
			if key == "STREAMER_APP" {
				canonical := canonicalStreamerSelection(value)
				if canonical != value {
					value = canonical
					s.Vars[key] = canonical
					warnings = append(warnings, fmt.Sprintf("normalized STREAMER_APP to %s", canonical))
				}
			}
			if err := validateCatalogValue(spec, value); err != nil {
				delete(s.Vars, key)
				warnings = append(warnings, fmt.Sprintf("ignored invalid %s: %v", key, err))
			}
			continue
		}
		delete(s.Vars, key)
		if envKeyPattern.MatchString(key) && !tunerKeyPattern.MatchString(key) {
			if _, exists := s.Extra[key]; !exists {
				s.Extra[key] = value
				warnings = append(warnings, fmt.Sprintf("moved unknown setting %s to additional variables", key))
				continue
			}
		}
		warnings = append(warnings, fmt.Sprintf("ignored unknown setting %s", key))
	}
	for key, value := range s.Extra {
		if environmentOnlyKeys[key] {
			delete(s.Extra, key)
			warnings = append(warnings, fmt.Sprintf("ignored %s from additional variables because it is environment-only", key))
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			delete(s.Extra, key)
			warnings = append(warnings, fmt.Sprintf("ignored additional variable %s because multiline values are not allowed", key))
			continue
		}
		if spec, managed := specs[key]; managed {
			delete(s.Extra, key)
			if key == "STREAMER_APP" {
				value = canonicalStreamerSelection(value)
			}
			if err := validateCatalogValue(spec, value); err != nil {
				warnings = append(warnings, fmt.Sprintf("ignored invalid %s from additional variables: %v", key, err))
				continue
			}
			if _, exists := s.Vars[key]; !exists {
				s.Vars[key] = value
				warnings = append(warnings, fmt.Sprintf("restored %s from additional variables", key))
			}
			continue
		}
		if !envKeyPattern.MatchString(key) || tunerKeyPattern.MatchString(key) {
			delete(s.Extra, key)
			warnings = append(warnings, fmt.Sprintf("ignored invalid additional variable %s", key))
		}
	}
	for index := range s.Tuners {
		fields := []struct {
			name  string
			value *string
		}{
			{"tunerIP", &s.Tuners[index].TunerIP},
			{"encoderURL", &s.Tuners[index].EncoderURL},
			{"cmd", &s.Tuners[index].CMD},
			{"teecmd", &s.Tuners[index].TEECMD},
		}
		for _, field := range fields {
			if strings.ContainsAny(*field.value, "\r\n") {
				*field.value = ""
				warnings = append(warnings, fmt.Sprintf("cleared tuners[%d].%s because multiline values are not allowed", index, field.name))
			}
		}
	}
	return s, warnings
}

func validateSettingsSchema(s Settings) error {
	catalog := catalogKeySet()
	validValue := func(key, value string) error {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s contains a newline", key)
		}
		return nil
	}
	for key, value := range s.Vars {
		if !catalog[key] {
			return fmt.Errorf("vars contains unknown key %s", key)
		}
		if err := validValue(key, value); err != nil {
			return err
		}
	}
	for key, value := range s.Extra {
		if !envKeyPattern.MatchString(key) || catalog[key] || tunerKeyPattern.MatchString(key) || environmentOnlyKeys[key] {
			return fmt.Errorf("extra contains invalid or managed key %s", key)
		}
		if err := validValue(key, value); err != nil {
			return err
		}
	}
	for i, tuner := range s.Tuners {
		for key, value := range map[string]string{
			"tunerIP": tuner.TunerIP, "encoderURL": tuner.EncoderURL, "cmd": tuner.CMD, "teecmd": tuner.TEECMD,
		} {
			if err := validValue(fmt.Sprintf("tuners[%d].%s", i, key), value); err != nil {
				return err
			}
		}
	}
	return nil
}

func settingsFileExists() bool {
	_, err := os.Stat(settingsFilePath())
	return err == nil
}

func saveSettings(s Settings) error {
	s = normalizeSettings(s)
	if err := validateSettingsSchema(s); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := settingsFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func envValueUsable(val string, present bool) bool {
	return present && val != ""
}

func environMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, item := range environ {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func nonEmptyEnvironmentKeys(values map[string]string) map[string]bool {
	owned := make(map[string]bool, len(values))
	for key, value := range values {
		if value != "" {
			owned[key] = true
		}
	}
	return owned
}

func catalogKeySet() map[string]bool {
	keys := make(map[string]bool, len(varCatalog))
	for _, spec := range varCatalog {
		keys[spec.Key] = true
	}
	return keys
}

func computeLockSet(environ []string, s Settings) map[string]bool {
	candidates := catalogKeySet()
	for key := range s.Vars {
		candidates[key] = true
	}
	for key := range s.Extra {
		candidates[key] = true
	}
	values := environMap(environ)
	locked := map[string]bool{}
	for key, val := range values {
		if (candidates[key] || tunerKeyPattern.MatchString(key)) && envValueUsable(val, true) {
			locked[key] = true
		}
	}
	return locked
}

func synthesizeTunerVars(tuners []TunerSpec) map[string]string {
	vars := map[string]string{"NUMBER_TUNERS": strconv.Itoa(len(tuners))}
	for i, tuner := range tuners {
		n := strconv.Itoa(i + 1)
		vars["TUNER"+n+"_IP"] = tuner.TunerIP
		vars["ENCODER"+n+"_URL"] = tuner.EncoderURL
		vars["CMD"+n] = tuner.CMD
		vars["TEECMD"+n] = tuner.TEECMD
	}
	return vars
}

func materializePlan(environ []string, s Settings) (map[string]string, map[string]bool) {
	s = normalizeSettings(s)
	values := environMap(environ)
	locked := computeLockSet(environ, s)
	sets := map[string]string{}
	consider := func(key, val string, allowEmpty bool) {
		if val == "" && !allowEmpty {
			return
		}
		current, present := values[key]
		if !envValueUsable(current, present) {
			sets[key] = val
		}
	}
	for key, val := range s.Vars {
		consider(key, val, false)
	}
	for key, val := range s.Extra {
		consider(key, val, false)
	}
	if !locked["NUMBER_TUNERS"] {
		for key, val := range synthesizeTunerVars(s.Tuners) {
			consider(key, val, key != "NUMBER_TUNERS")
		}
	}
	return sets, locked
}

func materializeBootstrapPlan(environ []string, s Settings, settingsPresent bool, legacy map[string]string) (map[string]string, map[string]bool, map[string]string) {
	sets, locked := materializePlan(environ, s)
	sources := map[string]string{}
	for key := range sets {
		sources[key] = "settings.json"
	}
	if !settingsPresent && sets["NUMBER_TUNERS"] == "0" {
		sources["NUMBER_TUNERS"] = "built-in default"
	}
	if !settingsPresent && envValueUsable(legacy["NUMBER_TUNERS"], legacy["NUMBER_TUNERS"] != "") {
		for key := range sets {
			if tunerKeyPattern.MatchString(key) {
				delete(sets, key)
				delete(sources, key)
			}
		}
	}
	values := environMap(environ)
	if raw, present := values["STREAMER_APP"]; envValueUsable(raw, present) {
		canonical := canonicalStreamerSelection(raw)
		if canonical != raw && validStreamerSelection(canonical) {
			sets["STREAMER_APP"] = canonical
			sources["STREAMER_APP"] = "normalized environment"
		}
	}
	consider := func(key, value, source string) {
		if !envKeyPattern.MatchString(key) || value == "" || strings.ContainsAny(value, "\r\n") {
			return
		}
		if current, present := values[key]; envValueUsable(current, present) {
			return
		}
		if _, exists := sets[key]; exists {
			return
		}
		sets[key] = value
		sources[key] = source
	}
	catalog := catalogKeySet()
	for key, value := range legacy {
		if settingsPresent && (catalog[key] || tunerKeyPattern.MatchString(key)) {
			continue
		}
		consider(key, value, "./env")
	}
	for key, value := range defaultValues {
		consider(key, value, "built-in default")
	}
	if _, exists := sets["NUMBER_TUNERS"]; !exists {
		if current, present := values["NUMBER_TUNERS"]; !envValueUsable(current, present) {
			sets["NUMBER_TUNERS"] = "0"
			sources["NUMBER_TUNERS"] = "built-in default"
		}
	}
	return sets, locked, sources
}

func readLegacyEnv() map[string]string {
	legacy, err := godotenv.Read("env")
	if err != nil {
		return map[string]string{}
	}
	return legacy
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func shellExports(sets map[string]string, locked, owned map[string]bool) string {
	for key, value := range sets {
		if !envKeyPattern.MatchString(key) || strings.ContainsAny(value, "\r\n") {
			return "export AH4C_ENV_LOCKED=''\nexport AH4C_ENV_OWNED=''\n# invalid settings key or multiline value rejected\n"
		}
	}
	keys := make([]string, 0, len(sets))
	for key := range sets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lockKeys := make([]string, 0, len(locked))
	for key, isLocked := range locked {
		if isLocked {
			lockKeys = append(lockKeys, key)
		}
	}
	sort.Strings(lockKeys)
	ownedKeys := make([]string, 0, len(owned))
	for key, isOwned := range owned {
		if isOwned {
			ownedKeys = append(ownedKeys, key)
		}
	}
	sort.Strings(ownedKeys)
	var b strings.Builder
	b.WriteString("export AH4C_ENV_LOCKED=")
	b.WriteString(shellQuote(strings.Join(lockKeys, ",")))
	b.WriteByte('\n')
	b.WriteString("export AH4C_ENV_OWNED=")
	b.WriteString(shellQuote(strings.Join(ownedKeys, ",")))
	b.WriteByte('\n')
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(shellQuote(sets[key]))
		b.WriteByte('\n')
	}
	return b.String()
}

func configComplete(merged func(string) string) bool {
	streamerApp := canonicalStreamerSelection(merged("STREAMER_APP"))
	return streamerApp == "" || validStreamerSelection(streamerApp)
}

func streamerTuneReady(merged func(string) string) bool {
	return validStreamerSelection(canonicalStreamerSelection(merged("STREAMER_APP")))
}

func tunerCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("must be a non-negative integer")
	}
	return n, nil
}

func parseLockSet(value string) map[string]bool {
	out := map[string]bool{}
	for _, key := range strings.Split(value, ",") {
		if key = strings.TrimSpace(key); key != "" {
			out[key] = true
		}
	}
	return out
}

func installSelectedScriptsRequested() bool {
	for _, arg := range os.Args[1:] {
		if arg == "-install-selected-scripts" {
			return true
		}
	}
	return false
}

func printModeRequested() bool {
	for _, arg := range os.Args[1:] {
		if arg == "-print-env" {
			return true
		}
	}
	return false
}

func logMaterialization(sets map[string]string, locked map[string]bool, sources map[string]string) {
	keys := make([]string, 0, len(sets)+len(locked))
	seen := map[string]bool{}
	for key := range sets {
		keys = append(keys, key)
		seen[key] = true
	}
	for key := range locked {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if locked[key] {
			logger("[CONFIG] %s locked by environment", key)
		} else {
			source := sources[key]
			if source == "" {
				source = "settings.json"
			}
			logger("[CONFIG] %s set from %s", key, source)
		}
	}
}

func envengineStartup() {
	s, loadWarnings, err := loadSettingsWithWarnings()
	settingsPresent := settingsFileExists()
	legacy := readLegacyEnv()
	if printModeRequested() {
		if err != nil {
			fmt.Fprintf(os.Stderr, "[CONFIG] ignoring malformed settings.json: %v\n", err)
			fmt.Print(shellExports(nil, nil, nonEmptyEnvironmentKeys(startupEnvironment)))
			fmt.Print("# settings.json ignored\n")
			os.Exit(0)
		}
		for _, warning := range loadWarnings {
			fmt.Fprintf(os.Stderr, "[CONFIG] %s\n", warning)
		}
		if settingsPresent && len(legacy) > 0 {
			fmt.Fprintln(os.Stderr, "[CONFIG] settings.json exists; managed values in ./env are ignored")
		}
		sets, locked, sources := materializeBootstrapPlan(os.Environ(), s, settingsPresent, legacy)
		lockedKeys := make([]string, 0, len(locked))
		for key := range locked {
			lockedKeys = append(lockedKeys, key)
		}
		sort.Strings(lockedKeys)
		for _, key := range lockedKeys {
			fmt.Fprintf(os.Stderr, "[CONFIG] %s locked by environment\n", key)
		}
		setKeys := make([]string, 0, len(sets))
		for key := range sets {
			setKeys = append(setKeys, key)
		}
		sort.Strings(setKeys)
		for _, key := range setKeys {
			fmt.Fprintf(os.Stderr, "[CONFIG] %s set from %s\n", key, sources[key])
		}
		fmt.Print(shellExports(sets, locked, nonEmptyEnvironmentKeys(startupEnvironment)))
		os.Exit(0)
	}
	if err != nil {
		logger("[CONFIG] ignoring malformed settings.json: %v", err)
		s = emptySettings()
	}
	for _, warning := range loadWarnings {
		logger("[CONFIG] %s", warning)
	}
	if settingsPresent && len(legacy) > 0 {
		logger("[CONFIG] settings.json exists; managed values in ./env are ignored")
	}
	lockValue, supervised := os.LookupEnv("AH4C_ENV_LOCKED")
	locked := parseLockSet(lockValue)
	owned := nonEmptyEnvironmentKeys(startupEnvironment)
	if supervised {
		owned = parseLockSet(os.Getenv("AH4C_ENV_OWNED"))
	}
	sets := map[string]string{}
	if !supervised {
		var sources map[string]string
		sets, locked, sources = materializeBootstrapPlan(os.Environ(), s, settingsPresent, legacy)
		for key, value := range sets {
			if err := os.Setenv(key, value); err != nil {
				logger("[CONFIG] could not set %s: %v", key, err)
			}
		}
		logMaterialization(sets, locked, sources)
	} else {
		for key, value := range s.Vars {
			if !locked[key] && os.Getenv(key) == value {
				sets[key] = value
			}
		}
		for key, value := range s.Extra {
			if !locked[key] && os.Getenv(key) == value {
				sets[key] = value
			}
		}
		if !locked["NUMBER_TUNERS"] {
			for key, value := range synthesizeTunerVars(s.Tuners) {
				if !locked[key] && os.Getenv(key) == value {
					sets[key] = value
				}
			}
		}
	}
	envEngineMu.Lock()
	envSettings = s
	envLocked = locked
	envOwned = owned
	envDockerManaged = supervised
	envEngineMu.Unlock()
	if supervised {
		logMaterialization(sets, locked, nil)
	}
	repairScriptPackageTransactions("scripts")
	if installSelectedScriptsRequested() {
		selection := canonicalStreamerSelection(os.Getenv("STREAMER_APP"))
		if selection == "" {
			fmt.Fprintln(os.Stderr, "[SCRIPTS] no STREAMER_APP configured; no scripts requested")
			os.Exit(0)
		}
		target := filepath.Join(strings.Split(selection, "/")...)
		if scriptPackageComplete(target) && !strings.EqualFold(os.Getenv("UPDATE_SCRIPTS"), "true") {
			fmt.Fprintf(os.Stderr, "[SCRIPTS] using stored %s package\n", selection)
			os.Exit(0)
		}
		if err := installScriptPackage(context.Background(), selection); err != nil {
			fmt.Fprintf(os.Stderr, "[SCRIPTS] %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "[SCRIPTS] installed %s from sullrich/ah4c\n", selection)
		os.Exit(0)
	}
	withWatchdog = envBoolTrueOrOne(os.Getenv("CC_WATCHDOG"))
	warnIfConfigNotPersistent()
}

func envBoolTrueOrOne(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "true") || strings.TrimSpace(value) == "1"
}

func mountPointState(dir string) (bool, string, string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true, "", ""
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return true, "", ""
	}
	defer f.Close()
	mounted, root := mountPointDetails(abs, f)
	return mounted, abs, root
}

func mountPointDetails(abs string, r io.Reader) (bool, string) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 5 && decodeMountInfoPath(fields[4]) == abs {
			return true, decodeMountInfoPath(fields[3])
		}
	}
	return false, ""
}

func decodeMountInfoPath(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

type persistentMountRequirement struct {
	Label            string
	CheckPath        string
	ContainerPath    string
	InvalidMountRoot string
}

func requiredPersistentMounts() []persistentMountRequirement {
	return []persistentMountRequirement{
		{Label: "Settings", CheckPath: filepath.Dir(settingsFilePath()), ContainerPath: "/opt/config", InvalidMountRoot: "/ah4c/config"},
	}
}

func filterMissingPersistentMounts(requirements []persistentMountRequirement, mounted func(persistentMountRequirement) bool) []persistentMountRequirement {
	missing := make([]persistentMountRequirement, 0, len(requirements))
	for _, requirement := range requirements {
		if !mounted(requirement) {
			missing = append(missing, requirement)
		}
	}
	return missing
}

func requiredMountPersistent(requirement persistentMountRequirement) bool {
	ok, _, root := mountPointState(requirement.CheckPath)
	if !ok {
		return false
	}
	return persistentMountRootValid(requirement, root)
}

func persistentMountRootValid(requirement persistentMountRequirement, root string) bool {
	return requirement.InvalidMountRoot == "" || filepath.Clean(root) != filepath.Clean(requirement.InvalidMountRoot)
}

func missingRequiredPersistentMounts() []persistentMountRequirement {
	if !runningInContainer() {
		return nil
	}
	return filterMissingPersistentMounts(requiredPersistentMounts(), requiredMountPersistent)
}

func runningInContainer() bool {
	if _, supervised := os.LookupEnv("AH4C_ENV_LOCKED"); supervised {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func warnIfConfigNotPersistent() {
	missing := missingRequiredPersistentMounts()
	if len(missing) == 0 {
		return
	}
	logger("[CONFIG] WARNING: setup is blocked because %d required persistent folder mount(s) are missing", len(missing))
	for _, requirement := range missing {
		logger("[CONFIG] WARNING: add a persistent folder for %s at %s", requirement.Label, requirement.ContainerPath)
	}
}
