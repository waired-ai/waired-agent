package update

import "os"

// ScriptBaseURLForChannel returns the installer-script download base for the
// host's current channel: "edge" fetches the moving edge prerelease, anything
// else the latest stable release. WAIRED_INSTALL_BASE_URL, when set, overrides
// both (the same precedence install.sh applies), so a mirror or pin still wins.
//
// The apply path (`waired update` / the tray) fetches the official installer
// from here and runs it under elevation — reusing the existing, signing-free
// installer machinery rather than reimplementing download/verify/swap/restart
// in Go. Fetching from the host's *current* channel mirror guarantees the
// script is at least as new as the running binary, so any newly-added installer
// flags (e.g. --stable) are understood.
func ScriptBaseURLForChannel(channel string) string {
	if v := os.Getenv("WAIRED_INSTALL_BASE_URL"); v != "" {
		return v
	}
	if channel == "edge" {
		return "https://github.com/" + defaultInstallRepo + "/releases/download/edge"
	}
	return "https://github.com/" + defaultInstallRepo + "/releases/latest/download"
}

// ScriptBaseURL returns the stable-channel installer-script base (back-compat
// shim over ScriptBaseURLForChannel).
func ScriptBaseURL() string { return ScriptBaseURLForChannel("stable") }

// ScriptName returns the installer script filename for goos.
func ScriptName(goos string) string {
	if goos == "windows" {
		return "install.ps1"
	}
	return "install.sh"
}

// ScriptURLForChannel is the full installer-script download URL for goos on the
// given host channel.
func ScriptURLForChannel(goos, channel string) string {
	return ScriptBaseURLForChannel(channel) + "/" + ScriptName(goos)
}

// ScriptURL is the stable-channel installer-script download URL for goos
// (back-compat shim over ScriptURLForChannel).
func ScriptURL(goos string) string { return ScriptURLForChannel(goos, "stable") }

// InstallerArgs returns the (command, args) to run the downloaded installer at
// scriptPath for goos. checkOnly runs the installer's read-only check (no
// elevation, no download of packages); otherwise it applies the update, and yes
// skips the installer's interactive confirmation (used by the tray, which has
// no TTY). channel selects the update channel: "edge" / "stable" append the
// installer's --edge / --stable (-Edge / -Stable on Windows) flag; "" leaves
// the channel unspecified so the installer preserves whatever the host tracks.
// The installer scripts self-elevate (install.sh via sudo; install.ps1
// relaunches under UAC), so the caller need not.
func InstallerArgs(goos, scriptPath string, checkOnly, yes bool, channel string) (string, []string) {
	if goos == "windows" {
		args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath}
		if checkOnly {
			args = append(args, "-Check")
		} else {
			args = append(args, "-Update")
			if yes {
				args = append(args, "-Yes")
			}
		}
		switch channel {
		case "edge":
			args = append(args, "-Edge")
		case "stable":
			args = append(args, "-Stable")
		}
		return "powershell", args
	}
	var args []string
	if checkOnly {
		args = []string{scriptPath, "--check"}
	} else {
		args = []string{scriptPath, "--update"}
		if yes {
			args = append(args, "--yes")
		}
	}
	switch channel {
	case "edge":
		args = append(args, "--edge")
	case "stable":
		args = append(args, "--stable")
	}
	return "sh", args
}
