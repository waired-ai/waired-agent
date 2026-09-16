package main

import (
	"fmt"
	"runtime"

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
	// exists to size waired's own /model rows and means nothing with
	// directives off.
	directives bool
	// elevated: only an elevated process may write the machine-wide file.
	elevated bool
	// managed: CLAUDE_CODE_MAX_CONTEXT_TOKENS as it stands on disk.
	managed string
}

// claudeWindowTopUpNeeded decides whether CLAUDE_CODE_MAX_CONTEXT_TOKENS has to
// be set to 200704 on the way out of init, `waired link` or doctor's repair.
func claudeWindowTopUpNeeded(f claudeWindowFacts) bool {
	return f.routed && f.directives && f.elevated &&
		f.managed != claudemanaged.DirectivesMaxContextTokensValue
}

// topUpClaudeWindow sets CLAUDE_CODE_MAX_CONTEXT_TOKENS to 200704 on a routed
// host whose managed settings say anything else (waired-agent#1396).
//
// Two kinds of host need it. One routed by an older build carries the window
// that build derived — this computer's, or the smallest it could reach — and
// every Waired row without "[1m]" is a 200k session now. One routed by the
// browser wizard of an older build carries nothing, because the route was
// applied before anything served and that build would not write a window it
// could not ask for (waired-agent#796, #311).
//
// Warn-only. Sign-in has already succeeded, and a wrong window key does not
// stop Claude Code — the gateway's per-request overflow guard still holds the
// turn to its row. Failing init over it would trade a cosmetic gap for a
// broken install.
func topUpClaudeWindow(stateDir string) {
	opts := claudeManagedWriteOptions(stateDir)
	path := claudemanaged.Path()
	if !claudeWindowTopUpNeeded(claudeWindowFacts{
		routed:     claudeCardRouted(stateDir),
		directives: opts.ModelRouteDirectives,
		elevated:   isElevatedFn(),
		managed:    claudemanaged.MaxContextTokensAt(path),
	}) {
		return
	}
	if _, err := claudemanaged.SetMaxContextTokensAt(path); err != nil {
		fmt.Fprintf(stderr, "Warning: couldn't record the Claude Code context window (%v). %s\n",
			err, elevationHintFor(runtime.GOOS, "waired claude enable"))
	}
}
