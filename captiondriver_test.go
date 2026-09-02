package main

import (
	"strings"
	"testing"
)

func TestRuntimeStateNeedsRestore(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		active    bool
		manifests bool
		broken    int
		want      bool
	}{
		{name: "Vulkan loader without ICD", key: "vulkan", active: true, want: true},
		{name: "working Vulkan driver", key: "vulkan", active: true, manifests: true, want: false},
		{name: "broken Vulkan ICD", key: "vulkan", active: true, manifests: true, broken: 1, want: true},
		{name: "missing CUDA library", key: "cuda", active: false, want: true},
		{name: "working CUDA runtime", key: "cuda", active: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runtimeStateNeedsRestore(tt.key, tt.active, tt.manifests, tt.broken); got != tt.want {
				t.Fatalf("runtimeStateNeedsRestore() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDriverPackageInstallRefusesDowngrades(t *testing.T) {
	args := strings.Join(driverPackageInstallArgs("saved.deb"), "\x00")
	if !strings.Contains(args, "--refuse-downgrade") {
		t.Fatalf("dpkg arguments allow a saved package to downgrade the base image: %q", args)
	}
}
