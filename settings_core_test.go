package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsCoreCanonicalizesServerAddresses(t *testing.T) {
	old := emptySettings()
	next, err := validatedSettingsRequest(configSaveRequest{Vars: map[string]string{
		"IPADDRESS":  "ah4c.local",
		"CHANNELSIP": "https://channels.local:9443/",
	}}, old)
	if err != nil {
		t.Fatalf("validatedSettingsRequest returned error: %v", err)
	}
	if got, want := next.Vars["IPADDRESS"], "http://ah4c.local:7654"; got != want {
		t.Fatalf("IPADDRESS = %q, want %q", got, want)
	}
	if got, want := next.Vars["CHANNELSIP"], "https://channels.local:9443"; got != want {
		t.Fatalf("CHANNELSIP = %q, want %q", got, want)
	}
}

func TestSettingsCoreRejectsContainerOwnedExtra(t *testing.T) {
	const key = "SETTINGS_CORE_CONTAINER_VALUE"
	envEngineMu.Lock()
	oldValue, existed := envOwned[key]
	envOwned[key] = true
	envEngineMu.Unlock()
	t.Cleanup(func() {
		envEngineMu.Lock()
		defer envEngineMu.Unlock()
		if existed {
			envOwned[key] = oldValue
		} else {
			delete(envOwned, key)
		}
	})

	_, err := validatedSettingsRequest(configSaveRequest{Extra: map[string]string{key: "replacement"}}, emptySettings())
	if err == nil || !strings.Contains(err.Error(), "owned by the container environment") {
		t.Fatalf("expected container ownership error, got %v", err)
	}
}

func TestSettingsCoreAllowsPersistedExtraAfterSupervisedRestart(t *testing.T) {
	const key = "SETTINGS_CORE_PERSISTED_VALUE"
	envEngineMu.Lock()
	oldOwned, existed := envOwned[key]
	delete(envOwned, key)
	envEngineMu.Unlock()
	t.Cleanup(func() {
		envEngineMu.Lock()
		defer envEngineMu.Unlock()
		if existed {
			envOwned[key] = oldOwned
		} else {
			delete(envOwned, key)
		}
	})

	next, err := validatedSettingsRequest(configSaveRequest{Extra: map[string]string{key: "saved"}}, emptySettings())
	if err != nil {
		t.Fatalf("persisted settings-owned extra was rejected: %v", err)
	}
	if got := next.Extra[key]; got != "saved" {
		t.Fatalf("persisted extra = %q, want saved", got)
	}
}

func TestSettingsCoreRejectsInternalExtra(t *testing.T) {
	_, err := validatedSettingsRequest(configSaveRequest{Extra: map[string]string{
		"AH4C_ENV_LOCKED": "forged",
	}}, emptySettings())
	if err == nil || !strings.Contains(err.Error(), "must be set in the container environment") {
		t.Fatalf("expected internal variable rejection, got %v", err)
	}
}

func TestSettingsCoreOnlyRequiresConfigMount(t *testing.T) {
	requirements := requiredPersistentMounts()
	if len(requirements) != 1 {
		t.Fatalf("requiredPersistentMounts returned %d entries, want 1", len(requirements))
	}
	if got, want := requirements[0].ContainerPath, "/opt/config"; got != want {
		t.Fatalf("required mount = %q, want %q", got, want)
	}
}

func TestM3UTemplateAddressSupportsBothTemplateShapes(t *testing.T) {
	tests := []struct {
		name             string
		value            string
		templateAddsPort bool
		want             string
	}{
		{name: "template supplies port", value: "https://ah4c.local:9443", templateAddsPort: true, want: "ah4c.local"},
		{name: "template uses configured port", value: "https://ah4c.local:9443", want: "ah4c.local:9443"},
		{name: "default port", value: "ah4c.local", want: "ah4c.local:7654"},
		{name: "IPv6 template supplies port", value: "2001:db8::5", templateAddsPort: true, want: "[2001:db8::5]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := m3uTemplateAddress(test.value, test.templateAddsPort)
			if err != nil {
				t.Fatalf("m3uTemplateAddress returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("m3uTemplateAddress = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSettingsCoreDeploymentFilesProvidePersistentConfig(t *testing.T) {
	for _, name := range []string{"ah4c.yaml", "ah4c-minimal.yaml"} {
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(contents), ":/opt/config") {
			t.Fatalf("%s does not mount /opt/config", name)
		}
	}
	minimal, err := os.ReadFile("ah4c-minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(minimal), "AH4C_COMPOSE=") {
		t.Fatal("ah4c-minimal.yaml does not set AH4C_COMPOSE")
	}
}

func TestSettingsCorePageDoesNotReferenceRemovedFeatureFunctions(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("html", "settings.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(contents)
	for _, removed := range []string{
		"decodeStreamerChoice(",
		"/api/config/check-connection",
		"/api/config/apple-tv-pairing",
		"/api/config/preroll",
		"/api/config/local-scripts",
	} {
		if strings.Contains(page, removed) {
			t.Fatalf("settings core still references removed feature %q", removed)
		}
	}
}
