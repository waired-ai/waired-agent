package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
)

// declaredWindowRe reads the window a written plugin declares. It matches
// the one line renderEntry emits from the template, so the two move
// together or this returns "no declaration" and every caller degrades to
// leaving the plugin alone.
var declaredWindowRe = regexp.MustCompile(`(?m)^const CONTEXT_WINDOW = (\d+);`)

// DeclaredContextWindow reports the window the installed plugin declares,
// and whether it declares one at all. 0/true is a real answer from a plugin an
// older build wrote before anything was serving; this build always declares
// 200704 (waired-agent#1396).
//
// ok=false means there is no plugin, or its file does not carry the line —
// a hand-edited plugin, or one from a build that predates the field. Both
// are "not ours to correct".
func DeclaredContextWindow(home string) (int, bool) {
	body, err := os.ReadFile(PluginEntryFile(home))
	if err != nil {
		return 0, false
	}
	m := declaredWindowRe.FindSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return n, true
}

// TopUpContextWindow rewrites an installed plugin that an older build wrote,
// or whose rows the gateway no longer offers, and returns the window it
// declares.
//
// The plugin is written ONCE, by the CLI, at link time, and nothing else
// goes back to it. This is the going back. It stays with the CLI — the daemon
// never writes into a user's home (waired#935) — and it is the shape
// topUpClaudeWindow has for Claude Code's own window, called from the same
// place for the same reason.
//
// The window needs no gateway: this build declares 200704 on every plugin,
// and a 1M window for every "[1m]" key (waired-agent#1396). What an older
// build declared — the window the gateway stated when it linked, or nothing
// when nothing served yet (#1001, #1029) — is rewritten here, and so is every
// plugin from an older revision of the template, because what it does is
// what changed.
//
// The rows name COMPUTERS (waired-agent#1306), and at link time inside
// `waired init` this computer is not on a mesh yet, so the plugin is written
// with the one row that needs no mesh. Refreshing them here is what turns that
// into the real list on a fresh install — and the rows also have to reach
// agents.defaults.models, because that allowlist is what OpenClaw's picker
// actually shows.
//
// changed=false with a nil error is the ordinary outcome and not a failure:
// no plugin, or the plugin already says the right thing. The rows are only
// rewritten when the gateway actually offered some.
func TopUpContextWindow(ctx context.Context, home, gatewayBaseURL string) (window int, changed bool, err error) {
	declared, ok := DeclaredContextWindow(home)
	if !ok {
		return 0, false, nil
	}
	fetched := rowsFn(ctx, GatewayBaseURL(gatewayBaseURL))
	window = pluginContextWindow
	rows := declaredRows(home)
	refsChanged := false
	if len(fetched) > 0 {
		refsChanged = !sameRefs(modelRefs(pluginRows(fetched)), modelRefs(rows))
		rows = pluginRows(fetched)
	}
	rowsChanged := len(fetched) > 0 && !sameRows(rows, declaredRows(home))
	if window == declared && !rowsChanged && declaredRevision(home) >= pluginRevision {
		return declared, false, nil
	}
	refs := modelRefs(rows)
	entry, err := renderEntry(gatewayBaseURL, rows)
	if err != nil {
		return declared, false, err
	}
	if err := writeFileAtomic(PluginEntryFile(home), entry, 0o644); err != nil {
		return declared, false, fmt.Errorf("openclaw: rewrite plugin: %w", err)
	}
	if refsChanged {
		if err := mergeConfigFile(ConfigFile(home), PluginDir(home), refs); err != nil {
			return window, true, fmt.Errorf("openclaw: refresh model list: %w", err)
		}
	}
	return window, true, nil
}

// declaredRows reads the rows the installed plugin currently carries, so a
// refresh that would write the same list writes nothing. It reads the plugin
// rather than openclaw.json because the plugin is the file this package owns
// outright; the config is the user's, merged into.
func declaredRows(home string) []pluginRow {
	body, err := os.ReadFile(PluginEntryFile(home))
	if err != nil {
		return nil
	}
	m := declaredModelsRe.FindSubmatch(body)
	if m == nil {
		return nil
	}
	var rows []pluginRow
	if err := json.Unmarshal(m[1], &rows); err != nil {
		return nil
	}
	return rows
}

// declaredRevRe reads PLUGIN_REV off a written plugin, unanchored at the end
// for the CRLF reason declaredModelsRe gives.
var declaredRevRe = regexp.MustCompile(`(?m)^const PLUGIN_REV = (\d+);`)

// declaredRevision is the template revision that wrote the installed plugin,
// 1 for a plugin from before the line existed.
func declaredRevision(home string) int {
	body, err := os.ReadFile(PluginEntryFile(home))
	if err != nil {
		return 0
	}
	m := declaredRevRe.FindSubmatch(body)
	if m == nil {
		return 1
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 1
	}
	return n
}

func sameRows(a, b []pluginRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// declaredModelsRe reads the rows a written plugin carries. It matches the one
// line renderEntry emits from the template, so the two move together or this
// returns nothing and the caller refreshes rather than skipping.
//
// Not anchored at the end of the line. The template is embedded at BUILD time
// and Git for Windows checks a text file out with CRLF endings by default
// (this repo carries no .gitattributes), so on a Windows build the line ends
// "];\r\n" and a "$" would never match — caught by the Windows unit-test leg,
// green on every Linux run. declaredWindowRe below was already unanchored and
// so was never exposed to this.
var declaredModelsRe = regexp.MustCompile(`(?m)^const MODELS = (\[.*\]);`)

func sameRefs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
