//go:build linux

package trayhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// GNOME AppIndicator host extension identifiers. GNOME has no built-in SNI
// host, so the waired-tray icon only renders when one of these is installed and
// enabled. We install the cross-Debian/Ubuntu upstream package and enable its
// UUID; Ubuntu Desktop ships its own (ubuntu-appindicators, pulled in by
// ubuntu-desktop via gnome-shell-ubuntu-extensions and enabled through the
// `ubuntu` session mode in /usr/share/gnome-shell/modes/ubuntu.json), which the
// installed-check below treats as "nothing to do".
const (
	appIndicatorPackage    = "gnome-shell-extension-appindicator"
	appIndicatorEnableUUID = "appindicatorsupport@rgcjonas.gmail.com"
)

var appIndicatorUUIDs = []string{
	appIndicatorEnableUUID,
	"ubuntu-appindicators@ubuntu.com",
}

// GatherRepairFacts completes a Check result into the inputs PlanRepair wants.
// It takes the Result rather than calling Check itself so a caller that already
// probed the session bus does not pay for a second D-Bus round trip.
func GatherRepairFacts(r Result) RepairFacts {
	return RepairFacts{
		Status:           r.Status,
		Desktop:          r.Desktop,
		ExtensionPresent: extensionInstalled(repairCheckHome()),
		GnomeShellOnPath: onPath(exec.LookPath("gnome-shell")),
		AptOnPath:        onPath(exec.LookPath("apt-get")),
	}
}

// Plan is the convenience pairing of the two: probe, then decide.
func Plan(r Result) RepairAction { return PlanRepair("linux", GatherRepairFacts(r)) }

// onPath adapts an exec.LookPath result to a bool. The LookPath calls stay
// spelled out with literal binary names at each site so
// scripts/ci/lookpathguard keeps reading as a list of what this repo asks $PATH
// about — a `lookPathOK(name)` wrapper would collapse them all into one
// undeclarable "name" entry.
func onPath(_ string, err error) bool { return err == nil }

// extensionInstalled reports whether any known AppIndicator host extension is
// already present, system-wide or for the user. Directory presence is a good
// proxy: on Ubuntu Desktop ubuntu-appindicators ships system-wide, so this
// returns true and the plan degrades to enable-only (or to nothing at all,
// because the session mode already enabled it).
func extensionInstalled(home string) bool {
	bases := []string{"/usr/share/gnome-shell/extensions"}
	if home != "" {
		bases = append(bases, filepath.Join(home, ".local", "share", "gnome-shell", "extensions"))
	}
	for _, base := range bases {
		for _, uuid := range appIndicatorUUIDs {
			if st, err := os.Stat(filepath.Join(base, uuid)); err == nil && st.IsDir() {
				return true
			}
		}
	}
	return false
}

// repairCheckHome resolves whose per-user extension dir to inspect: the
// sudo-invoking user under sudo (their session is the one missing an icon),
// else this process's home.
func repairCheckHome() string {
	if u, ok := desktopUser(); ok {
		if h, err := userHome(u); err == nil {
			return h
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

// desktopUser names the user whose GNOME session the repair targets when this
// process is root-but-invoked-via-sudo. Empty/false means "this process's own
// user", which is the normal `waired doctor` and waired-tray case.
func desktopUser() (string, bool) {
	if os.Geteuid() != 0 {
		return "", false
	}
	u := os.Getenv("SUDO_USER")
	if u == "" || u == "root" {
		return "", false
	}
	return u, true
}

func userHome(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// Install installs the AppIndicator host extension with apt. Callers must only
// reach this via a RepairInstallThenEnable plan, which guarantees gnome-shell is
// already on the host — see PlanRepair for why that guarantee is what keeps apt
// from dragging a desktop onto a server.
//
// apt needs root: when this process is not root the command is re-run through
// sudo, which prompts on the caller's terminal. Progress streams to out.
func Install(ctx context.Context, out io.Writer) error {
	argv := []string{"apt-get", "install", "-y", appIndicatorPackage}
	if os.Geteuid() != 0 {
		if !onPath(exec.LookPath("sudo")) {
			return fmt.Errorf("installing %s needs root and sudo is not available; run: apt-get install -y %s",
				appIndicatorPackage, appIndicatorPackage)
		}
		argv = append([]string{"sudo"}, argv...)
	}
	_, _ = fmt.Fprintf(out, "  running: %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	cmd.Stdin = os.Stdin // sudo may need to prompt for a password
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// Enable enables the upstream AppIndicator extension for the desktop user.
//
// This is a per-user dconf write and needs no privilege — which is what lets
// waired-tray repair a merely-disabled extension silently at session start. The
// one wrinkle is the reverse of Install's: when this process IS root (a
// `sudo waired doctor --fix`), writing our own dconf would enable the extension
// for root's session, not the user's, so we hop to SUDO_USER and hand them the
// session-bus address for their own /run/user/<uid>.
func Enable(ctx context.Context) error {
	if !onPath(exec.LookPath("gnome-extensions")) {
		return fmt.Errorf("gnome-extensions is not on PATH; enable it by hand: gnome-extensions enable %s",
			appIndicatorEnableUUID)
	}
	username, hop := desktopUser()
	if !hop {
		cmd := exec.CommandContext(ctx, "gnome-extensions", "enable", appIndicatorEnableUUID)
		if outBytes, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("gnome-extensions enable: %w: %s", err, strings.TrimSpace(string(outBytes)))
		}
		return nil
	}
	return enableAsUser(ctx, username)
}

// enableAsUser runs the enable in username's session. runuser (util-linux,
// Essential on Debian) is preferred — it resolves the user via NSS and needs no
// sudoers entry; `sudo -u <user> -H` is the fallback. The child gets a minimal
// env plus the session-bus coordinates, rather than root's environment, so
// nothing of root's leaks into the user's session.
func enableAsUser(ctx context.Context, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", username, err)
	}
	runtimeDir := filepath.Join("/run/user", u.Uid)
	inner := []string{
		"env",
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(runtimeDir, "bus"),
		"gnome-extensions", "enable", appIndicatorEnableUUID,
	}

	var argv []string
	switch {
	case onPath(exec.LookPath("runuser")):
		argv = append([]string{"runuser", "-u", username, "--"}, inner...)
	case onPath(exec.LookPath("sudo")):
		argv = append([]string{"sudo", "-u", username, "-H", "--"}, inner...)
	default:
		return fmt.Errorf("no runuser or sudo to enable the extension as %s; run as that user: gnome-extensions enable %s",
			username, appIndicatorEnableUUID)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + u.HomeDir,
		"USER=" + username,
	}
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gnome-extensions enable as %s: %w: %s",
			username, err, strings.TrimSpace(string(outBytes)))
	}
	return nil
}

// RepairPackage and RepairUUID expose the identifiers for callers that only
// want to print them (the doctor's manual-fix wording).
func RepairPackage() string { return appIndicatorPackage }
func RepairUUID() string    { return appIndicatorEnableUUID }
