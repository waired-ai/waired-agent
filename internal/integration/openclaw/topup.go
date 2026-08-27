package openclaw

import (
	"context"
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

// TopUpContextWindow rewrites the installed plugin's declared window to the
// one the gateway reports now, and returns the value it wrote.
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
// changed=false with a nil error is the ordinary outcome and not a failure:
// no plugin, no answer from the gateway, or the plugin already says the
// right thing. The window is only ever rewritten to a POSITIVE value — a
// gateway that cannot answer must not blank a declaration that was right.
func TopUpContextWindow(ctx context.Context, home, gatewayBaseURL string) (window int, changed bool, err error) {
	declared, ok := DeclaredContextWindow(home)
	if !ok {
		return 0, false, nil
	}
	live := contextWindowFn(ctx, GatewayBaseURL(gatewayBaseURL), modelRefs()[0])
	if live <= 0 || live == declared {
		return declared, false, nil
	}
	entry, err := renderEntry(gatewayBaseURL, live)
	if err != nil {
		return declared, false, err
	}
	if err := writeFileAtomic(PluginEntryFile(home), entry, 0o644); err != nil {
		return declared, false, fmt.Errorf("openclaw: rewrite plugin window: %w", err)
	}
	return live, true, nil
}
