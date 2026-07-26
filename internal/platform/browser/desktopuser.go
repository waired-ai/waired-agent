package browser

import (
	"errors"
	"strconv"
	"strings"
)

// The portable core behind Open. Every OS-varying decision is a pure function
// here, taking the facts (GOOS, euid, SUDO_USER, the system directory) as
// arguments instead of reading them — so the whole matrix is table-tested on
// one OS, which matters because CI's unit job runs on Linux only and
// `make verify-cross` type-checks the other two without running anything.
//
// Three separate defects motivated this split (#181 / #182 / #183); all three
// were per-OS argv the tests could not see.

// hop tool kinds for linuxHopArgv. runuser (util-linux, Essential on Debian)
// is preferred over sudo: it resolves the user through NSS and needs no
// sudoers entry.
const (
	hopRunuser = "runuser"
	hopSudo    = "sudo"
)

// validateURL rejects input Open must not hand to a launcher. Empty is a
// caller bug; a leading '-' would be parsed as a flag by open(1)/xdg-open
// rather than as a URL. Every real caller passes http(s), so neither costs
// anything.
func validateURL(url string) error {
	if url == "" {
		return errors.New("browser.Open: empty url")
	}
	if strings.HasPrefix(url, "-") {
		return errors.New("browser.Open: url must not start with '-'")
	}
	return nil
}

// elevatedFor reports the unprivileged user Open should hand the URL to when
// the process itself is root. Same contract as cmd/waired's
// invokingSudoUserAt: sudo is a Unix concept, euid must be 0, and SUDO_USER
// must name someone other than root — a real root login has no hop target.
func elevatedFor(goos string, euid int, sudoUser string) (string, bool) {
	if goos != "linux" && goos != "darwin" {
		return "", false
	}
	if euid != 0 {
		return "", false
	}
	if sudoUser == "" || sudoUser == "root" {
		return "", false
	}
	return sudoUser, true
}

// darwinHopArgv builds the de-escalated `open(1)` call for macOS (#182).
//
// Both halves are load-bearing. `launchctl asuser <uid>` moves the child into
// the user's bootstrap namespace (per-user Mach services), but it still runs
// as root — and open(1) picks the browser from the LaunchServices handler map
// of the EFFECTIVE uid, where root has no http/https entry and falls back to
// Safari. The inner `sudo -u` is what actually drops to the user. `-n` keeps
// sudo from ever waiting on a prompt (root is not asked to authenticate in any
// normal configuration, but the CLI must not be able to hang here).
func darwinHopArgv(name, uid, url string) []string {
	return []string{
		"/bin/launchctl", "asuser", uid,
		"/usr/bin/sudo", "-n", "-u", name,
		"/usr/bin/open", url,
	}
}

// linuxHopArgv builds the de-escalated xdg-open call for Linux (#183).
//
// sudo's env_reset keeps DISPLAY/XAUTHORITY in the common configuration but
// drops XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS, so a root xdg-open finds
// the display yet has no session bus: it resolves the handler from root's MIME
// database and starts a root-profile browser. Re-supplying the two session
// variables is the same shape cmd/waired/init_tray_linux.go already uses to
// reach the user's gnome-shell.
//
// An empty uid means the lookup failed; hopping to the user without the
// session env still beats running as root, so the prefix is simply omitted.
func linuxHopArgv(hopBin, hopKind, name, uid, url string) []string {
	inner := []string{"xdg-open", url}
	if uid != "" {
		// Concatenated, not filepath.Join: this file is untagged, and the
		// test must produce the same POSIX path whatever GOOS it runs on.
		runtimeDir := "/run/user/" + uid
		inner = append([]string{
			"env",
			"XDG_RUNTIME_DIR=" + runtimeDir,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
		}, inner...)
	}
	switch hopKind {
	case hopRunuser:
		return append([]string{hopBin, "-u", name, "--"}, inner...)
	case hopSudo:
		return append([]string{hopBin, "-u", name, "-H", "--"}, inner...)
	default:
		return nil
	}
}

// windowsRundllCmd builds the CreateProcess arguments for Windows (#181).
//
// Win32 does NOT search %PATH% when lpApplicationName is non-NULL — it
// resolves relative to the current directory only. A bare "rundll32.exe" from
// the CLI's usual working directory (the user's home) therefore fails with
// err=2, "The system cannot find the file specified", and nothing opens.
// systemDir comes from GetSystemDirectory; when it cannot be resolved the app
// name is empty, meaning a NULL lpApplicationName — the command line then
// resolves rundll32 through the normal search order, which is verified to work
// and is still better than a name that cannot resolve at all.
//
// The backslash is written out rather than joined via filepath so the builder
// yields a Windows path even when the table test runs on Linux.
func windowsRundllCmd(systemDir, url string) (app, cmdline string) {
	cmdline = `rundll32.exe url.dll,FileProtocolHandler ` + url
	if systemDir == "" {
		return "", cmdline
	}
	return strings.TrimRight(systemDir, `\`) + `\rundll32.exe`, cmdline
}

// parseConsoleOwner reads `stat -f "%u %Su" /dev/console`, macOS's answer to
// "who is sitting at this Mac" — the fallback when a root session has no
// SUDO_USER to name. root owns /dev/console at the login window and before
// anyone logs in, which is not a hop target, so it is rejected along with a
// non-numeric uid.
func parseConsoleOwner(out string) (name, uid string, ok bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 {
		return "", "", false
	}
	uid, name = f[0], f[1]
	if name == "" || name == "root" {
		return "", "", false
	}
	n, err := strconv.Atoi(uid)
	if err != nil || n <= 0 {
		return "", "", false
	}
	return name, uid, true
}

// scrubEnv drops the variables that would point the de-escalated child at
// root's state instead of the user's (a stray `sudo -E` leak). runuser and
// `sudo -H` set HOME for the target user themselves. Mirrors
// cmd/waired/init_integration.go's scrubbedChildEnv; DISPLAY/XAUTHORITY and
// the rest pass through untouched, since the whole point is to reach the
// user's session.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") ||
			strings.HasPrefix(kv, "WAIRED_STATE_DIR=") ||
			strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
