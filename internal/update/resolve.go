// Package update resolves the latest published waired release and compares
// it against the running build, powering the manual update path (#293).
//
// The daemon (waired-agent) exposes this over the Local Management API as a
// read-only check/status surface; it runs unprivileged, so it never applies
// anything. The actual apply is driven by the CLI (`waired update`) and the
// tray, which delegate to the existing installer scripts under elevation —
// see packaging/install/install.{sh,ps1}.
//
// Version-feed source (user decision, #293): apt query on Linux
// (resolve_linux.go), the mirror's GitHub Releases API on Windows/macOS
// (resolve_other.go). Both compare against buildinfo.Version via
// internal/version. The env conventions (WAIRED_INSTALL_REPO) mirror
// install.sh so the daemon and the installer agree on "latest".
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/version"
)

// defaultInstallRepo mirrors install.sh's WAIRED_INSTALL_REPO — the public
// mirror whose GitHub Releases feed the version check.
const defaultInstallRepo = "waired-ai/waired-install"

// Result is the outcome of a version check.
type Result struct {
	// Available is true when Latest is a strictly newer real release than
	// Current. Always false for dev/edge builds (see isDevVersion).
	Available bool
	// Current is the running build (buildinfo.Version).
	Current string
	// Latest is the resolved latest published version, or "" if unresolved.
	Latest string
}

// Resolver resolves the latest published version. The GOOS-specific
// LatestVersion implementation is selected at build time (resolve_linux.go
// vs resolve_other.go). Every external dependency is injectable so tests run
// without real network or apt.
type Resolver struct {
	// HTTPClient is used by the GitHub-API path. nil => a 5s-timeout default.
	HTTPClient *http.Client
	// Repo is the "owner/name" whose releases/latest is queried. "" =>
	// WAIRED_INSTALL_REPO env, else defaultInstallRepo.
	Repo string
	// apiBase overrides the GitHub API base (default https://api.github.com);
	// injected by tests. "" => the real API.
	apiBase string
	// runCommand runs an external command and returns its stdout; nil =>
	// exec.CommandContext. Injected by tests (Linux apt path).
	runCommand func(ctx context.Context, name string, args ...string) (string, error)
}

// Check resolves the latest version and compares it against current.
func (r *Resolver) Check(ctx context.Context, current string) (Result, error) {
	res := Result{Current: current}
	latest, err := r.LatestVersion(ctx)
	if err != nil {
		return res, err
	}
	res.Latest = latest
	res.Available = available(current, latest)
	return res, nil
}

// available reports whether latest is a strictly newer real release than
// current. It is a pure function so the comparison policy is testable
// cross-OS. Dev/edge builds (the "0.0.0-<sha>" / "0.0.0-dev" sentinels and
// the "<core>-edge.<ts>+<sha>" edge versions) are never flagged: comparing
// them to a stable tag is meaningless and would nag every developer and
// edge-channel host. Stable manual update is the #293 scope; edge update
// flow is deferred to #294/#295.
func available(current, latest string) bool {
	if latest == "" || isDevVersion(current) {
		return false
	}
	cmp, ok := version.Compare(current, latest)
	return ok && cmp < 0
}

// isDevVersion reports whether v is a non-release sentinel — empty, "dev",
// the Makefile's "0.0.0-<...>" default (LDFLAGS_VERSION /
// PKG_VERSION ?= 0.0.0-$(VERSION)), or an edge (latest-main) build. Edge
// versions appear in two shapes: the semver "<core>-edge.<ts>+<sha>" (macOS /
// Windows binaries) and the dpkg "<core>~edge.<ts>+<sha>" (the .deb package
// version); matching the "edge." token covers both. Edge hosts must never be
// nagged to a stable tag — their base core may even equal that tag.
func isDevVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == "dev" || strings.HasPrefix(v, "0.0.0") || strings.Contains(v, "edge.")
}

func (r *Resolver) repo() string {
	if r.Repo != "" {
		return r.Repo
	}
	if env := os.Getenv("WAIRED_INSTALL_REPO"); env != "" {
		return env
	}
	return defaultInstallRepo
}

func (r *Resolver) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// latestFromGitHub queries the mirror's GitHub Releases "latest" endpoint
// (the newest non-prerelease, so the `edge` prerelease is excluded) and
// returns its tag_name. Mirrors install.sh's resolve_latest_version.
func (r *Resolver) latestFromGitHub(ctx context.Context) (string, error) {
	base := r.apiBase
	if base == "" {
		base = "https://api.github.com"
	}
	url := base + "/repos/" + r.repo() + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github releases/latest: status %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode github release: %w", err)
	}
	tag := strings.TrimSpace(body.TagName)
	if tag == "" {
		return "", fmt.Errorf("github releases/latest: empty tag_name")
	}
	return tag, nil
}
