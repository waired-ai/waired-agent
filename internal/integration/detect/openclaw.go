package detect

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// OpenClaw inspects the waired-authored OpenClaw plugin entry at
// ~/.openclaw/plugins/waired/index.mjs and reports whether it registers the
// "waired" provider pointing at the expected data-plane baseURL. Mirrors the
// Claude/OpenCode detectors' Configured/Stale taxonomy so the management API
// + tray render OpenClaw integration uniformly.
//
// expectedBaseURL is the value the plugin's BASE_URL *should* contain — the
// agent's no-token data-plane URL with the /v1 suffix, e.g.
// "http://127.0.0.1:9479/v1". A divergent on-disk value reports Stale with
// CurrentValue surfaced so the user can spot the drift.
//
// Background: OpenClaw is wired via a self-contained plugin plus a small
// openclaw.json enable/allowlist merge; see internal/integration/openclaw.
func OpenClaw(homeDir, expectedBaseURL string) Result {
	path := filepath.Join(homeDir, ".openclaw", "plugins", "waired", "index.mjs")
	r := Result{Path: path, Flavor: "openclaw"}

	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r // Configured=false, no Note
		}
		r.Note = fmt.Sprintf("read: %v", err)
		return r
	}
	if !bytes.Contains(body, []byte("registerProvider")) {
		r.Note = "plugin present but does not register a provider"
		return r
	}
	current, ok := extractOpenClawPluginBaseURL(body)
	if !ok {
		r.Note = "plugin present but provider baseURL not found"
		return r
	}
	r.Configured = true
	r.CurrentValue = current
	if expectedBaseURL != "" && current != expectedBaseURL {
		r.Stale = true
	}
	return r
}

var openClawPluginBaseURLRe = regexp.MustCompile(`BASE_URL\s*=\s*"([^"]+)"`)

// extractOpenClawPluginBaseURL pulls the provider BASE_URL string literal out
// of the generated plugin JS (const BASE_URL = "<url>";).
func extractOpenClawPluginBaseURL(body []byte) (string, bool) {
	m := openClawPluginBaseURLRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}
