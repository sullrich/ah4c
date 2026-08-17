package main

import (
	_ "embed"
	"strings"
)

// versionFile is the build stamp, embedded rather than passed in with -ldflags.
//
// Embedding means a binary always knows its own version however it was built —
// go build on a laptop, the Dockerfile, or the workflow — with nothing to
// remember to pass. scripts/bump-version.sh writes it, in UTC, and the build
// runs that before compiling.
//
//go:embed VERSION
var versionFile string

// buildVersion is what the page and the log show: vYYYY.MM.DD.HHMM in UTC.
func buildVersion() string {
	v := strings.TrimSpace(versionFile)
	if v == "" {
		return "dev"
	}
	return v
}
