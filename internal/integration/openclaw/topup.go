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
// and whether it declares one at all. 0/true is a real answer: the plugin
// was written before anything was serving, so it says the host does not
// know — the shape docs/decisions/20260822/2116 chose over guessing.
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

// TopUpContextWindow rewrites the installed plugin's declared window, and the
// rows it offers, to what the gateway reports now, and returns the window it
// wrote.
//
// It exists because the plugin's window is written ONCE, by the CLI, at link
// time — and `waired init` applies the integrations before anything is
// serving, so on a fresh install the gateway cannot be asked and the plugin
// correctly declares nothing. Nothing ever went back. The host then relied on
// OpenClaw's own default forever, which is the class of problem
// waired-agent#1001 fixed for the known-window case and
// docs/decisions/20260822/2116 recorded as the cost of writing nothing:
//
//	窓が変わる…と、プラグインの値は次に link するまで古いまま になる。
//	…ずれを検出して doctor に出す のは今回入れていない。
//
// This is the "go back" half. It stays with the CLI — the daemon never
// writes into a user's home (waired#935) — and it is the exact shape
// topUpClaudeWindow already has for Claude Code's own window
// (waired-agent#796), called from the same place for the same reason.
//
// The rows are the same problem one level up (waired-agent#1306). A row names
// a COMPUTER, and at link time inside `waired init` this computer is not on a
// mesh yet, so the plugin is written with the one row that needs no mesh.
// Refreshing them here is what turns that into the real list on a fresh
// install — and unlike the window, the rows also have to reach
// agents.defaults.models, because that allowlist is what OpenClaw's picker
// actually shows.
//
// changed=false with a nil error is the ordinary outcome and not a failure:
// no plugin, no answer from the gateway, or the plugin already says the
// right thing. The window is only ever rewritten to a POSITIVE value — a
// gateway that cannot answer must not blank a declaration that was right —
// and the rows are only rewritten when the gateway actually offered some.
func TopUpContextWindow(ctx context.Context, home, gatewayBaseURL string) (window int, changed bool, err error) {
	declared, ok := DeclaredContextWindow(home)
	if !ok {
		return 0, false, nil
	}
	live := contextWindowFn(ctx, GatewayBaseURL(gatewayBaseURL), modelRefPrefix+defaultModelKey)
	fetched := rowsFn(ctx, GatewayBaseURL(gatewayBaseURL))
	window = declared
	if live > 0 {
		window = live
	}
	rows := pluginRows(fetched)
	refs := modelRefs(rows)
	rowsChanged := len(fetched) > 0 && !sameRefs(refs, declaredRefs(home))
	if window == declared && !rowsChanged {
		return declared, false, nil
	}
	entry, err := renderEntry(gatewayBaseURL, window, rows)
	if err != nil {
		return declared, false, err
	}
	if err := writeFileAtomic(PluginEntryFile(home), entry, 0o644); err != nil {
		return declared, false, fmt.Errorf("openclaw: rewrite plugin: %w", err)
	}
	if rowsChanged {
		if err := mergeConfigFile(ConfigFile(home), PluginDir(home), refs); err != nil {
			return window, true, fmt.Errorf("openclaw: refresh model list: %w", err)
		}
	}
	return window, true, nil
}

// declaredRefs reads the picker references the installed plugin currently
// carries, so a refresh that would write the same list writes nothing. It
// reads the plugin rather than openclaw.json because the plugin is the file
// this package owns outright; the config is the user's, merged into.
func declaredRefs(home string) []string {
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
	return modelRefs(rows)
}

// declaredModelsRe reads the rows a written plugin carries. It matches the one
// line renderEntry emits from the template, so the two move together or this
// returns nothing and the caller refreshes rather than skipping.
var declaredModelsRe = regexp.MustCompile(`(?m)^const MODELS = (\[.*\]);$`)

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
