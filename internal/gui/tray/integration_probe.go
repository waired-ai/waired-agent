package tray

import (
	"os"
	"os/user"

	"github.com/waired-ai/waired-agent/internal/integration/detect"
)

// The OpenCode and OpenClaw rows are read HERE, in the tray, rather than
// asked of the daemon (waired-agent#986).
//
// Both integrations are files in a home directory: ~/.config/opencode/
// plugin/waired.js and ~/.openclaw/plugins/waired/index.mjs. Under the
// deployment the installers produce by default the daemon is a service —
// LocalSystem on Windows, `waired` on Linux, root on macOS — so its
// os.UserHomeDir() is C:\WINDOWS\system32\config\systemprofile or
// /var/lib/waired, and probing from there reported "not configured" on
// every host where the integration was in fact working. The tray runs as
// the desktop user (internal/platform/paths: "the tray and CLI run as the
// desktop user while the daemon runs as a service user"), which is the
// same user the plugin belongs to, so it is the surface that can answer.
//
// The daemon still owns the one fact the tray cannot compute — which port
// its data-plane gateway is listening on — and hands it over as the
// expected baseURL. detect.OpenCode / detect.OpenClaw are pure functions
// over (home, expectedBaseURL), so nothing is re-implemented here.

// trayHomeFn resolves the desktop user's home. Overridden in tests.
var trayHomeFn = trayHome

// trayHome resolves this process's home with the same precedence
// trayLogHome uses ($HOME first, then the passwd entry). Empty when
// neither answers, which the callers read as "cannot probe" — the row
// group hides rather than claiming a file is missing.
func trayHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// probeOpenCode reports the state of the waired OpenCode plugin in the
// tray user's own home. A nil result means there is no home to look in.
func probeOpenCode(home, expectedBaseURL string) *detect.Result {
	if home == "" {
		return nil
	}
	r := detect.OpenCode(home, expectedBaseURL)
	return &r
}

// probeOpenClaw is the OpenClaw counterpart of probeOpenCode.
func probeOpenClaw(home, expectedBaseURL string) *detect.Result {
	if home == "" {
		return nil
	}
	r := detect.OpenClaw(home, expectedBaseURL)
	return &r
}
