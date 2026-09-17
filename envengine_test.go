package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSettingsUsesPrivateAtomicFile(t *testing.T) {
	oldPath := settingsPathOverride
	settingsPathOverride = t.TempDir() + "/nested/settings.json"
	t.Cleanup(func() { settingsPathOverride = oldPath })
	s := emptySettings()
	s.Vars["IPADDRESS"] = "ah4c:7654"
	if err := saveSettings(s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(settingsPathOverride)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("settings mode = %o", info.Mode().Perm())
	}
	if _, err := os.Stat(settingsPathOverride + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file remains: %v", err)
	}
}

func TestFilterMissingPersistentMountsReportsEveryMissingFolder(t *testing.T) {
	requirements := []persistentMountRequirement{
		{Label: "Settings", CheckPath: "/opt/config", ContainerPath: "/opt/config"},
		{Label: "Scripts", CheckPath: "/opt/scripts", ContainerPath: "/opt/scripts"},
		{Label: "M3Us", CheckPath: "/opt/m3u", ContainerPath: "/opt/m3u"},
	}
	mounted := map[string]bool{"/opt/config": true}
	missing := filterMissingPersistentMounts(requirements, func(requirement persistentMountRequirement) bool {
		return mounted[requirement.CheckPath]
	})
	if len(missing) != 2 || missing[0].ContainerPath != "/opt/scripts" || missing[1].ContainerPath != "/opt/m3u" {
		t.Fatalf("missing mounts = %#v", missing)
	}
}

func TestRequiredMountRejectsEmptyHostDirRoot(t *testing.T) {
	oldPath := settingsPathOverride
	settingsPathOverride = "/opt/config/settings.json"
	t.Cleanup(func() { settingsPathOverride = oldPath })
	requirements := requiredPersistentMounts()
	mountRoots := map[string]string{
		"/opt/config":    "/ah4c/config",
		"/opt/scripts":   "/ah4c/scripts",
		"/opt/m3u":       "/ah4c/m3u",
		"/root/.android": "/ah4c/adb",
		"/opt/captions":  "/ah4c/captions",
		"/opt/preroll":   "/ah4c/preroll",
	}
	missing := filterMissingPersistentMounts(requirements, func(requirement persistentMountRequirement) bool {
		return persistentMountRootValid(requirement, mountRoots[requirement.CheckPath])
	})
	if len(missing) != len(requirements) {
		t.Fatalf("missing mounts = %d, want %d: %#v", len(missing), len(requirements), missing)
	}
}

func TestMountPointDetailsReadsBindSourceRoot(t *testing.T) {
	mountInfo := "390 213 0:2 /ah4c/scripts /opt/scripts rw - rootfs rootfs rw\n" +
		"391 213 0:2 /srv/ah4c/config /opt/config rw - rootfs rootfs rw\n"
	if mounted, root := mountPointDetails("/opt/scripts", strings.NewReader(mountInfo)); !mounted || root != "/ah4c/scripts" {
		t.Fatalf("scripts mount = (%v, %q)", mounted, root)
	}
	if mounted, root := mountPointDetails("/opt/config", strings.NewReader(mountInfo)); !mounted || root != "/srv/ah4c/config" {
		t.Fatalf("config mount = (%v, %q)", mounted, root)
	}
}

func TestHostDirMarkerOnlyBlocksWhenExplicitlyEmpty(t *testing.T) {
	t.Setenv("AH4C_HOST_DIR_CONFIGURED", "")
	if !hostDirMarkerMissing() {
		t.Fatal("empty HOST_DIR marker was accepted")
	}
	t.Setenv("AH4C_HOST_DIR_CONFIGURED", "true")
	if hostDirMarkerMissing() {
		t.Fatal("configured HOST_DIR marker was rejected")
	}
}

func TestEnvValueUsable(t *testing.T) {
	for _, tc := range []struct {
		value   string
		present bool
		want    bool
	}{
		{"true", true, true},
		{"", true, false},
		{"", false, false},
	} {
		if got := envValueUsable(tc.value, tc.present); got != tc.want {
			t.Fatalf("envValueUsable(%q, %v) = %v, want %v", tc.value, tc.present, got, tc.want)
		}
	}
}

func TestComputeLockSetTreatsEmptyAsUnset(t *testing.T) {
	s := Settings{Vars: map[string]string{"PLAYBACK_DETECTION": "false"}, Extra: map[string]string{"MY_KNOB": "x"}}
	got := computeLockSet([]string{"PLAYBACK_DETECTION=", "IPADDRESS=ah4c:7654", "TUNER1_IP=box:5555", "MY_KNOB="}, s)
	if got["PLAYBACK_DETECTION"] || got["MY_KNOB"] {
		t.Fatal("empty environment values must not lock settings")
	}
	if !got["IPADDRESS"] || !got["TUNER1_IP"] {
		t.Fatal("non-empty catalog and tuner values must lock settings")
	}
}

func TestSynthesizeTunerVars(t *testing.T) {
	got := synthesizeTunerVars([]TunerSpec{{TunerIP: "box", EncoderURL: "http://enc", CMD: "ffmpeg", TEECMD: "tee"}})
	want := map[string]string{"NUMBER_TUNERS": "1", "TUNER1_IP": "box", "ENCODER1_URL": "http://enc", "CMD1": "ffmpeg", "TEECMD1": "tee"}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s = %q, want %q", key, got[key], value)
		}
	}
	if got := synthesizeTunerVars(nil)["NUMBER_TUNERS"]; got != "0" {
		t.Fatalf("empty tuner list produced NUMBER_TUNERS=%q", got)
	}
}

func TestMaterializePlanPrecedence(t *testing.T) {
	s := Settings{
		Vars:   map[string]string{"IPADDRESS": "json:7654", "PLAYBACK_DETECTION": "true"},
		Tuners: []TunerSpec{{TunerIP: "json-box", EncoderURL: "http://json-enc"}},
		Extra:  map[string]string{"MY_KNOB": "json"},
	}
	sets, locked := materializePlan([]string{"IPADDRESS=env:7654", "PLAYBACK_DETECTION=", "MY_KNOB=env", "TUNER1_IP=env-box"}, s)
	if sets["IPADDRESS"] != "" || !locked["IPADDRESS"] {
		t.Fatal("non-empty environment did not win")
	}
	if sets["PLAYBACK_DETECTION"] != "true" || locked["PLAYBACK_DETECTION"] {
		t.Fatal("empty environment should be filled from settings")
	}
	if sets["TUNER1_IP"] != "" || !locked["TUNER1_IP"] {
		t.Fatal("individual tuner environment value did not win")
	}
	if sets["NUMBER_TUNERS"] != "1" {
		t.Fatalf("NUMBER_TUNERS = %q", sets["NUMBER_TUNERS"])
	}
}

func TestMaterializeBootstrapPrecedence(t *testing.T) {
	s := emptySettings()
	s.Vars["SPEED_MODE"] = "true"
	sets, locked, sources := materializeBootstrapPlan(
		[]string{"PYATV=true", "UPDATE_M3US="}, s, true,
		map[string]string{"SPEED_MODE": "false", "UPDATE_M3US": "false"},
	)
	if !locked["PYATV"] || sets["PYATV"] != "" {
		t.Fatal("real environment must win and lock")
	}
	if sets["SPEED_MODE"] != "true" || sources["SPEED_MODE"] != "settings.json" {
		t.Fatal("settings must win over ./env")
	}
	if sets["UPDATE_M3US"] != "true" || sources["UPDATE_M3US"] != "built-in default" {
		t.Fatal("managed ./env values must not return after settings.json exists")
	}
	if sets["ENCODER_CODEC"] != "h264" || sources["ENCODER_CODEC"] != "built-in default" {
		t.Fatal("built-in default was not materialized")
	}
}

func TestSettingsFileKeepsTunersWhenOneVarIsUnknown(t *testing.T) {
	oldPath := settingsPathOverride
	settingsPathOverride = filepath.Join(t.TempDir(), "settings.json")
	t.Cleanup(func() { settingsPathOverride = oldPath })
	data := `{"version":1,"vars":{"IPADDRESS":"ah4c:7654","FUTURE_SETTING":"kept"},"tuners":[{"tunerIP":"box","encoderURL":"http://encoder"}],"extra":{}}`
	if err := os.WriteFile(settingsPathOverride, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	settings, warnings, err := loadSettingsWithWarnings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Tuners) != 1 || settings.Tuners[0].TunerIP != "box" {
		t.Fatalf("tuners were discarded: %#v", settings.Tuners)
	}
	if settings.Extra["FUTURE_SETTING"] != "kept" {
		t.Fatalf("unknown safe variable was not preserved: %#v", settings.Extra)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "FUTURE_SETTING") {
		t.Fatalf("unknown variable was not reported: %#v", warnings)
	}
}

func TestPreviouslyAcceptedSettingsStillLoadIntact(t *testing.T) {
	oldPath := settingsPathOverride
	settingsPathOverride = filepath.Join(t.TempDir(), "settings.json")
	t.Cleanup(func() { settingsPathOverride = oldPath })
	want := emptySettings()
	want.Vars = map[string]string{
		"IPADDRESS":         "http://192.168.200.40:7655",
		"CHANNELSIP":        "channels-dvr:8090",
		"ALERT_SMTP_SERVER": "smtp.example.com",
		"ALERT_EMAIL_FROM":  "ah4c@example.com",
		"ALERT_EMAIL_TO":    "viewer@example.com",
		"UPDATE_SCRIPTS":    "TRUE",
		"PLAYBACK_DELAY":    "24h",
		"KEEP_WATCHING":     "240m",
		"LIVETV_ATTEMPTS":   "defined-by-the-script",
		"SPEED_MODE":        "1",
		"AUTOCROP_CHANNELS": "2.1 7.2 120",
	}
	want.Tuners = []TunerSpec{{
		TunerIP: "living-room:5555", EncoderURL: "http://encoder:8090/stream",
		CMD: "custom command --flag", TEECMD: "tee /tmp/capture",
	}}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPathOverride, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, warnings, err := loadSettingsWithWarnings()
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("valid settings produced warnings: %v", warnings)
	}
	for key, value := range want.Vars {
		if got.Vars[key] != value {
			t.Errorf("%s = %q, want %q", key, got.Vars[key], value)
		}
	}
	if len(got.Tuners) != 1 || got.Tuners[0] != want.Tuners[0] {
		t.Fatalf("tuners = %#v, want %#v", got.Tuners, want.Tuners)
	}
}

func TestCanonicalStreamerSelection(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"scripts/firetv/hulu/", "scripts/firetv/hulu"},
		{"./scripts/firetv/hulu", "scripts/firetv/hulu"},
		{"  ./scripts/firetv/hulu/  ", "scripts/firetv/hulu"},
		{"./scripts/david/", "scripts/david"},
	} {
		if got := canonicalStreamerSelection(tc.value); got != tc.want {
			t.Fatalf("canonicalStreamerSelection(%q) = %q", tc.value, got)
		}
		if !validStreamerSelection(tc.value) {
			t.Fatalf("validStreamerSelection(%q) = false", tc.value)
		}
	}
	sets, locked, _ := materializeBootstrapPlan([]string{"STREAMER_APP=./scripts/firetv/hulu/"}, emptySettings(), false, nil)
	if sets["STREAMER_APP"] != "scripts/firetv/hulu" || !locked["STREAMER_APP"] {
		t.Fatalf("environment streamer was not normalized and locked: sets=%#v locked=%#v", sets, locked)
	}
}

func TestLegacyTunerTopologyWinsWhenSettingsFileIsAbsent(t *testing.T) {
	sets, _, sources := materializeBootstrapPlan(nil, emptySettings(), false, map[string]string{
		"NUMBER_TUNERS": "1", "TUNER1_IP": "legacy-box", "ENCODER1_URL": "http://legacy-encoder",
	})
	if sets["NUMBER_TUNERS"] != "1" || sets["TUNER1_IP"] != "legacy-box" || sources["NUMBER_TUNERS"] != "./env" {
		t.Fatalf("legacy tuner topology not preserved: %#v %#v", sets, sources)
	}
}

func TestJSONTunerEmptyFieldBlocksLegacyFallback(t *testing.T) {
	s := emptySettings()
	s.Tuners = []TunerSpec{{TunerIP: "box", CMD: "capture"}}
	sets, _, _ := materializeBootstrapPlan(nil, s, true, map[string]string{"ENCODER1_URL": "http://old-encoder"})
	value, exists := sets["ENCODER1_URL"]
	if !exists || value != "" {
		t.Fatalf("empty JSON tuner field did not remain authoritative: %#v", sets)
	}
}

func TestLockedNumberTunersIgnoresJSONTopology(t *testing.T) {
	s := Settings{Tuners: []TunerSpec{{TunerIP: "json-box", EncoderURL: "http://json-enc"}}}
	sets, locked := materializePlan([]string{"NUMBER_TUNERS=2"}, s)
	if !locked["NUMBER_TUNERS"] {
		t.Fatal("NUMBER_TUNERS should be locked")
	}
	if _, ok := sets["TUNER1_IP"]; ok {
		t.Fatal("JSON tuner topology must be ignored when NUMBER_TUNERS is locked")
	}
}

func TestShellExportsQuotesHostileValues(t *testing.T) {
	out := shellExports(map[string]string{"SAFE": "a b'$(touch /tmp/nope)"}, map[string]bool{"IPADDRESS": true})
	if !strings.Contains(out, "export SAFE='a b'\\''$(touch /tmp/nope)'") {
		t.Fatalf("unexpected shell quoting: %s", out)
	}
	if !strings.Contains(out, "export AH4C_ENV_LOCKED='IPADDRESS'") {
		t.Fatalf("missing lock export: %s", out)
	}
	for _, bad := range []map[string]string{{"bad-key": "x"}, {"SAFE": "one\ntwo"}} {
		out = shellExports(bad, nil)
		if strings.Contains(out, "export SAFE=") || !strings.Contains(out, "AH4C_ENV_LOCKED=''") {
			t.Fatalf("bad export was not rejected: %s", out)
		}
	}
}

func TestConfigComplete(t *testing.T) {
	values := map[string]string{"STREAMER_APP": "scripts/firetv/hulu", "NUMBER_TUNERS": "0"}
	if !configComplete(func(key string) string { return values[key] }) {
		t.Fatal("zero-tuner configuration with a selected script was rejected")
	}
	values["STREAMER_APP"] = ""
	if !configComplete(func(key string) string { return values[key] }) {
		t.Fatal("configuration without streamer app was rejected")
	}
	values["STREAMER_APP"] = "hulu"
	if configComplete(func(key string) string { return values[key] }) {
		t.Fatal("configuration with an invalid streamer path was accepted")
	}
}

func TestStreamerTuneReadyRequiresValidSelection(t *testing.T) {
	values := map[string]string{}
	lookup := func(key string) string { return values[key] }
	if streamerTuneReady(lookup) {
		t.Fatal("tuning was ready without a streamer script")
	}
	values["STREAMER_APP"] = "hulu"
	if streamerTuneReady(lookup) {
		t.Fatal("tuning was ready with an invalid streamer path")
	}
	values["STREAMER_APP"] = "scripts/firetv/hulu"
	if !streamerTuneReady(lookup) {
		t.Fatal("tuning was not ready with a valid streamer path")
	}
}

func TestTunerCountDegradesMissingConfigurationToZero(t *testing.T) {
	if got, err := tunerCount(""); err != nil || got != 0 {
		t.Fatalf("missing count = %d, %v", got, err)
	}
	for _, value := range []string{"nope", "-1"} {
		if got, err := tunerCount(value); err == nil || got != 0 {
			t.Fatalf("invalid count %q = %d, %v", value, got, err)
		}
	}
}

func TestBoolCompatibility(t *testing.T) {
	for _, value := range []string{"true", "TRUE", "1"} {
		if !envBoolTrueOrOne(value) {
			t.Fatalf("%q should enable a boolean", value)
		}
	}
	for _, value := range []string{"false", "0", "yes", ""} {
		if envBoolTrueOrOne(value) {
			t.Fatalf("%q should not enable a boolean", value)
		}
	}
}

func TestValidateSettingsSchemaRejectsUnsafeKeysAndValues(t *testing.T) {
	for _, settings := range []Settings{
		{Version: 1, Vars: map[string]string{"NOT_CATALOGED": "x"}},
		{Version: 1, Extra: map[string]string{"bad-key": "x"}},
		{Version: 1, Extra: map[string]string{"CMD1": "x"}},
		{Version: 1, Extra: map[string]string{"SAFE_KEY": "one\ntwo"}},
		{Version: 1, Tuners: []TunerSpec{{CMD: "one\ntwo"}}},
	} {
		if err := validateSettingsSchema(normalizeSettings(settings)); err == nil {
			t.Fatalf("unsafe settings were accepted: %#v", settings)
		}
	}
}
