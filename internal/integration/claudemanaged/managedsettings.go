// Package claudemanaged writes Claude Code "managed settings" so the local
// waired agent can route Claude Code at its loopback gateway without any MITM
// proxy, CA, /etc/hosts edit, or shell-env management (#488).
//
// It sets env.ANTHROPIC_BASE_URL — pointing at waired's plain-HTTP loopback
// Anthropic listener (127.0.0.1:ClaudeGatewayPort) — plus one non-credential
// flag: env.CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1 (#623, populates the
// /model picker from our /v1/models). It deliberately writes NO credential
// variable. Per the Claude Code docs, a base-URL-only managed setting (no auth
// token) does not replace the claude.ai subscription, so subscription
// auto-mode (opusplan + the Max usage-threshold Opus->Sonnet fallback) is
// preserved.
//
// Context-window posture (#623 → #771): Claude Code resolves its auto-compact
// window per turn from the selected model id alone ("[1m]" variants → 1M,
// otherwise its built-in per-model table; verified against v2.1.207). An env
// CLAUDE_CODE_AUTO_COMPACT_WINDOW override outranks that resolution and is
// frozen at process start, so the static 200000 backstop #623 wrote here
// capped genuine 1M Anthropic sessions at 200k while adding nothing below
// 200k (the value never went under the model default). #771 therefore stops
// writing it — the gateway's per-request "prompt is too long" 400
// (internal/gateway/anthropic.go) remains the invariant that protects the
// smaller effective local window on the waired/auto routes, and Claude Code's
// own per-model resolution now governs the anthropic route, tracking /model
// switches mid-session. Write scrubs the legacy value from earlier installs.
//
// The model-route-directives feature (#52), when opted in, additionally writes
// env.CLAUDE_CODE_MAX_CONTEXT_TOKENS. That override is honoured ONLY for model
// ids not starting with "claude-", so it sizes the non-"claude-" directive ids
// ("anthropic-waired-local" and "anthropic-waired-auto") while never touching
// real "claude-*" ids — categorically different from the #771 auto-compact
// backstop that capped 1M Anthropic sessions. On by default (opt-out via
// agentconfig); WriteWithOptions gates the actual write.
//
// That value is the window this host can ACTUALLY serve, not a claim (#408):
// the caller resolves it from the gateway (Deps.ContextWindowFor — min of the
// manifest's native window and the tuning the engine really applied) and hands
// it in as WriteOptions.LocalContextWindow. Before #408 it was a static
// 250000, which promised ~256k on hosts serving 32k; once the agent started
// writing the /model picker cache (#407) users could select that id and
// believe the number. When the window cannot be resolved (agent down, no
// active model) Write leaves whatever the file carries alone rather than
// restating a figure it cannot stand behind.
//
// Refresh is deliberately NOT the daemon's: the writer is always the elevated
// CLI (docs/decisions/20260728/1444-init-daemon-path-owns-claude-routing.md
// §4, waired#935 — the daemon runs as a service account, Linux User=waired,
// behind an unauthenticated local IPC socket, so writing this admin-owned file
// would make it a privilege bridge). A serving-model change therefore leaves
// the value stale until the next `waired claude enable` / init; `waired claude
// status` shows the drift, the gateway's per-request overflow 400 keeps
// guarding the real window, and waired#1031 removes the drift structurally by
// fixing the window as an advertised contract. Note too that Claude Code
// applies env only at process start — no writer, however privileged, can
// correct a session already running.
//
// It also installs a SessionStart hook that keeps the /model picker entries
// current (waired-agent#830; see hook.go). Hooks array-merge across settings
// scopes, so a managed entry fires without clobbering the user's own hooks.
//
// It used to install a Stop hook as well, announcing a turn that had fallen
// back to the real Anthropic API. Nothing falls back any more, so there is
// nothing for it to announce and Write removes it
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
//
// managed-settings.json is the highest-precedence, system-wide Claude Code
// config; Claude Code reads it at startup independently of any shell rc, so a
// single root-time write covers every CLI invocation with no restart. The file
// lives at a fixed OS path (see path_*.go).
//
// The writer is merge-safe: it preserves any keys an operator (or MDM) already
// placed in managed-settings.json and only touches its own env keys
// (ANTHROPIC_BASE_URL, the #623 discovery flag, the legacy #623 auto-compact
// window when it still carries the value waired wrote) and its hooks.Stop
// entry. Remove undoes exactly those (the flags only when our loopback base
// URL is present), leaving a pre-existing file otherwise intact.
package claudemanaged

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

const baseURLKey = "ANTHROPIC_BASE_URL"

// discoveryKey turned on Claude Code's gateway model discovery (v2.1.129+):
// at startup it queries {ANTHROPIC_BASE_URL}/v1/models and lists the returned
// ids in the /model picker.
//
// waired no longer writes it (waired-agent#1185). Discovery never ran on a
// subscription-OAuth host in the first place — it is credential-gated and
// waired supplies no credential (#488) — so the flag bought nothing on the
// hosts waired configures, and the Waired rows now come from the documented
// `modelPicker` setting instead. The key is still recognised so Write scrubs
// the value waired used to set and Remove strips it.
//
// The gateway keeps serving /v1/models: OpenCode and anything else pointed at
// the Anthropic-compatible surface still discovers the local catalog there.
const discoveryKey = "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"

// wairedDiscoveryValue is the value waired used to write for discoveryKey.
// Only that exact value is scrubbed: an operator who turned discovery on for
// their own reasons keeps it, the same way autoCompactWindowKey works.
const wairedDiscoveryValue = "1"

// autoCompactWindowKey is Claude Code's highest-precedence auto-compact
// window override: window = min(model window, env value), frozen at process
// start. waired no longer writes it (#771) — see the package comment — but
// still recognizes the key to scrub the legacy value on Write and strip it
// on Remove.
const autoCompactWindowKey = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

// legacyAutoCompactWindowValue is the static value pre-#771 waired wrote for
// autoCompactWindowKey (the ~200k coding floor). Write deletes the key only
// when it still carries exactly this value, so an operator's own deliberate
// override survives an upgrade.
const legacyAutoCompactWindowValue = "200000"

// SubagentModelID was the model id waired pinned as every Claude Code
// subagent's model (#645/#646), so the gateway could tell a subagent's turn
// from the main conversation's by reading the id. It meant nothing to the
// real Anthropic API, so every passthrough leg had to rewrite it back into a
// real model — the one place waired put a model id on the wire the user never
// typed.
//
// waired-agent#1186 retired it: the gateway classifies on the attribution
// header Claude Code already stamps on a subagent's request, and
// CLAUDE_CODE_SUBAGENT_MODEL goes back to meaning what Claude Code documents
// it to mean — where subagents run, chosen by the operator, in their OWN
// settings (internal/integration/claudecode/subagentplacement.go).
//
// The constant stays so Remove can recognise the value waired wrote and take
// it out of a machine that has one.
const SubagentModelID = "waired/subagent"

// subagentModelKey is the Claude Code env var that used to carry the label
// above. waired no longer writes it here; Write scrubs its own value and
// Remove strips it.
const subagentModelKey = "CLAUDE_CODE_SUBAGENT_MODEL"

// maxContextTokensKey is Claude Code's per-session context-window override for
// model ids that do NOT start with "claude-" (verified against v2.1.207): for
// such an id the window is CLAUDE_CODE_MAX_CONTEXT_TOKENS when set, else the
// 200k default. It does NOT touch real "claude-*" ids, so — unlike the #771
// CLAUDE_CODE_AUTO_COMPACT_WINDOW backstop this package deliberately stopped
// writing — it can never cap a genuine 1M Anthropic session. waired writes it
// only for the model-route-directives feature (#52), to give the non-"claude-"
// directive ids ("anthropic-waired-local" / "anthropic-waired-auto") the real
// local window; like every managed env it is frozen at Claude Code process start.
const maxContextTokensKey = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"

// MaxContextTokensKey is maxContextTokensKey for callers that need to name
// the key in a message — `waired claude disable` says which key it left
// behind when it could not confirm the value was ours
// (waired-agent#1174).
const MaxContextTokensKey = maxContextTokensKey

// legacyDirectivesMaxContextTokensValue is the static window pre-#408 waired
// wrote for maxContextTokensKey — "a little under the ~256k local engine
// window", chosen before anything measured the window a host actually serves.
// Write now derives the value per host (WriteOptions.LocalContextWindow) and
// keeps this constant for the two ownership questions it still answers:
// replace it on upgrade, and recognise it as ours when the feature is toggled
// off. Same shape as legacyAutoCompactWindowValue.
const legacyDirectivesMaxContextTokensValue = "250000"

// loopbackPrefix is the signature of a URL waired itself writes. Remove only
// strips ANTHROPIC_BASE_URL when it carries this prefix, so an operator's own
// non-loopback gateway URL is never clobbered by a waired uninstall.
const loopbackPrefix = "http://127.0.0.1:"

// ErrUnsupportedOS is returned by Write on platforms with no known Claude Code
// managed-settings path.
var ErrUnsupportedOS = errors.New("claudemanaged: no managed-settings path for this OS")

// pathResolver yields the managed-settings.json path. It is a package var only
// so tests can redirect writes away from the real root-owned system path; in
// production it always resolves the per-OS location.
var pathResolver = managedSettingsPath

func resolvePath() string { return pathResolver() }

// SwapPathForTest redirects the managed-settings path for the caller's tests and
// returns the restore function.
//
// It exists because the file is machine-global: a package outside this one that
// reads Path() reads the developer's real /etc/claude-code (or
// %ProgramFiles%\ClaudeCode) file, which is the shape of hidden dependency #386
// set out to end — a clean CI runner hides it, and the test only misbehaves on
// the machine editing the code. Seal it in a package's TestMain, not per test.
// Same contract as download.SwapCandidatesForTest.
func SwapPathForTest(path string) (restore func()) {
	prev := pathResolver
	pathResolver = func() string { return path }
	return func() { pathResolver = prev }
}

// Path returns the absolute managed-settings.json path for this OS, or "" when
// unsupported.
func Path() string { return resolvePath() }

// ExpectedBaseURL is the loopback Anthropic base URL waired serves and writes
// into managed settings, derived from the agent's configured
// ClaudeGatewayPort (agent.json over the built-in defaults). The port is
// returned alongside it because callers that want to dial the listener need
// it, and re-parsing the string to get it back is how the two drift.
//
// It lives here, next to ViewAt, because "routed through Waired" is the
// comparison of what the file says against what this agent serves, and one
// definition is the only way those two stay the same question. There used to
// be two — cmd/waired's claudeBaseURL and the management API's
// expectedBaseURL — which is the shape waired-agent#1032 was filed as.
func ExpectedBaseURL(stateDir string) (string, int) {
	c := agentconfig.Defaults()
	_ = c.MergeJSON(agentconfig.JSONPathFor(stateDir))
	port := c.Inference.ClaudeGatewayPort
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}

// WriteOptions carries the feature toggles a managed-settings write depends on
// beyond the base URL.
type WriteOptions struct {
	// ModelRouteDirectives mirrors agentconfig
	// InferenceConfig.ClaudeModelRouteDirectives (#52). When true, Write sets
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS so the non-"claude-" local /model id gets
	// the real local window; when false, Write scrubs the value waired wrote
	// (leaving an operator's own override alone), so toggling the feature off
	// and re-running `waired claude enable` cleans up after itself.
	ModelRouteDirectives bool

	// LocalContextWindow is the effective input-token window local inference
	// can actually serve on this host — the gateway's ContextWindowFor for the
	// claude-route serving model, i.e. the same number /v1/models advertises as
	// max_input_tokens for "anthropic-waired-local" (#408).
	//
	// 0 means "could not be determined" (agent not running, no active model,
	// unknown sizing). With the feature ON that makes Write leave any existing
	// value untouched — silence beats replacing one unverifiable number with
	// another. Callers should resolve it even when ModelRouteDirectives is
	// false: the feature-off scrub identifies waired's value by matching it,
	// so it needs to know what this host would have written.
	LocalContextWindow int

	// PeerContextWindow is the smallest input-token window among the
	// computers this one can currently reach, and it is used ONLY when
	// LocalContextWindow is 0 (waired-agent#1246).
	//
	// A computer with no engine of its own resolved no local window, so
	// nothing was written at all — and Claude Code then assumed its own
	// 200k default AND showed "isn't described by this version's model
	// catalog" on every Waired row, which is the notice this variable
	// exists to suppress. Every row such a host offers is a peer row, so a
	// peer's window is not an approximation of something better; it is the
	// only honest number available.
	//
	// Smallest rather than largest: over-declaring means a turn is
	// compacted only after the gateway has already refused it. 0 keeps
	// today's behaviour of writing nothing.
	PeerContextWindow int

	// ModelPeerEntries mirrors agentconfig
	// InferenceConfig.ClaudeModelPeerEntries: how many per-computer rows the
	// /model picker cache should carry (waired-agent#830). Write itself does
	// nothing with it — managed settings hold no picker entries — but it rides
	// here because applyClaudeRoute already threads these options to the
	// per-user cache write, and a second parallel path for one integer is how
	// the two end up disagreeing about what was configured.
	ModelPeerEntries int
}

// DeclaredContextWindow is the number this host tells Claude Code to size a
// session with: its own window when it has an engine, and otherwise the
// smallest one it can reach. 0 = nothing to declare, and nothing is written.
func (o WriteOptions) DeclaredContextWindow() int {
	return declaredContextWindow(o.LocalContextWindow, o.PeerContextWindow)
}

// RemoveOptions carries what Remove needs beyond the file itself.
type RemoveOptions struct {
	// LocalContextWindow has the same meaning and the same source as
	// WriteOptions.LocalContextWindow; Remove uses it only to recognise a
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS value as waired-written. 0 (agent already
	// stopped by the time disable runs, say) falls back to recognising the
	// pre-#408 static constant, and a host-derived value then survives the
	// disable — inert, because the loopback base URL that gave the
	// non-"claude-" directive ids their meaning goes with it.
	LocalContextWindow int

	// PeerContextWindow has the same meaning and the same source as
	// WriteOptions.PeerContextWindow, and matters here for the same reason
	// LocalContextWindow does: a value this host wrote from a peer's window
	// has to be recognisable as ours, or disable leaves it behind
	// (waired-agent#1246, the shape waired-agent#1174 warns about).
	PeerContextWindow int
}

// DeclaredContextWindow is RemoveOptions' half of the same question: which
// number would this host have written, so a scrub can recognise it.
func (o RemoveOptions) DeclaredContextWindow() int {
	return declaredContextWindow(o.LocalContextWindow, o.PeerContextWindow)
}

// declaredContextWindow prefers this host's own window and falls back to the
// reachable one. Local first is the point: on a host that serves, its own
// number is exact for the row most people use, and the peer rows are the
// approximation the gateway's own 400 catches
// (docs/decisions/20260906/0415-the-declared-window-falls-back-to-a-peers.md).
func declaredContextWindow(local, peer int) int {
	if local > 0 {
		return local
	}
	return peer
}

// wairedOwnedMaxContextTokens reports whether cur is a
// CLAUDE_CODE_MAX_CONTEXT_TOKENS value waired wrote, so a scrub leaves an
// operator's own override in place. Two shapes qualify: the pre-#408 static
// constant, and the window this host resolves right now.
//
// This cannot recognise a value written for a DIFFERENT serving model than the
// one running at scrub time. The alternative — stamping an ownership marker
// into a file operators and MDM also own — is worse than leaving one inert key
// behind in that case.
func wairedOwnedMaxContextTokens(cur string, window int) bool {
	if cur == legacyDirectivesMaxContextTokensValue {
		return true
	}
	return window > 0 && cur == strconv.Itoa(window)
}

// Write merges env.ANTHROPIC_BASE_URL=baseURL into the OS
// managed-settings.json (creating it and its parent dir if needed),
// preserving every other key. No credential variable is written. Returns
// the path written. It is WriteWithOptions with all feature toggles off — the
// common enable path.
func Write(baseURL string) (string, error) {
	return WriteWithOptions(baseURL, WriteOptions{})
}

// WriteWithOptions is Write with the caller's resolved feature toggles (#52).
func WriteWithOptions(baseURL string, opts WriteOptions) (string, error) {
	path := resolvePath()
	if path == "" {
		return "", ErrUnsupportedOS
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("claudemanaged: mkdir %s: %w", filepath.Dir(path), err)
	}
	obj, err := readObject(path)
	if err != nil {
		return "", err
	}
	// waired-agent#1188: read before writing. On a machine an organisation
	// already manages, pointing ANTHROPIC_BASE_URL at the local gateway is
	// not one setting among others — a non-default base URL is documented as
	// a way server-managed settings are BYPASSED, so the write would switch
	// off centrally delivered policy for every user of the machine
	// (https://code.claude.com/docs/en/server-managed-settings). Refuse, and
	// say what was found; there is no safe "write anyway", because what would
	// be overwritten is the mechanism by which the owner would say no.
	//
	// Deliberately checked on the object just read, not by a second read:
	// two reads is a window in which the answer can change.
	if sig := orgSignalsIn(obj); len(sig) > 0 {
		return "", &ErrOrgManaged{Path: path, Signals: sig}
	}
	env, _ := obj["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env[baseURLKey] = baseURL
	// waired-agent#1185: the discovery flag is no longer written. It never
	// did anything on a subscription-OAuth host — discovery is
	// credential-gated and waired supplies none — and the Waired /model rows
	// come from the `modelPicker` setting now. Scrub the value pre-#1185
	// waired wrote; an operator who turned discovery on themselves keeps it.
	if cur, ok := env[discoveryKey].(string); ok && cur == wairedDiscoveryValue {
		delete(env, discoveryKey)
	}
	// #771: the static auto-compact window backstop is gone — it capped 1M
	// Anthropic sessions at 200k while the per-request 400 overflow guard
	// already protects sub-200k local windows. Scrub the value a pre-#771
	// waired wrote; an operator's own different value is left alone.
	if cur, ok := env[autoCompactWindowKey].(string); ok && cur == legacyAutoCompactWindowValue {
		delete(env, autoCompactWindowKey)
	}
	// waired-agent#1186: the subagent label is not written any more. Scrub
	// the value waired itself wrote, so a machine upgrading past it stops
	// carrying an id nothing understands; an operator's own choice of
	// subagent model — any other value — is left exactly where it is.
	if cur, ok := env[subagentModelKey].(string); ok && cur == SubagentModelID {
		delete(env, subagentModelKey)
	}
	// #52/#408: size the non-"claude-" local /model directive id via
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS when the feature is on, from the window
	// this host actually serves. Overwritten unconditionally like the base URL
	// and the subagent label — the key exists because waired introduced it and
	// means nothing without waired's directive ids. An unresolved window is the
	// one case we do not write: a stale honest number beats a fresh guess, and
	// `waired claude status` reports the gap. Scrub our value when the feature
	// is off (an operator's own override survives; see
	// wairedOwnedMaxContextTokens for what "ours" can and cannot recognise).
	window := opts.DeclaredContextWindow()
	switch {
	case opts.ModelRouteDirectives && window > 0:
		env[maxContextTokensKey] = strconv.Itoa(window)
	case opts.ModelRouteDirectives:
		// Window unknown — leave whatever the file carries untouched.
	default:
		if cur, ok := env[maxContextTokensKey].(string); ok && wairedOwnedMaxContextTokens(cur, window) {
			delete(env, maxContextTokensKey)
		}
	}
	obj["env"] = env

	// Take the retired Stop hook with us: a host upgrading from a build that
	// installed it would otherwise keep running `waired claude _fallback-hook`
	// on every turn-end, and that command is gone
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	removeStopHook(obj)

	// The SessionStart hook that keeps the /model picker entries current
	// (waired-agent#830). Gated on the same flag that advertises the ids at
	// all: with directives off there is no cache to maintain, and the enable
	// path removes the file instead, so a hook rewriting it would be
	// maintaining something nothing offers.
	if opts.ModelRouteDirectives {
		ensureRefreshHook(runtime.GOOS, obj, opts.ModelPeerEntries)
	} else {
		removeRefreshHook(obj)
	}

	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", fmt.Errorf("claudemanaged: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := secrets.WriteFile(path, data, secrets.NonSecret); err != nil {
		return "", fmt.Errorf("claudemanaged: write %s: %w", path, err)
	}
	return path, nil
}

// SetMaxContextTokens writes env.CLAUDE_CODE_MAX_CONTEXT_TOKENS into an
// EXISTING managed-settings file, leaving every other key — the base URL, the
// discovery flag, the subagent label, hooks.Stop, and whatever an operator or an
// MDM put there — exactly as it found them. It reports whether the file was
// rewritten.
//
// It exists because the browser wizard applies the Claude Code route BEFORE the
// model download (waired-agent#311 moved it there deliberately: the one step
// that needs a person should not sit behind the longest unattended wait). At
// that moment there is no serving model, so WriteOptions.LocalContextWindow is
// 0, Write correctly declines to guess — a stale honest number beats a fresh
// guess — and the key is simply absent. `waired claude status` then reported
// "(managed settings: not set)" on every wizard-driven install
// (waired-agent#796). This is the top-up once the model is ready and /v1/models
// can answer, not a change to that declining.
//
// Deliberately narrow: it never creates the file (a host that was never routed
// gets nothing), never writes a base URL or a hook, does nothing for a window
// <= 0, and does nothing when the file already carries that exact value.
func SetMaxContextTokens(window int) (bool, error) {
	return SetMaxContextTokensAt(resolvePath(), window)
}

// SetMaxContextTokensAt is SetMaxContextTokens against an explicit path, the
// #604 reason ViewAt exists: a caller outside this package must be able to point
// it somewhere other than the real root-owned file.
func SetMaxContextTokensAt(path string, window int) (bool, error) {
	if path == "" || window <= 0 {
		return false, nil
	}
	obj, _, err := readSettingsObject(path)
	if err != nil {
		if errors.Is(err, errSettingsUnparseable) {
			// An operator's unparseable file is not ours to rewrite; the same
			// posture every other reader here takes.
			return false, nil
		}
		return false, err
	}
	if obj == nil {
		return false, nil
	}
	env, _ := obj["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	want := strconv.Itoa(window)
	if cur, ok := env[maxContextTokensKey].(string); ok && cur == want {
		return false, nil
	}
	env[maxContextTokensKey] = want
	obj["env"] = env
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return false, fmt.Errorf("claudemanaged: marshal: %w", err)
	}
	if err := secrets.WriteFile(path, append(data, '\n'), secrets.NonSecret); err != nil {
		return false, fmt.Errorf("claudemanaged: write %s: %w", path, err)
	}
	return true, nil
}

// Remove is RemoveWithOptions with no resolved local window — the caller
// either has no agent to ask or does not care. See RemoveOptions for what that
// costs.
func Remove() (bool, error) { return RemoveWithOptions(RemoveOptions{}) }

// RemoveWithOptions strips env.ANTHROPIC_BASE_URL (only when it points at
// waired's loopback listener) from managed-settings.json, cleaning up an
// emptied env / object / file. It is a no-op (removed=false) when the file is
// absent, unparseable, or the key is missing or operator-owned. Best-effort: a
// pre-existing operator file with other keys is left otherwise untouched.
func RemoveWithOptions(opts RemoveOptions) (bool, error) {
	path := resolvePath()
	if path == "" {
		return false, nil
	}
	obj, _, err := readSettingsObject(path)
	if err != nil || obj == nil {
		return false, nil // absent / not ours / unparseable — leave it alone
	}
	removed := false
	// Strip our loopback ANTHROPIC_BASE_URL (only when it is ours) together
	// with the #623 discovery flag we co-write with it, preserving an
	// operator-owned non-loopback URL and any other env keys. The legacy
	// auto-compact window (no longer written since #771) is stripped only
	// when it still carries the exact value pre-#771 waired wrote, so an
	// operator's own override survives a disable. The subagent label (#646)
	// has the same ownership-guard shape.
	if env, ok := obj["env"].(map[string]any); ok {
		if cur, ok := env[baseURLKey].(string); ok && strings.HasPrefix(cur, loopbackPrefix) {
			delete(env, baseURLKey)
			if cur, ok := env[discoveryKey].(string); ok && cur == wairedDiscoveryValue {
				delete(env, discoveryKey)
			}
			if cur, ok := env[autoCompactWindowKey].(string); ok && cur == legacyAutoCompactWindowValue {
				delete(env, autoCompactWindowKey)
			}
			// #52/#408: scrub our max-context-tokens value (an operator's own
			// override — any other value — is preserved). Since the value is
			// host-derived, "ours" depends on which number this host would
			// have written — its own window, or a reachable one when it has
			// no engine (waired-agent#1246).
			if cur, ok := env[maxContextTokensKey].(string); ok && wairedOwnedMaxContextTokens(cur, opts.DeclaredContextWindow()) {
				delete(env, maxContextTokensKey)
			}
			removed = true
		}
		if cur, ok := env[subagentModelKey].(string); ok && cur == SubagentModelID {
			delete(env, subagentModelKey)
			removed = true
		}
		if removed {
			if len(env) == 0 {
				delete(obj, "env")
			} else {
				obj["env"] = env
			}
		}
	}
	// Strip our Stop hook (#580) independently of the base URL, so it is cleaned
	// up even if an operator has since repointed ANTHROPIC_BASE_URL.
	if removeStopHook(obj) {
		removed = true
	}
	// Same for the SessionStart picker-cache refresh (waired-agent#830).
	if removeRefreshHook(obj) {
		removed = true
	}
	if !removed {
		return false, nil // nothing of ours present
	}
	if len(obj) == 0 {
		// waired's key was the file's only content — drop the file.
		if err := os.Remove(path); err != nil {
			return false, err
		}
		return true, nil
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return false, fmt.Errorf("claudemanaged: marshal: %w", err)
	}
	if err := secrets.WriteFile(path, append(data, '\n'), secrets.NonSecret); err != nil {
		return false, err
	}
	return true, nil
}

// View reports the managed-settings state for the management API / tray: the
// resolved path, whether the file exists, and the ANTHROPIC_BASE_URL it carries
// (empty if unset or unparseable).
func View() (path string, present bool, baseURL string) {
	path = resolvePath()
	present, baseURL = ViewAt(path)
	return path, present, baseURL
}

// ViewAt is View against an explicit path, so callers outside this package can
// point the view at a non-system location (#604 — tests must not read the real
// root-owned file). An empty path (unsupported OS) reports absent.
func ViewAt(path string) (present bool, baseURL string) {
	present, baseURL, _ = ViewDetailAt(path)
	return present, baseURL
}

// ViewDetailAt is ViewAt plus the state the two-value form cannot express: a
// file that is there and cannot be read as JSON. Its baseURL is "" like a file
// that simply sets nothing, and telling a reader those apart is the difference
// between "waired is not routing Claude Code" and "waired cannot tell, and here
// is why" (waired-agent#1067).
func ViewDetailAt(path string) (present bool, baseURL string, err error) {
	obj, present, err := readSettingsObject(path)
	if err != nil || obj == nil {
		return present, "", err
	}
	return present, envString(obj, baseURLKey), nil
}

// SubagentModelAt reports the CLAUDE_CODE_SUBAGENT_MODEL value in the
// managed-settings file at path ("" when absent / unparseable / unset) —
// the #646 counterpart to ViewAt's base-URL view, kept as a separate
// helper so ViewAt's signature stays stable for its callers.
func SubagentModelAt(path string) string { return envStringAt(path, subagentModelKey) }

// MaxContextTokensAt reports the CLAUDE_CODE_MAX_CONTEXT_TOKENS value in the
// managed-settings file at path ("" when absent / unparseable / unset). It
// exists so `waired claude status` can show the window Claude Code will be
// started with next to the one local inference actually serves: since #408 the
// two can disagree (a serving-model change after the last elevated write), and
// a silent disagreement is exactly the failure #408 set out to end.
func MaxContextTokensAt(path string) string { return envStringAt(path, maxContextTokensKey) }

// envStringAt reads one env value out of the managed-settings file at path,
// returning "" for every "not there" case (no path, unreadable, unparseable,
// key absent, non-string value) — these accessors are display helpers and must
// never turn a malformed operator file into an error.
func envStringAt(path, key string) string {
	obj, _, err := readSettingsObject(path)
	if err != nil || obj == nil {
		return ""
	}
	return envString(obj, key)
}

// readObject parses path as a JSON object, returning an empty map when the file
// is absent or blank. A non-object / malformed file is an error so Write does
// not silently discard operator content.
func readObject(path string) (map[string]any, error) {
	obj, _, err := readSettingsObject(path)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return map[string]any{}, nil
	}
	return obj, nil
}
