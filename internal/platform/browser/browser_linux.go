//go:build linux

package browser

import (
	"os"
	"os/exec"
	"os/user"
	"runtime"
)

// Open launches the user's preferred handler for url via xdg-open — the only
// standard on Linux desktops, honored by both X11 and Wayland sessions (it is
// a thin shell over .desktop MIME resolution).
//
// When the process is root it first de-escalates to the desktop user (#183).
// Under sudo, DISPLAY and XAUTHORITY survive env_reset but XDG_RUNTIME_DIR and
// DBUS_SESSION_BUS_ADDRESS do not, so a root xdg-open finds the display and
// then resolves the handler from root's MIME database with no session bus: a
// root-profile browser instance rather than the user's own.
func Open(url string) error {
	if err := validateURL(url); err != nil {
		return err
	}
	if name, uid, ok := desktopTarget(); ok {
		if bin, kind, found := hopTool(); found {
			if err := runHop(linuxHopArgv(bin, kind, name, uid, url)); err == nil {
				return nil
			}
			// Fall through: a failed hop degrades to the pre-#183
			// behaviour rather than to no browser at all.
		}
	}
	return openDirect(url)
}

// openDirect is the unprivileged launch — what Open did before #183, and the
// fallback for every path where de-escalation does not apply or does not work.
func openDirect(url string) error {
	cmd := exec.Command("xdg-open", url)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// desktopTarget names the user whose session should own the browser. The uid
// may come back empty — linuxHopArgv then omits the session env and the hop
// still lands as the right user, which is the part that matters most.
func desktopTarget() (name, uid string, ok bool) {
	euid := os.Geteuid()
	if euid != 0 {
		return "", "", false
	}
	if n, isSudo := elevatedFor(runtime.GOOS, euid, os.Getenv("SUDO_USER")); isSudo {
		return n, lookupUID(n), true
	}
	// pkexec (the tray's elevation path) exports the caller's uid rather than
	// a name, so it is a second, equally reliable source.
	if v := os.Getenv("PKEXEC_UID"); v != "" && v != "0" {
		if u, err := user.LookupId(v); err == nil && u.Username != "" && u.Username != "root" {
			return u.Username, v, true
		}
	}
	return "", "", false
}

// hopTool picks how to become another user. runuser is preferred (util-linux,
// Essential on Debian: it resolves the user through NSS and needs no sudoers
// entry); sudo is the fallback. Same order as cmd/waired's own hops.
func hopTool() (bin, kind string, ok bool) {
	if p, err := exec.LookPath("runuser"); err == nil {
		return p, hopRunuser, true
	}
	if p, err := exec.LookPath("sudo"); err == nil {
		return p, hopSudo, true
	}
	return "", "", false
}

// HasDisplay reports whether a graphical session is present. On Linux a
// headless SSH server has neither DISPLAY (X11) nor WAYLAND_DISPLAY, so
// auto-opening a browser there is pointless — callers print the URL instead.
//
// Deliberately still read from the CURRENT process env even under sudo: those
// two variables survive env_reset in the common configuration, so an elevated
// `waired init` sees the same answer the user's shell would.
func HasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
