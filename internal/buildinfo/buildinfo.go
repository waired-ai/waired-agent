// Package buildinfo exposes the binary's build-time version and commit,
// stamped via the linker (-ldflags -X). Every cmd/* main is built with
// these set (see the Makefile LDFLAGS_VERSION); library code reads
// buildinfo.Version instead of hardcoding "0.1.0".
//
// This is the canonical version surface for waired/waired-agent/waired-tray
// and the foundation the update mechanism builds on: the `waired version`
// subcommand reports it, enrollment sends it as the client version, and
// the installer / `waired update` (#293) / auto-check (#294) compare it
// against the latest published release.
package buildinfo

// Version and BuildSHA are overwritten at link time via:
//
//	-X github.com/waired-ai/waired-agent/internal/buildinfo.Version=<version>
//	-X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=<short-sha>
//
// The defaults apply to `go run` / `go test` and to any build that
// forgets the ldflags.
var (
	Version  = "0.0.0-dev"
	BuildSHA = ""
)

// Short returns the version with the commit appended when known, e.g.
// "0.0.1-rc6 (a1b2c3d)" — or just "0.0.0-dev" when no SHA was stamped.
func Short() string {
	if BuildSHA == "" {
		return Version
	}
	return Version + " (" + BuildSHA + ")"
}
