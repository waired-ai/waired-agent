//go:build darwin

package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Darwin runs waired-agent as a system LaunchDaemon (not a per-user
// LaunchAgent): a root-owned job under /Library/LaunchDaemons that
// launchd starts at boot, independent of any GUI (Aqua) login session.
// This matches the Linux systemd unit (system, boot-time) and the
// Windows SCM service (LocalSystem, Automatic) so a headless / server
// Mac runs the agent before — and without — anyone logging in.
//
// Run-as is root (no UserName key), the macOS analog of Windows
// LocalSystem and the model the open-source `tailscaled
// install-system-daemon` uses. The data plane is pure userspace
// (netstack TUN) so root is not functionally required, but launchd
// plists cannot express the systemd sandbox directives (ProtectSystem,
// NoNewPrivileges, …) that make a dedicated unprivileged user worthwhile
// on Linux, and macOS has no `useradd --system` convention for daemons.
// At-rest secret hardening instead comes from the System keychain
// (internal/platform/keychain, root-owned, session-less). See #520.
//
// The tray stays a per-user LaunchAgent (internal/platform/autostart) —
// it is a menu-bar GUI app — and reaches this daemon over the loopback
// management API.

const (
	// darwinLabel is the launchd job label, used both as the plist's
	// <key>Label</key> value and the suffix on `launchctl ... system/<label>`.
	darwinLabel = "com.waired.agent"
)

// runLaunchctlFn is overridden in tests so we can assert the argv that
// would be passed to launchctl without actually exec-ing it.
var runLaunchctlFn = runLaunchctlReal

// geteuidFn is overridden in tests so the root requirement on Install /
// Uninstall can be exercised on a non-root CI host. systemDaemonDir
// (declared in proxy_dropin_darwin.go) is likewise a var so tests can
// redirect the plist path away from the root-only /Library/LaunchDaemons.
var geteuidFn = os.Geteuid

func newManager() Manager { return darwinManager{} }

// Installed reports whether the system LaunchDaemon plist is present.
// Used by `waired init` to decide whether auto-starting the agent is
// possible (vs a raw-binary dev run).
func Installed() bool {
	_, statErr := os.Stat(systemLaunchDaemonPath(darwinLabel))
	return statErr == nil
}

// StartHint is the manual command shown when init cannot (or is told not
// to) auto-start the agent. The system domain needs root, so the hint
// carries sudo.
func StartHint() string {
	return "sudo launchctl kickstart -k system/" + darwinLabel
}

// FixStateOwnership is a no-op on macOS: the system LaunchDaemon runs as
// root, which can read every file under the (root-owned) system state
// dir regardless of who created it. There is no root-vs-service-user
// split to reconcile (contrast Linux's User=waired, which needs a
// chown-back — #335/#484).
func FixStateOwnership(string) error { return nil }

// osDispatchInteractive: launchd hands the daemon a normal foreground
// process — there is no equivalent to Windows's SCM dispatcher. The
// agent reads SIGTERM from `launchctl kill SIGTERM` and exits via the
// usual signal.NotifyContext path.
func osDispatchInteractive(_ []string, _ RunHook) (bool, int) {
	return false, 0
}

type darwinManager struct{}

func (m darwinManager) Install(cfg Config) error {
	if geteuidFn() != 0 {
		return errors.New("install: registering a system LaunchDaemon under " +
			systemDaemonDir + " requires root — re-run with sudo")
	}
	plistPath := systemLaunchDaemonPath(darwinLabel)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(plistPath), err)
	}

	body, err := renderLaunchDaemonPlist(cfg)
	if err != nil {
		return fmt.Errorf("render plist: %w", err)
	}

	// launchd chdir()s into WorkingDirectory (= cfg.StateDir, set by
	// renderLaunchDaemonPlist) before exec, and the plist sets RunAtLoad.
	// If the state dir does not exist yet — e.g. an installer run with
	// --no-init, before `waired init` has created it — launchd cannot
	// chdir into it, fails the job with EX_CONFIG (78), and KeepAlive
	// crash-loops it (mgmt API never comes up). Create it up front so the
	// agent starts regardless of init ordering. 0o755 matches `waired
	// init`'s own state-dir creation (cmd/waired/main.go). renderLaunchDaemonPlist
	// above guarantees cfg.StateDir is non-empty.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir %s: %w", cfg.StateDir, err)
	}

	// Best-effort: tear down any pre-existing per-user LaunchAgent from an
	// older build (the model #520 replaced). Idempotent and harmless on a
	// fresh install; only meaningful for a host upgraded across the switch.
	bootoutLegacyPerUserAgent()

	if err := os.WriteFile(plistPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}

	// `launchctl bootstrap` loads + registers the job in the system
	// domain. Idempotent failure mode: bootstrap returns exit 17 if the
	// job is already loaded, so we bootout first (best-effort) and
	// re-bootstrap.
	_, _, _ = runLaunchctlFn([]string{"bootout", "system/" + darwinLabel})
	if _, stderr, err := runLaunchctlFn([]string{
		"bootstrap", "system", plistPath,
	}); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w (stderr=%q)", err, truncate(stderr))
	}

	// `launchctl enable` flips the "should launchd auto-start this at
	// boot" bit, separate from bootstrap which only loads it into the
	// current boot. Without this, RunAtLoad fires today but the agent
	// will not come back after a reboot.
	if _, stderr, err := runLaunchctlFn([]string{
		"enable", "system/" + darwinLabel,
	}); err != nil {
		return fmt.Errorf("launchctl enable: %w (stderr=%q)", err, truncate(stderr))
	}
	return nil
}

func (m darwinManager) Uninstall() error {
	plistPath := systemLaunchDaemonPath(darwinLabel)

	// Best-effort sequence — every step tolerated so a partial install
	// (plist written, never bootstrapped, etc.) can still be cleaned.
	_, _, _ = runLaunchctlFn([]string{"bootout", "system/" + darwinLabel})
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", plistPath, err)
	}
	return nil
}

func (m darwinManager) Start(extraArgs []string) error {
	if len(extraArgs) > 0 {
		// Same rationale as Linux: ExecStart equivalent (the plist's
		// ProgramArguments) is fixed at install time, so refusing
		// extra args here saves the caller from a silent ignore.
		return fmt.Errorf("Start: extra args not supported on darwin (the plist's ProgramArguments is fixed at install time)")
	}
	// `kickstart -k` kills any running instance and restarts. Without
	// -k it would only start if the job is currently stopped, which
	// is the wrong behaviour for `service start` after a config
	// reinstall.
	if _, stderr, err := runLaunchctlFn([]string{
		"kickstart", "-k", "system/" + darwinLabel,
	}); err != nil {
		return fmt.Errorf("launchctl kickstart: %w (stderr=%q)", err, truncate(stderr))
	}
	return nil
}

func (m darwinManager) Stop() error {
	// Send SIGTERM and let the agent's signal.NotifyContext path
	// handle shutdown. `launchctl stop` would be cleaner but it
	// also tries to re-launch under KeepAlive, which we want for
	// `kickstart` but not here.
	if _, stderr, err := runLaunchctlFn([]string{
		"kill", "SIGTERM", "system/" + darwinLabel,
	}); err != nil {
		return fmt.Errorf("launchctl kill SIGTERM: %w (stderr=%q)", err, truncate(stderr))
	}
	return nil
}

// renderLaunchDaemonPlist emits the plist body for the waired-agent
// system LaunchDaemon. We hand-build the XML rather than using a plist
// library because the schema is tiny and avoiding a dep keeps go.sum
// small.
//
// Notable choices:
//   - No UserName key, so the job runs as root (the macOS analog of
//     Windows LocalSystem). Run-as identity rationale is at the top of
//     this file.
//   - RunAtLoad=true so the agent starts the moment launchctl
//     bootstrap finishes (and again on every boot).
//   - KeepAlive {SuccessfulExit=false} so a clean exit (the user
//     uninstalled, or the daemon hit a graceful "config invalid, refuse
//     to start" path) does not flap the agent, but any crash brings it
//     back.
//   - ProcessType=Background tells App Nap to leave us alone — the
//     agent is doing useful overlay-routing work even when no UI is
//     visible.
//   - StandardOutPath / StandardErrorPath under /Library/Logs (a
//     system location, since the daemon runs as root) so a tail-able
//     file makes triage easier and matches systemd's `journalctl -u`
//     ergonomic.
//   - EnvironmentVariables{HOME=StateDir}: launchd exports no $HOME to a
//     system daemon (systemd derives one from User=), so subprocesses
//     that resolve ~ die — `ollama serve` aborted with "$HOME is not
//     defined" (#22). This closes the launch-environment parity gap so
//     every spawned process inherits a writable HOME.
func renderLaunchDaemonPlist(cfg Config) ([]byte, error) {
	if cfg.Binary == "" {
		return nil, errors.New("renderLaunchDaemonPlist: cfg.Binary is required")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("renderLaunchDaemonPlist: cfg.StateDir is required")
	}
	args := []string{cfg.Binary, "--state-dir=" + cfg.StateDir}
	if cfg.MgmtAddr != "" {
		args = append(args, "--mgmt="+cfg.MgmtAddr)
	}
	args = append(args, cfg.ExtraArgs...)

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	writeKeyString(&b, "Label", darwinLabel)

	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range args {
		b.WriteString("    <string>")
		_ = xml.EscapeText(&b, []byte(a))
		b.WriteString("</string>\n")
	}
	b.WriteString("  </array>\n")

	writeKeyBool(&b, "RunAtLoad", true)

	b.WriteString("  <key>KeepAlive</key>\n  <dict>\n")
	writeKeyBool(&b, "SuccessfulExit", false)
	writeKeyBool(&b, "Crashed", true)
	b.WriteString("  </dict>\n")

	writeKeyString(&b, "ProcessType", "Background")
	writeKeyString(&b, "WorkingDirectory", cfg.StateDir)
	writeKeyString(&b, "StandardOutPath", "/Library/Logs/waired-agent.out.log")
	writeKeyString(&b, "StandardErrorPath", "/Library/Logs/waired-agent.err.log")

	// #22: launchd (unlike systemd's User=, which derives $HOME from the
	// service user's passwd entry) exports NO $HOME to a system daemon.
	// Subprocesses that resolve ~ then die — `ollama serve` aborted at
	// startup with "$HOME is not defined". Give the daemon, and thus every
	// process it spawns, a writable HOME = the state dir (already its
	// WorkingDirectory), the macOS analog of the HOME systemd provides.
	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	writeKeyString(&b, "HOME", cfg.StateDir)
	b.WriteString("  </dict>\n")

	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func writeKeyString(b *bytes.Buffer, key, value string) {
	b.WriteString("  <key>")
	_ = xml.EscapeText(b, []byte(key))
	b.WriteString("</key>\n  <string>")
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString("</string>\n")
}

func writeKeyBool(b *bytes.Buffer, key string, value bool) {
	b.WriteString("  <key>")
	_ = xml.EscapeText(b, []byte(key))
	b.WriteString("</key>\n  ")
	if value {
		b.WriteString("<true/>\n")
	} else {
		b.WriteString("<false/>\n")
	}
}

// systemLaunchDaemonPath returns /Library/LaunchDaemons/<label>.plist.
// systemDaemonDir is a package var (proxy_dropin_darwin.go) so tests can
// point it at a temp dir; the real path requires root to write.
func systemLaunchDaemonPath(label string) string {
	return filepath.Join(systemDaemonDir, label+".plist")
}

// bootoutLegacyPerUserAgent best-effort removes the pre-#520 per-user
// LaunchAgent so an upgraded host does not end up running two agents.
// The old job lived in the invoking user's gui/<uid> domain with its
// plist under that user's ~/Library/LaunchAgents. We are root here
// (Install enforces it), so we resolve the human user from $SUDO_USER.
// Every step is ignored on error: a fresh install has nothing to clean,
// and a residual job must never block the new daemon's registration.
func bootoutLegacyPerUserAgent() {
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return
	}
	u, err := user.Lookup(name)
	if err != nil {
		return
	}
	if uid, err := strconv.Atoi(u.Uid); err == nil {
		_, _, _ = runLaunchctlFn([]string{"bootout", fmt.Sprintf("gui/%d/%s", uid, darwinLabel)})
	}
	if u.HomeDir != "" {
		_ = os.Remove(filepath.Join(u.HomeDir, "Library", "LaunchAgents", darwinLabel+".plist"))
	}
}

// runLaunchctlReal forks /bin/launchctl with the supplied argv and
// returns stdout/stderr/err. Tests inject a fake via runLaunchctlFn.
func runLaunchctlReal(args []string) ([]byte, []byte, error) {
	cmd := exec.Command("/bin/launchctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func truncate(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
