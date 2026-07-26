//go:build darwin

package browser

import (
	"context"
	"os"
	"os/exec"
	"runtime"
)

// Open launches url with the user's default handler. macOS ships `open(1)`,
// the canonical LaunchServices entry point (equivalent to xdg-open on Linux).
//
// When the process is root it first de-escalates to the desktop user (#182).
// install.sh always elevates before running `waired init`, and open(1) resolves
// the handler map for the EFFECTIVE uid — root has no http/https handler, so
// Safari wins over the user's actual default. That is not cosmetic: the setup
// ticket is bound to the browser session that completed sign-in, so the wizard
// would otherwise only ever be drivable from a browser the user does not use.
func Open(url string) error {
	if err := validateURL(url); err != nil {
		return err
	}
	if name, uid, ok := desktopTarget(); ok {
		if err := runHop(darwinHopArgv(name, uid, url)); err == nil {
			return nil
		}
		// Fall through: a failed hop degrades to the pre-#182 behaviour
		// rather than to no browser at all.
	}
	return openDirect(url)
}

// openDirect is the unprivileged launch — what Open did before #182, and the
// fallback for every path where de-escalation does not apply or does not work.
func openDirect(url string) error {
	cmd := exec.Command("/usr/bin/open", url)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// desktopTarget names the user whose LaunchServices should choose the browser.
// $SUDO_USER first (the installer's path), then whoever owns /dev/console,
// which covers a genuine root login on a Mac someone is sitting at.
func desktopTarget() (name, uid string, ok bool) {
	euid := os.Geteuid()
	if euid != 0 {
		return "", "", false
	}
	if n, isSudo := elevatedFor(runtime.GOOS, euid, os.Getenv("SUDO_USER")); isSudo {
		if u := lookupUID(n); u != "" {
			return n, u, true
		}
	}
	return consoleOwner()
}

// consoleOwner asks who is logged in at the physical console. The uid is
// needed as well as the name because `launchctl asuser` takes a uid.
func consoleOwner() (name, uid string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/stat", "-f", "%u %Su", "/dev/console").Output()
	if err != nil {
		return "", "", false
	}
	return parseConsoleOwner(string(out))
}

// HasDisplay reports whether a graphical session is present. On macOS we
// assume the GUI is available (the tray only runs in an Aqua session, and the
// CLI falls back to printing the URL if `open` fails over a headless SSH login).
func HasDisplay() bool { return true }
