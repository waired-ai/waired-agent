//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"
)

// GNOME AppIndicator host extension identifiers. GNOME has no built-in SNI host
// (issue #493), so the waired-tray icon only renders when one of these is
// installed and enabled. We install the cross-Debian/Ubuntu upstream package and
// enable its UUID; Ubuntu Desktop ships its own (ubuntu-appindicators) already
// enabled, which the installed-check below treats as "nothing to do".
const (
	appIndicatorPackage    = "gnome-shell-extension-appindicator"
	appIndicatorEnableUUID = "appindicatorsupport@rgcjonas.gmail.com"
)

var appIndicatorUUIDs = []string{
	appIndicatorEnableUUID,
	"ubuntu-appindicators@ubuntu.com",
}

// trayExtFacts are the inputs decideTrayExtAction acts on. Split out so the
// decision is pure and unit-tested without touching the host or running apt.
type trayExtFacts struct {
	trayBinaryPresent  bool // waired-tray on $PATH (deb or tarball install)
	gnomeShellPresent  bool // this is a GNOME host
	extensionInstalled bool // an AppIndicator host extension is already present
	aptPresent         bool // apt-get is available to install one
}

type trayExtAction int

const (
	trayExtSkip       trayExtAction = iota // nothing to do (not GNOME / no tray / already fine)
	trayExtInstall                         // apt install + enable
	trayExtManualHint                      // GNOME + missing but no apt: print a hint only
)

// decideTrayExtAction is the pure decision behind ensureTrayHostExtension.
func decideTrayExtAction(f trayExtFacts) trayExtAction {
	// No tray to host, not GNOME, or already covered → leave the host alone.
	// Notably, a default Ubuntu Desktop already ships ubuntu-appindicators, so
	// extensionInstalled short-circuits here and init stays a silent no-op.
	if !f.trayBinaryPresent || !f.gnomeShellPresent || f.extensionInstalled {
		return trayExtSkip
	}
	if !f.aptPresent {
		return trayExtManualHint
	}
	return trayExtInstall
}

// ensureTrayHostExtension auto-installs (and enables) a GNOME AppIndicator host
// extension on a fresh `waired init` so the waired-tray SNI icon renders (#493).
// It runs as root, where the dpkg lock is free and `apt install` is valid — a
// deb postinst cannot do this because dpkg holds the lock during install. Every
// step is best-effort and prints what it does; failures degrade to a hint and
// never fail init. No-op on non-GNOME, when an extension already exists, or when
// there is no waired-tray on the host.
func ensureTrayHostExtension(out io.Writer) {
	sudoUser, isSudo := invokingSudoUser()
	home := trayExtCheckHome(sudoUser, isSudo)

	switch decideTrayExtAction(gatherTrayExtFacts(home)) {
	case trayExtSkip:
		return
	case trayExtManualHint:
		_, _ = fmt.Fprintf(out, "\n%s GNOME detected but no AppIndicator tray host extension (and apt-get is unavailable).\n",
			emo("🖥️", "*"))
		_, _ = fmt.Fprintf(out, "   Install one so the waired-tray icon renders, e.g. the %q package, then log out and back in.\n",
			appIndicatorPackage)
	case trayExtInstall:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, _ = fmt.Fprintf(out, "\n%s Installing the GNOME AppIndicator extension so the waired-tray icon renders…\n",
			emo("🖥️", "*"))
		if err := aptInstallAppIndicator(ctx, out); err != nil {
			_, _ = fmt.Fprintf(out, "   warn: install failed (%v); install it manually: sudo apt install %s\n",
				err, appIndicatorPackage)
			return
		}
		enableTarget := sudoUser
		if !isSudo {
			enableTarget = "" // enable for the current (non-sudo) user
		}
		enableAppIndicatorExtension(ctx, enableTarget)
		_, _ = fmt.Fprintf(out, "   Installed and enabled. Log out and back in (required on Wayland) to load it and show the tray icon.\n")
	}
}

// gatherTrayExtFacts probes the host. home is the user whose per-user extension
// dir is checked (the sudo-invoking user under sudo, else the process home).
func gatherTrayExtFacts(home string) trayExtFacts {
	return trayExtFacts{
		trayBinaryPresent:  lookPathOK("waired-tray"),
		gnomeShellPresent:  lookPathOK("gnome-shell"),
		extensionInstalled: appIndicatorInstalled(home),
		aptPresent:         lookPathOK("apt-get"),
	}
}

func lookPathOK(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// appIndicatorInstalled reports whether any known AppIndicator host extension is
// already present, system-wide or for the user. Directory presence is a good
// proxy: on Ubuntu Desktop ubuntu-appindicators ships system-wide and enabled,
// so this returns true and init does nothing.
func appIndicatorInstalled(home string) bool {
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

// trayExtCheckHome resolves the home whose per-user extension dir to inspect.
func trayExtCheckHome(sudoUser string, isSudo bool) string {
	if isSudo {
		if h, err := sudoUserHome(sudoUser); err == nil {
			return h
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

func aptInstallAppIndicator(ctx context.Context, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "apt-get", "install", "-y", appIndicatorPackage)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// enableAppIndicatorExtension enables the upstream AppIndicator extension via the
// gnome-extensions CLI. When username is non-empty (the sudo-invoking user) it
// hops to that user — runuser/sudo plus the session env (XDG_RUNTIME_DIR /
// DBUS_SESSION_BUS_ADDRESS for /run/user/<uid>) so the CLI can reach the user's
// gnome-shell. Best-effort: a freshly-installed extension may need a shell
// re-login before it loads, which the caller already tells the user to do.
func enableAppIndicatorExtension(ctx context.Context, username string) {
	if username == "" {
		cmd := exec.CommandContext(ctx, "gnome-extensions", "enable", appIndicatorEnableUUID)
		_ = cmd.Run()
		return
	}

	inner := []string{"gnome-extensions", "enable", appIndicatorEnableUUID}
	if u, err := user.Lookup(username); err == nil {
		runtimeDir := filepath.Join("/run/user", u.Uid)
		inner = append([]string{
			"env",
			"XDG_RUNTIME_DIR=" + runtimeDir,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(runtimeDir, "bus"),
		}, inner...)
	}

	var argv []string
	switch {
	case lookPathOK("runuser"):
		p, _ := exec.LookPath("runuser")
		argv = append([]string{p, "-u", username, "--"}, inner...)
	case lookPathOK("sudo"):
		p, _ := exec.LookPath("sudo")
		argv = append([]string{p, "-u", username, "-H", "--"}, inner...)
	default:
		return
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = scrubbedChildEnv(os.Environ())
	_ = cmd.Run()
}
