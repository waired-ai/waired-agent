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

// OpenCode inspects the waired-authored OpenCode plugin at
// ~/.config/opencode/plugin/waired.js and reports whether it registers
// the "waired" provider pointing at the expected data-plane baseURL.
// Mirrors the Claude detectors' Configured/Stale taxonomy so the
// management API + tray render OpenCode integration uniformly.
//
// expectedBaseURL is the value the plugin's provider baseURL *should*
// contain — the agent's no-token OpenCode data-plane URL, e.g.
// "http://127.0.0.1:9479/v1". A divergent on-disk value reports Stale
// with CurrentValue surfaced so the user can spot the drift.
//
// Background: OpenCode is wired via a plugin (not opencode.json) because a
// provider only surfaces with a config-side stanza and a self-contained
// plugin file is cleaner to own; see internal/integration/opencode.
func OpenCode(homeDir, expectedBaseURL string) Result {
	path := filepath.Join(homeDir, ".config", "opencode", "plugin", "waired.js")
	r := Result{Path: path, Flavor: "opencode"}

	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r // Configured=false, no Note
		}
		r.Note = fmt.Sprintf("read: %v", err)
		return r
	}
	if !bytes.Contains(body, []byte("provider.waired")) {
		r.Note = "plugin present but does not register provider.waired"
		return r
	}
	current, ok := extractOpenCodePluginBaseURL(body)
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

var openCodePluginBaseURLRe = regexp.MustCompile(`baseURL:\s*"([^"]+)"`)

// extractOpenCodePluginBaseURL pulls the provider baseURL string literal
// out of the generated plugin JS (options: { baseURL: "<url>" }).
func extractOpenCodePluginBaseURL(body []byte) (string, bool) {
	m := openCodePluginBaseURLRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}
