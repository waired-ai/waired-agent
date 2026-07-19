//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

func newManager() Manager { return &linuxManager{} }

// osDispatchInteractive is a no-op on Linux — there is no equivalent
// to Windows's SCM auto-detection. The daemon is always started by
// systemd as a foreground process, and signal.NotifyContext in main()
// catches SIGTERM from `systemctl stop`.
func osDispatchInteractive(_ []string, _ RunHook) (bool, int) {
	return false, 0
}

// linuxManager generates a systemd unit at install time and shells out
// to systemctl for daemon-reload / enable / start / stop. The unit is
// written to /etc/systemd/system/<ServiceName>.service.
type linuxManager struct{}

const (
	linuxUnitDir    = "/etc/systemd/system"
	linuxEnvFileDir = "/etc/waired"
	// LinuxEnvFilePath is the systemd EnvironmentFile the installer writes
	// (e.g. WAIRED_CONTROL_URL=...). Exported so `waired init` can read the
	// installer-configured control URL from a single source of truth.
	LinuxEnvFilePath = "/etc/waired/agent.env"
	// linuxServiceUser is the unprivileged user the systemd unit runs as
	// (User=/Group= in the rendered unit). The daemon reads identity /
	// secrets from the state dir as this user, so files written by a root
	// `sudo waired init` must be chowned to it (see FixStateOwnership).
	linuxServiceUser = "waired"
)

func (m *linuxManager) unitPath() string {
	return filepath.Join(linuxUnitDir, ServiceName+".service")
}

// linuxUnitDirs are every directory a waired-agent unit may live in. The
// CLI-managed install (`waired service install`) writes to linuxUnitDir
// (/etc/systemd/system), but the .deb ships the unit to /lib/systemd/system
// (packaging/nfpm/waired.yaml.tmpl), which on merged-/usr distros resolves
// under /usr/lib/systemd/system. Installed() must check all of them:
// checking only /etc made it return false on every .deb install, so
// FixStateOwnership no-op'd and a root `sudo waired init` left the
// identity/secrets root-owned — the daemon (User=waired) then could not
// read them and stayed unenrolled (the #335 failure mode). A package var so
// tests can point it at a temp dir.
var linuxUnitDirs = []string{
	linuxUnitDir,              // /etc/systemd/system — CLI `waired service install`
	"/lib/systemd/system",     // .deb (nfpm dst)
	"/usr/lib/systemd/system", // .deb on merged-/usr distros
}

// Installed reports whether the systemd unit file is present (i.e. the
// agent was installed via .deb / `waired service install` rather than run
// as a raw binary). Used by `waired init` to decide whether auto-starting
// the service is even possible and by FixStateOwnership to decide whether a
// service user owns the state dir.
func Installed() bool {
	for _, dir := range linuxUnitDirs {
		if _, err := os.Stat(filepath.Join(dir, ServiceName+".service")); err == nil {
			return true
		}
	}
	return false
}

// StartHint is the manual command shown when init cannot (or is told not
// to) auto-start the agent.
func StartHint() string { return "sudo systemctl start " + ServiceName }

// FixStateOwnership chowns the state-dir tree to the systemd service user
// so the unprivileged waired-agent daemon (User=waired) can read the
// identity / secrets / agent.json that a root `sudo waired init` just
// wrote. Without this the files are root-owned and the daemon — though
// the device is enrolled at the Control Plane — never sees the identity
// and stays unenrolled. No-op (returns nil) unless we are root and the
// systemd unit is actually installed; in the raw-binary / non-root dev
// case the state dir is already owned by the running user.
func FixStateOwnership(stateDir string) error {
	if os.Geteuid() != 0 || !Installed() {
		return nil
	}
	return chownRecursive(stateDir, linuxServiceUser)
}

func (m *linuxManager) Install(cfg Config) error {
	user := cfg.User
	if user == "" {
		user = linuxServiceUser
	}

	// 1. Ensure the service user exists.
	if err := ensureSystemUser(user, cfg.StateDir); err != nil {
		return fmt.Errorf("ensure user %q: %w", user, err)
	}

	// 2. State dir + secrets subdir, owned by the service user.
	if err := secrets.SecureDir(cfg.StateDir); err != nil {
		return err
	}
	if err := secrets.SecureDir(filepath.Join(cfg.StateDir, "secrets")); err != nil {
		return err
	}
	if err := chownRecursive(cfg.StateDir, user); err != nil {
		return fmt.Errorf("chown %s: %w", cfg.StateDir, err)
	}

	// 3. /etc/waired/agent.env stub (EnvironmentFile=-… makes systemd
	//    tolerate it being missing, but having a placeholder makes
	//    `journalctl` happier and gives the operator something to grep).
	if err := os.MkdirAll(linuxEnvFileDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", linuxEnvFileDir, err)
	}
	if _, err := os.Stat(LinuxEnvFilePath); os.IsNotExist(err) {
		if err := os.WriteFile(LinuxEnvFilePath,
			[]byte("# Optional overrides for waired-agent. Uncomment to set.\n"+
				"# WAIRED_CONTROL_URL=https://control.example.com\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", LinuxEnvFilePath, err)
		}
	}

	// 4. Generate + write the unit.
	unit := renderSystemdUnit(cfg, user)
	if err := os.WriteFile(m.unitPath(), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", m.unitPath(), err)
	}

	// 5. Reload + enable. Start is left for the operator (or `service start`).
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", ServiceName); err != nil {
		return err
	}
	return nil
}

func (m *linuxManager) Uninstall() error {
	// Best-effort stop + disable; tolerate errors so a half-installed
	// unit can still be cleaned up.
	_ = runSystemctl("stop", ServiceName)
	_ = runSystemctl("disable", ServiceName)
	if err := os.Remove(m.unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", m.unitPath(), err)
	}
	return runSystemctl("daemon-reload")
}

func (m *linuxManager) Start(extraArgs []string) error {
	if len(extraArgs) > 0 {
		// systemd does not accept arbitrary args at `systemctl start`
		// time — the unit's ExecStart is the source of truth. Refuse
		// up-front so the user knows the flag was ignored.
		return fmt.Errorf("Start: extra args not supported on Linux (the unit's ExecStart is fixed at install time)")
	}
	return runSystemctl("start", ServiceName)
}

func (m *linuxManager) Stop() error {
	return runSystemctl("stop", ServiceName)
}

// renderSystemdUnit produces the .service body. Mirrors
// build/waired-agent.service but inlines the binary path / state dir
// / user from cfg rather than relying on the bootstrap wrapper.
func renderSystemdUnit(cfg Config, user string) string {
	exec := cfg.Binary + " --state-dir=" + cfg.StateDir
	if cfg.MgmtAddr != "" {
		exec += " --mgmt=" + cfg.MgmtAddr
	}
	for _, a := range cfg.ExtraArgs {
		exec += " " + a
	}
	var b strings.Builder
	fmt.Fprintln(&b, "[Unit]")
	fmt.Fprintln(&b, "Description="+Description)
	fmt.Fprintln(&b, "After=network-online.target")
	fmt.Fprintln(&b, "Wants=network-online.target")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Service]")
	fmt.Fprintln(&b, "Type=simple")
	fmt.Fprintln(&b, "ExecStart="+exec)
	fmt.Fprintln(&b, "Restart=always")
	fmt.Fprintln(&b, "RestartSec=5s")
	// Restart=always is required for the management API's "click →
	// SIGTERM → restart with new config" flow. See
	// build/waired-agent.service for the long-form explanation.
	fmt.Fprintln(&b, "User="+user)
	fmt.Fprintln(&b, "Group="+user)
	fmt.Fprintln(&b, "EnvironmentFile=-"+LinuxEnvFilePath)
	fmt.Fprintln(&b, "ReadWritePaths="+cfg.StateDir)
	// RuntimeDirectory creates /run/waired (owned by User=, mode 0755) for
	// the Local Management API write socket (waired#838), adds it to the
	// service's writable set under ProtectSystem=strict, and removes it on
	// stop (stale-socket cleanup). The desktop-user tray/CLI traverse the
	// 0755 dir to reach the 0666 socket the daemon binds there.
	fmt.Fprintln(&b, "RuntimeDirectory=waired")
	fmt.Fprintln(&b, "RuntimeDirectoryMode=0755")
	fmt.Fprintln(&b, "ProtectSystem=strict")
	fmt.Fprintln(&b, "ProtectHome=yes")
	fmt.Fprintln(&b, "NoNewPrivileges=yes")
	fmt.Fprintln(&b, "PrivateTmp=yes")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Install]")
	fmt.Fprintln(&b, "WantedBy=multi-user.target")
	return b.String()
}

func ensureSystemUser(name, home string) error {
	if _, err := exec.LookPath("getent"); err == nil {
		if err := exec.Command("getent", "passwd", name).Run(); err == nil {
			return nil // already exists
		}
	}
	useradd, err := exec.LookPath("useradd")
	if err != nil {
		// Not fatal — many minimal containers lack useradd; the
		// operator can pre-create the user out-of-band.
		fmt.Fprintf(os.Stderr, "warning: useradd not found; assuming %q already exists\n", name)
		return nil
	}
	out, err := exec.Command(useradd,
		"--system",
		"--home-dir", home,
		"--shell", "/usr/sbin/nologin",
		name,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func chownRecursive(path, user string) error {
	chown, err := exec.LookPath("chown")
	if err != nil {
		return fmt.Errorf("chown not found: %w", err)
	}
	out, err := exec.Command(chown, "-R", user+":"+user, path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown -R %s %s: %w: %s", user, path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runSystemctl(args ...string) error {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return fmt.Errorf("systemctl not found: %w", err)
	}
	out, err := exec.Command(systemctl, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
