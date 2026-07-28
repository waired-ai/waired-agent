package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/proxy/legacycleanup"
)

// Claude Code request routing for `waired init` (#294).
//
// Routing means one thing: the machine-wide Claude Code managed-settings
// file carries ANTHROPIC_BASE_URL pointing at waired's loopback gateway
// (no credential, so the claude.ai subscription and auto-mode survive).
// `waired claude enable` writes it by hand; init is supposed to be the
// SINGLE decider at install time — the installers deleted their own
// post-init `waired claude enable` step precisely because it overrode an
// interactive "no", and forward their opt-out as --skip-claude-route
// instead (install.sh linux_maybe_init / install.ps1 Get-WairedInitArgs).
//
// That contract was only ever honoured by the standalone enrollment path.
// Enrollment is daemon-owned now (#175) and the installers start the
// service before running init, so EVERY real install takes the daemon
// route — where nothing wrote managed settings at all. A CLI install
// finished with Claude Code still talking to the Anthropic API, and
// --skip-claude-route opted out of something that was not going to
// happen (#294). The write lives here, on the path installs actually
// take.
//
// The writer is always THIS process, never the daemon: the daemon runs
// as a service account and making it write a root-owned machine-wide
// file would turn it into a privilege bridge for any local process that
// can reach its unauthenticated management API (waired#935 §8.3). The
// per-user sudo hop (runLinkAllAsUser) cannot do it either — it drops
// root by design. So the elevated CLI writes it in-process, exactly as
// `waired claude enable` does.

// isElevatedFn is the elevation probe the routing decision reads. A seam
// because the decision is otherwise untestable: a unit test never runs
// elevated, so every routing path would resolve to "needs elevation" and
// the branch that actually writes would never be exercised. planClaudeRoute
// keeps the logic itself pure and table-tested; this only supplies the fact.
var isElevatedFn = elevation.IsElevated

// claudeRouteAction is what a run should do about Claude Code routing.
type claudeRouteAction int

const (
	// claudeRouteNone: do not touch routing. No integration consent, an
	// explicit opt-out, or an OS with no managed-settings location.
	claudeRouteNone claudeRouteAction = iota
	// claudeRouteNeedsElevation: the operator consented but this process
	// cannot write the machine-wide file. Say so with the platform-correct
	// elevation hint rather than skipping silently — the silent skip is
	// what left the consent copy looking like routing had been enabled
	// (waired#749).
	claudeRouteNeedsElevation
	// claudeRouteApply: write it without asking. Either the run is
	// non-interactive (the installers' --yes), or the browser wizard is
	// driving and already asked — in both cases a terminal prompt would
	// have nobody to answer it.
	claudeRouteApply
	// claudeRouteAsk: ask first, default Yes. The deferred routing question
	// (waired#772): asked at the end of init, once the engine, the model
	// and the benchmark are done and the local stack can actually serve.
	claudeRouteAsk
)

// claudeRouteFacts is everything the decision depends on, so the decision
// itself is a pure function that can be table-tested over every
// combination (CLAUDE.md §Test discipline).
type claudeRouteFacts struct {
	// integConsent is the coding-agent integration consent. On the
	// terminal path it is the answer to the one consent question; on the
	// wizard path it is "claude-code is among the toggles the wizard
	// turned on". Routing is part of the Claude Code integration, not a
	// separate product, so a "no" there means no routing either.
	integConsent bool
	// elevated is elevation.IsElevated() at the call site — NOT a bare
	// euid check, which is -1 on Windows and would exclude an elevated
	// Administrator run entirely (waired#749).
	elevated bool
	// managedPath is claudemanaged.Path(); empty only on an OS with no
	// managed-settings location, which cannot be written regardless.
	managedPath string
	// skipClaudeRoute is --skip-claude-route (WAIRED_NO_CLAUDE_PROXY /
	// the installers' -SkipClaudeProxy / --skip-claude-proxy). It outranks
	// everything below: it is the operator saying "leave Claude Code
	// alone", and this is the gate the installers' deleted post-init
	// `waired claude enable` used to bypass.
	skipClaudeRoute bool
	// nonInteractive is --non-interactive: there is nobody to answer a
	// prompt, so the consent already given stands on its own.
	nonInteractive bool
	// wizardDriving is true while the browser setup wizard holds the
	// flow (waired#835 §4.2). The terminal must not ask its own
	// questions then — the wizard's own toggle is the answer.
	wizardDriving bool
}

// planClaudeRoute decides what a run does about Claude Code routing.
//
// Order matters: the opt-outs are checked before the capability gates, so
// a `--skip-claude-route` run never prints an elevation hint for work it
// was told not to do.
func planClaudeRoute(f claudeRouteFacts) claudeRouteAction {
	if !f.integConsent || f.skipClaudeRoute || f.managedPath == "" {
		return claudeRouteNone
	}
	if !f.elevated {
		return claudeRouteNeedsElevation
	}
	if f.wizardDriving || f.nonInteractive {
		return claudeRouteApply
	}
	return claudeRouteAsk
}

// claudeRouteApplyOpts parameterises applyClaudeRoute.
type claudeRouteApplyOpts struct {
	StateDir string
	In       lineReader
	// AllowPrompt gates the statusline's ask-before-wrapping question:
	// false while the browser wizard drives (waired#835 §4.2) or on a
	// non-interactive run, true when the caller owns the TTY.
	AllowPrompt bool
	// SkipStatusline is `waired claude enable --no-statusline`: route, but
	// leave the Claude Code footer alone.
	SkipStatusline bool
}

// applyClaudeRoute performs the routing flip: the legacy-MITM sweep, the
// managed-settings write, and the two per-user extras. It is the single
// implementation behind `waired claude enable` and both of init's
// daemon-path call sites, so "routed by init" and "routed by hand" leave
// a host in the same state — they used to differ (init never installed
// the /waired-route slash command). Returns the written path.
func applyClaudeRoute(o claudeRouteApplyOpts) (string, error) {
	baseURL, _ := claudeBaseURL(o.StateDir)

	// Sweep any retired MITM proxy artifacts first: a stale
	// api.anthropic.com hosts redirect would otherwise break the new
	// gateway's passthrough leg.
	legacycleanup.Run(o.StateDir, stderrLogger())

	// Write also installs the Stop hook (managed-settings hooks.Stop) so a
	// post-dispatch fallback is visible in the Claude Code TUI (#580).
	path, err := claudemanaged.WriteWithOptions(baseURL, claudeManagedWriteOptions(o.StateDir))
	if err != nil {
		return "", err
	}

	// Both are best-effort: the managed-settings write above is the core
	// of routing, and neither of these can undo it.
	installRouteSkillForInvoker()
	installStatuslineForInvoker(o.SkipStatusline, o.AllowPrompt, o.In)
	return path, nil
}

// applyClaudeRouteFn is the seam tests replace to exercise init's routing
// decision without writing this machine's real managed settings. It takes
// the full options so a fake records the real arguments (CLAUDE.md
// §Test discipline); `waired claude enable` calls applyClaudeRoute
// directly, so the real implementation still runs under the enable tests.
var applyClaudeRouteFn = applyClaudeRoute

// routeClaudeNow applies routing and reports whether it took. Failures
// warn and return false: sign-in has already succeeded and a routing
// failure must not turn a good install into a failed one, but it must
// also never be reported as routed.
func routeClaudeNow(o claudeRouteApplyOpts, out io.Writer) bool {
	path, err := applyClaudeRouteFn(o)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warn: writing Claude Code managed settings failed (%v); %s\n",
			err, elevationHintFor(runtime.GOOS, "waired claude enable"))
		return false
	}
	baseURL, _ := claudeBaseURL(o.StateDir)
	writePromptf(out, "  %s → ANTHROPIC_BASE_URL=%s (no credential; subscription/auto-mode preserved)\n", path, baseURL)
	return true
}

// promptClaudeRouting asks the deferred end-of-init routing question
// (waired#772): route Claude Code inference through Waired now? It runs
// after the engine setup, model download, and benchmark, so a "yes" flips
// the route at the moment the local stack can actually serve. Default Yes
// — the user already opted into the integration up front. On no it leaves
// the integration artifacts installed, Claude traffic on the real
// Anthropic API, and prints how to enable later. Returns whether routing
// was enabled.
func promptClaudeRouting(out io.Writer, sc lineReader, stateDir string) bool {
	baseURL, _ := claudeBaseURL(stateDir)
	return promptClaudeRoutingWith(out, sc, baseURL, func() bool {
		// init owns the TTY here, so the statusline step may prompt.
		return routeClaudeNow(claudeRouteApplyOpts{
			StateDir: stateDir, In: sc, AllowPrompt: true,
		}, out)
	})
}

// promptClaudeRoutingWith is promptClaudeRouting with the apply step
// injectable, so prompt-level tests don't write system managed settings.
func promptClaudeRoutingWith(out io.Writer, sc lineReader, baseURL string, apply func() bool) bool {
	writePrompt(out)
	writePromptf(out, "%s %s\n", emo("🔌", "*"), bold("Claude Code request routing"))
	writePromptf(out, "Routing points Claude Code's ANTHROPIC_BASE_URL at your local Waired gateway\n")
	writePromptf(out, "(%s — no credential; subscription/auto-mode preserved; requests local\n", baseURL)
	writePrompt(out, "inference can't serve fall back to the Anthropic API).")
	if !ynPrompt(out, sc, "Route Claude Code inference through Waired now?", true) {
		writePrompt(out, "Routing left off — Claude Code keeps talking to the Anthropic API directly.")
		writePromptf(out, "Enable anytime with `%s`; steer per-session with\n", elevatedCmdline(runtime.GOOS, "waired claude enable"))
		writePrompt(out, "`waired claude route` (or the `/waired-route` skill).")
		return false
	}
	return apply()
}

// printClaudeRouteElevationHint is the honest report for a consented run
// that cannot write the machine-wide file. Suppressed on an OS with no
// managed-settings path at all — planClaudeRoute never returns
// claudeRouteNeedsElevation there.
func printClaudeRouteElevationHint(out io.Writer) {
	writePrompt(out)
	writePromptf(out, "%s Claude Code request routing needs elevation — %s to route Claude through Waired.\n",
		emo("🔌", "*"), elevationHintFor(runtime.GOOS, "waired claude enable"))
}
