// Package controlurl resolves which Control Plane this device talks to
// before it is enrolled.
//
// Both binaries need the same answer and used to disagree. `waired init`
// (cmd/waired) had a three-tier resolution — explicit flag/env, the
// installer-recorded agent.env, a baked production default — while the
// daemon's login controller (cmd/waired-agent, the app's "Sign in…" ->
// POST /waired/v1/login/start) had only the process environment, which
// launchd and the Windows SCM cannot supply at all. A stock install that
// was not enrolled at install time therefore failed sign-in from the app
// on every OS (#174, the daemon half of #42). This package is the single
// implementation both import; it is not a fourth mechanism.
//
// Once a device IS enrolled the URL comes from identity.json instead
// (Identity.ControlURL) and nothing here is consulted.
package controlurl

import (
	"bufio"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// Default is the production Control Plane used when the operator passed
// no --control, set no $WAIRED_CONTROL_URL, and the installer recorded
// none in agent.env. It lets a bare `waired init` (or the installer's
// auto-init, or app sign-in on a stock install) work with no
// configuration. Overridable by either higher-priority source.
const Default = "https://app.waired.ai"

// linuxEnvFileDir holds the systemd EnvironmentFile the installer writes.
// Declared here rather than imported from internal/platform/service
// because service.LinuxEnvFilePath lives in a //go:build linux file and
// this package is deliberately untagged so envFileDir can be table-tested
// over all three GOOS values on any runner. TestLinuxEnvFilePathMatchesService
// (linux-tagged) fails if the two ever drift.
const linuxEnvFileDir = "/etc/waired"

// Resolve applies the control-URL precedence: an explicit --control /
// $WAIRED_CONTROL_URL wins, then the installer-recorded value from
// agent.env (platformDefault, see PlatformDefault), then the baked
// production Default. The result still has to go through Normalize.
func Resolve(explicit, platformDefault string) string {
	url, _ := ResolveWithSource(explicit, platformDefault)
	return url
}

// Source names the layer of the precedence that supplied the URL.
//
// It exists because the three layers mean very different things to a
// person and the URL alone cannot tell them apart. In waired-agent#800 a
// host lost its state dir, agent.env went with it, and the fallback to
// SourceBuiltin silently pointed a dev machine at production — an
// operator who completed that sign-in would have enrolled into a
// different control plane with nothing on screen to say so.
type Source string

const (
	// SourceOperator — an explicit --control or $WAIRED_CONTROL_URL.
	SourceOperator Source = "operator"
	// SourceInstaller — the value the installer recorded in agent.env.
	SourceInstaller Source = "installer"
	// SourceBuiltin — nothing said otherwise, so this is Default. The one
	// worth naming out loud: it is reached both by a stock install (where
	// it is correct) and by a host that LOST the answer (where it is not),
	// and this package cannot tell those apart.
	SourceBuiltin Source = "builtin"
)

// ResolveWithSource is Resolve plus which layer answered. Resolve is the
// façade for the callers that only need the URL.
func ResolveWithSource(explicit, platformDefault string) (string, Source) {
	if explicit != "" {
		return explicit, SourceOperator
	}
	if platformDefault != "" {
		return platformDefault, SourceInstaller
	}
	return Default, SourceBuiltin
}

// envFileDir returns the directory holding agent.env for goos. Linux
// uses the systemd EnvironmentFile's fixed path; macOS and Windows use
// the system state dir — <state dir>/agent.env is what install.sh's
// darwin_write_control_url and install.ps1's Write-ControlUrlEnvFile
// write.
//
// This is the (GOOS, facts) -> decision seam: it returns the directory
// rather than the joined path so a table test over all three GOOS values
// is meaningful on a single-OS runner (filepath.Join would otherwise bake
// in the host's separator).
func envFileDir(goos, systemStateDir string) string {
	if goos == "linux" {
		return linuxEnvFileDir
	}
	return systemStateDir
}

// EnvFilePath is the agent.env location for goos, given that OS's system
// state dir (paths.StateDir(paths.System)).
func EnvFilePath(goos, systemStateDir string) string {
	return filepath.Join(envFileDir(goos, systemStateDir), "agent.env")
}

// PlatformDefault returns the Control Plane URL the installer recorded in
// this host's agent.env, or "" when none is set or the file is
// unreadable.
//
// It is how `install.sh --control <URL>` / `-Dev` survive an install
// where enrollment did not happen inline: sudo strips the caller's
// environment, and on macOS and Windows the service manager cannot feed
// the daemon an env file at all, so the file is the only place the choice
// persists. The System state dir is used unconditionally (not a --state-dir
// override) because that is where every installer writes it, and because
// on Linux the path is fixed by the systemd unit regardless.
//
// A non-elevated read returns "": the state dir is locked down to
// SYSTEM/root + administrators, and the parser treats any read error as
// "not configured". That is harmless — both callers are elevated (the
// daemon runs as a service, `waired init` must be elevated to write
// identity.json).
func PlatformDefault() string {
	return ParseEnvFile(EnvFilePath(runtime.GOOS, paths.StateDir(paths.System)))
}

// ParseEnvFile reads WAIRED_CONTROL_URL from a systemd-style KEY=VALUE
// env file. Any read error is treated as "not configured" and returns ""
// — never fatal (the file is typically owner-only and may be unreadable
// to an unprivileged caller).
func ParseEnvFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "WAIRED_CONTROL_URL" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v != "" {
			return v
		}
	}
	return ""
}

// Normalize canonicalises a Control Plane URL supplied via --control,
// $WAIRED_CONTROL_URL, or agent.env. A bare host like "dev.waired.net"
// (no scheme) is the natural thing to type, but net/http rejects it
// ("unsupported protocol scheme \"\"") once it reaches the enroll POST,
// so we prepend a scheme here: https for remote hosts, http for loopback
// (matching the --control example http://127.0.0.1:9477). An empty input
// is returned unchanged so the caller's "required" check still fires.
// Non-http(s) schemes and host-less inputs are rejected with a clear
// message.
//
// Every resolved value must pass through here, including agent.env's:
// the installers persist the operator's --control string verbatim, so a
// scheme-less --dev/--control install leaves a bare host in the file.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		host := s
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if isLoopbackHost(host) {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	u, err := url.ParseRequestURI(s)
	if err != nil {
		return "", fmt.Errorf("invalid control URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid control URL %q: scheme %q is not http or https", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid control URL %q: missing host", raw)
	}
	return strings.TrimRight(s, "/"), nil
}

// isLoopbackHost reports whether host (a hostname or host:port, no
// scheme) is loopback. Loopback control planes are the local-dev default
// and speak plain http.
func isLoopbackHost(host string) bool {
	h := host
	if strings.HasPrefix(h, "[") { // [::1]:port
		if i := strings.Index(h, "]"); i >= 0 {
			h = h[1:i]
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i] // host:port (single colon → not a bare IPv6)
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.IsLoopback()
	}
	return false
}
