// Package version holds build-time identification for the databox binary.
//
// The values in this package are plain strings so that release builds can
// overwrite them with the Go linker, for example:
//
//	go build -ldflags "-X github.com/hyperkubeorg/databox/pkg/version.Version=v1.0.0"
//
// Nothing in here has behavior; it exists so every part of the program
// (CLI `databox version`, the HTTP API, the GUI footer, logs) reports the
// exact same build information.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version of this build. "dev" means the binary
	// was built directly from a working tree rather than a tagged release.
	Version = "dev"

	// Commit is the git commit hash the binary was built from, when known.
	Commit = "unknown"

	// BuildDate is the UTC timestamp of the build, when known.
	BuildDate = "unknown"
)

// String renders a single human-readable line describing the build,
// e.g. "databox dev (commit unknown, built unknown, go1.26.4 linux/amd64)".
func String() string {
	return fmt.Sprintf("databox %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
