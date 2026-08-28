package main

import (
	"fmt"
	"runtime"
	"strconv"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
)

// claudeRoutedNow reports whether Claude Code on this machine is pointed at this
// device's gateway, read from the managed-settings file rather than from what
// the calling run happened to do.
//
// `waired init`'s closing card used to answer this from a local bool that was
// only ever assigned on the terminal-driven path, so a browser-wizard install
// closed by reporting Claude Code unrouted over a machine it had just routed
// (waired-agent#796). Reading the file is the same source `waired claude status`
// reports from, which is what makes the two structurally unable to disagree.
//
// Pure over (managedPath, expectedBaseURL) so the verdict is table-testable
// without an init run. An empty path (an OS with no managed-settings location)
// is not routed, and neither is a file pointing somewhere that is not us.
func claudeRoutedNow(managedPath, expectedBaseURL string) bool {
	present, baseURL := claudemanaged.ViewAt(managedPath)
	return present && baseURL != "" && baseURL == expectedBaseURL
}

// claudeCardRouted is claudeRoutedNow wired to this host, for init's closing
// card. The expected URL comes from claudeBaseURL — agent.json's
// ClaudeGatewayPort — because that is what the enable path writes and what
// `waired claude status` compares against.
func claudeCardRouted(stateDir string) bool {
	baseURL, _ := claudeBaseURL(stateDir)
	return claudeRoutedNow(claudemanaged.Path(), baseURL)
}

// claudeWindowFacts is everything the context-window top-up decision depends on,
// so the decision itself is pure and every combination is table-tested.
type claudeWindowFacts struct {
	// routed: managed settings carry THIS host's loopback base URL. A file we
	// did not write is not ours to add keys to.
	routed bool
	// directives: agent.json's ClaudeModelRouteDirectives (#52). The window key
	// exists to size waired's own local /model directive id and means nothing
	// with directives off.
	directives bool
	// elevated: only an elevated process may write the machine-wide file.
	elevated bool
	// managed: CLAUDE_CODE_MAX_CONTEXT_TOKENS as it stands on disk.
	managed string
	// live: what the gateway advertises right now. 0 = unknown, and an unknown
	// window is never written — a stale honest number beats a fresh guess.
	live int
}

// claudeWindowTopUpNeeded decides whether init should fill in
// CLAUDE_CODE_MAX_CONTEXT_TOKENS on its way out (waired-agent#796).
func claudeWindowTopUpNeeded(f claudeWindowFacts) bool {
	return f.routed && f.directives && f.elevated &&
		f.live > 0 && f.managed != strconv.Itoa(f.live)
}

// topUpClaudeWindow closes the second half of waired-agent#796. The browser
// wizard applies the Claude Code route before the model download
// (waired-agent#311), so the window is unresolvable at write time and the key is
// correctly left out; by the time init reaches its closing card the model is
// ready and /v1/models can answer, so the number exists and can be recorded.
//
// Warn-only. Sign-in has already succeeded, and a missing window key does not
// stop Claude Code — the gateway's per-request overflow guard still protects the
// real window. Failing init over it would trade a cosmetic gap for a broken
// install.
func topUpClaudeWindow(stateDir string) {
	opts := claudeManagedWriteOptions(stateDir)
	path := claudemanaged.Path()
	if !claudeWindowTopUpNeeded(claudeWindowFacts{
		routed:     claudeCardRouted(stateDir),
		directives: opts.ModelRouteDirectives,
		elevated:   isElevatedFn(),
		managed:    claudemanaged.MaxContextTokensAt(path),
		live:       opts.LocalContextWindow,
	}) {
		return
	}
	if _, err := claudemanaged.SetMaxContextTokensAt(path, opts.LocalContextWindow); err != nil {
		fmt.Fprintf(stderr, "warn: could not record the Claude Code context window (%v); %s\n",
			err, elevationHintFor(runtime.GOOS, "waired claude enable"))
	}
}
