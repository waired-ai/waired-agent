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
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// Bounds for the bootout settle loop below. Variables so a test can
// shorten them; the loop's exit condition is the observable state, not
// the clock, so these only cap how long a wedged launchd can stall us.
var (
	launchdSettleTimeout = 10 * time.Second
	launchdSettlePoll    = 50 * time.Millisecond
)

// bootoutAndSettle removes the job from the given launchd domain target
// and waits until launchd has actually finished doing it.
//
// `launchctl bootout` is ASYNCHRONOUS: it returns as soon as the removal
// is REQUESTED, and the job lingers in the domain for a moment after.
// A `bootstrap` issued into that window fails, so `waired-agent uninstall`
// immediately followed by `waired-agent install` — a repair, the most
// ordinary reason anyone reinstalls — failed with the installer reporting
// exit 1 and the host left with no daemon at all. The uninstall→reinstall
// leg added in #195 caught it on its first CI run; nothing before that ever
// reinstalled on a host that had just uninstalled.
//
// The wait is a poll of the observable state rather than a sleep: for a
// registered job `launchctl print <target>` succeeds, and once launchd has
// torn it down it fails. That makes this a barrier (a happens-before on the
// teardown) instead of a guess about how long teardown takes — the same
// correction #144 made to the gateway release path.
//
// Best-effort throughout: a target that was never loaded fails the first
// probe and returns immediately, and a timeout returns rather than erroring
// so the caller's own bootstrap reports the real failure.
func bootoutAndSettle(target string) {
	_, _, _ = runLaunchctlFn([]string{"bootout", target})

	deadline := time.Now().Add(launchdSettleTimeout)
	for {
		if _, _, err := runLaunchctlFn([]string{"print", target}); err != nil {
			return // gone from the domain
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(launchdSettlePoll)
	}
}

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
// At-rest, the secrets are 0600 files in a root-owned 0700 state dir,
// which is what a non-root local account cannot reach; beyond that they
// are only as protected as the disk is. See #520.
//
// The tray stays a per-user LaunchAgent (internal/platform/autostart) —
// it is a menu-bar GUI app — and reaches this daemon over the loopback
// management API.

const (
	// darwinLabel is the launchd job label, used both as the plist's
	// <key>Label</key> value and the suffix on `launchctl ... system/<label>`.
	darwinLabel = DarwinLabel
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
	// agent starts regardless of init ordering. renderLaunchDaemonPlist
	// above guarantees cfg.StateDir is non-empty.
	//
	// SecureDir creates it 0700 (owner-only), the macOS analog of the
	// Linux service's secrets.SecureDir (service_linux.go) and the Windows
	// restrictive DACL: the daemon runs as root and is the only reader, so
	// there is no reason to leave agent.json / identity.json world-readable
	// under /Library/Application Support/waired. No chown is needed (root
	// owns it; FixStateOwnership is a no-op on darwin for the same reason).
	if err := secrets.SecureDir(cfg.StateDir); err != nil {
		return fmt.Errorf("secure state dir %s: %w", cfg.StateDir, err)
	}

	// Best-effort: tear down any pre-existing per-user LaunchAgent from an
	// older build (the model #520 replaced). Idempotent and harmless on a
	// fresh install; only meaningful for a host upgraded across the switch.
	bootoutLegacyPerUserAgent()

	if err := os.WriteFile(plistPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}

	// Self-heal, and it MUST come before bootstrap. Builds between
	// 2026-07-15 and #176 ran `launchctl disable` on uninstall, which does
	// not "clear" anything — it WRITES a persistent per-label override into
	// /var/db/com.apple.xpc.launchd/disabled.plist. That override outlives
	// the plist, the state dir, `uninstall.sh --clean` and a reboot, and
	// `bootstrap` fails with EIO(5) on a disabled label, so the enable below
	// the bootstrap could never be reached. `enable` is the only call that
	// clears it. Best-effort: a no-op on a host that was never disabled.
	_, _, _ = runLaunchctlFn([]string{"enable", "system/" + darwinLabel})

	// `launchctl bootstrap` loads + registers the job in the system
	// domain. Idempotent failure mode: bootstrap returns exit 17 if the
	// job is already loaded, so we bootout first (best-effort) and
	// re-bootstrap — and WAIT for that bootout, because an unsettled one
	// makes the bootstrap below fail. See bootoutAndSettle.
	bootoutAndSettle("system/" + darwinLabel)
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
	//
	// Deliberately NO `launchctl disable` here (#176). It reads like the
	// macOS analog of Linux Uninstall's `systemctl disable`, but the two are
	// not analogous: `systemctl disable` removes symlinks, and the unit file
	// is deleted on the next line anyway, whereas `launchctl disable` writes
	// a PERSISTENT per-label entry into launchd's disabled DB that outlives
	// everything this function removes — plist, state dir, `--clean`, reboot
	// — and permanently breaks the `bootstrap` in Install with EIO(5).
	// Nothing here should leave state behind that Uninstall cannot remove.
	//
	// Settled, not just requested: on return this function promises the job
	// is gone, and callers act on that promise — `waired-agent uninstall`
	// followed by `waired-agent install` is the ordinary repair, and it
	// failed while the teardown was still in flight. See bootoutAndSettle.
	bootoutAndSettle("system/" + darwinLabel)
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
//
//   - No UserName key, so the job runs as root (the macOS analog of
//     Windows LocalSystem). Run-as identity rationale is at the top of
//     this file.
//
//   - RunAtLoad=true so the agent starts the moment launchctl
//     bootstrap finishes (and again on every boot).
//
//   - KeepAlive {SuccessfulExit=false} so a clean exit (the user
//     uninstalled, or the daemon hit a graceful "config invalid, refuse
//     to start" path) does not flap the agent, but any crash brings it
//     back.
//
//     This is also what honours RestartRequestedExitCode (#684): exit 17
//     is non-zero, so launchd restarts the job, and a preferred-model
//     switch completes on macOS the same way it does under systemd. What
//     it CANNOT do is tell 17 apart from a crash — KeepAlive is a dict of
//     conditions (SuccessfulExit, Crashed, NetworkState, PathState,
//     OtherJobEnabled) with no per-exit-code key — so an intentional
//     restart and a fault look identical to launchd, share the same
//     ThrottleInterval, and read the same in `launchctl print`.
//     Distinguishing them would mean moving the decision out of launchd
//     into the process. RestartOnExitFor("darwin") states this, and one
//     table test on the Linux leg keeps all three OSes' answers in view.
//
//   - ProcessType=Background tells App Nap to leave us alone — the
//     agent is doing useful overlay-routing work even when no UI is
//     visible.
//
//   - StandardOutPath / StandardErrorPath under /Library/Logs (a
//     system location, since the daemon runs as root) so a tail-able
//     file makes triage easier and matches systemd's `journalctl -u`
//     ergonomic.
//
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
	// The constants come from internal/platform/logrotate because the
	// daemon rotates these two files itself (#331). One definition, so
	// the rotator can never be left watching a path this plist stopped
	// using.
	writeKeyString(&b, "StandardOutPath", logrotate.AgentOutPath)
	writeKeyString(&b, "StandardErrorPath", logrotate.AgentErrPath)

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
