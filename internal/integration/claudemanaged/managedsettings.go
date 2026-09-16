// Package claudemanaged writes Claude Code "managed settings" so the local
// waired agent can route Claude Code at its loopback gateway without any MITM
// proxy, CA, /etc/hosts edit, or shell-env management (#488).
//
// It sets env.ANTHROPIC_BASE_URL — pointing at waired's plain-HTTP loopback
// Anthropic listener (127.0.0.1:ClaudeGatewayPort) — and, when the
// model-route directives are on, env.CLAUDE_CODE_MAX_CONTEXT_TOKENS (below).
// The discovery flag #623 used to co-write here,
// env.CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1, is retired: the /model
// rows come from the documented modelPicker setting instead
// (waired-agent#1185), and Write only scrubs the flag from earlier installs.
// It deliberately writes NO credential variable. Per the Claude Code docs, a base-URL-only managed setting (no auth
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
// writing it — the gateway's per-request context-overflow 400
// (internal/gateway/anthropic.go, carrying the documented
// `capability_rejected: prompt_too_long` token since waired-agent#1187)
// remains the invariant that holds a turn on a Waired row to its window, and
// Claude Code's own per-model resolution governs a turn on an Anthropic
// model, tracking /model switches mid-session. Write scrubs the legacy value
// from earlier installs.
//
// The model-route-directives feature (#52), when opted in, additionally writes
// env.CLAUDE_CODE_MAX_CONTEXT_TOKENS. That override is honoured ONLY for model
// ids not starting with "claude-", so it sizes the Waired rows (`waired`,
// `waired/local`, `waired/peer`, … — none starts with "claude-" since
// waired-agent#1185; the older `anthropic-waired-*` spellings are honoured as
// legacy ids) while never touching real "claude-*" ids — categorically different from the #771 auto-compact
// backstop that capped 1M Anthropic sessions. On by default (opt-out via
// agentconfig); WriteWithOptions gates the actual write.
//
// That value is 200704 on every host (DirectivesMaxContextTokensValue). Every
// Waired row without "[1m]" is a 200k session whichever computer answers it,
// and the computer that answers has to hold one (owner decision 2026-09-16,
// waired-agent#1396; gateway.RequiredWindowFor). The "[1m]" rows are sized by
// Claude Code itself from the id. There is one variable for every row, so one
// number is the only one that can be true of all of them.
//
// It used to be the window this host served (#408), and the smallest one it
// could reach when it had no engine (waired-agent#1246). That number was exact
// for the local row only, could not follow a model switch — only an elevated
// process may write this file, and Claude Code reads env once at start — and
// was 0, so nothing was written, whenever the agent could not be asked. Before
// #408 it was a static 250000.
//
// Writing stays the elevated CLI's job, never the daemon's
// (docs/decisions/20260728/1444-init-daemon-path-owns-claude-routing.md §4,
// waired#935 — the daemon runs as a service account behind an unauthenticated
// local IPC socket, so writing this admin-owned file would make it a
// privilege bridge).
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
	"io/fs"
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
// only for the model-route-directives feature (#52), to size the Waired rows;
// like every managed env it is frozen at Claude Code process start.
const maxContextTokensKey = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"

// MaxContextTokensKey is maxContextTokensKey for callers that need to name
// the key in a message — `waired claude disable` says which key it left
// behind when it could not confirm the value was ours
// (waired-agent#1174).
const MaxContextTokensKey = maxContextTokensKey

// DirectivesMaxContextTokensValue is what Write puts in maxContextTokensKey:
// the 200k session every Waired row without "[1m]" is (hostfit's
// ServingWindow200k; owner decision 2026-09-16, waired-agent#1396). The
// uninstall scripts carry the same literal.
const DirectivesMaxContextTokensValue = "200704"

// legacyDirectivesMaxContextTokensValue is the static window pre-#408 waired
// wrote for maxContextTokensKey — "a little under the ~256k local engine
// window", chosen before anything measured the window a host actually serves.
// It stays so a value an old install left is recognised as ours. Same shape
// as legacyAutoCompactWindowValue.
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
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS to DirectivesMaxContextTokensValue; when
	// false, Write scrubs the value waired wrote (leaving an operator's own
	// override alone), so toggling the feature off and re-running `waired
	// claude enable` cleans up after itself.
	ModelRouteDirectives bool

	// PriorContextWindow is what a build before waired-agent#1396 would have
	// written on this host — its own serving window, or the smallest one it
	// could reach when it had no engine — so a scrub recognises that value as
	// ours too. 0 means unknown, and then only the fixed values are
	// recognised. It is never written.
	PriorContextWindow int

	// ModelPeerEntries mirrors agentconfig
	// InferenceConfig.ClaudeModelPeerEntries: how many per-computer rows the
	// /model picker cache should carry (waired-agent#830). Write itself does
	// nothing with it — managed settings hold no picker entries — but it rides
	// here because applyClaudeRoute already threads these options to the
	// per-user cache write, and a second parallel path for one integer is how
	// the two end up disagreeing about what was configured.
	ModelPeerEntries int
}

// RemoveOptions carries what Remove needs beyond the file itself.
type RemoveOptions struct {
	// PriorContextWindow has the same meaning and the same source as
	// WriteOptions.PriorContextWindow. 0 (agent already stopped by the time
	// disable runs, say) recognises only the fixed values, and a value an
	// older build derived from this host then survives the disable
	// (waired-agent#1174).
	PriorContextWindow int
}

// wairedOwnedMaxContextTokens reports whether cur is a
// CLAUDE_CODE_MAX_CONTEXT_TOKENS value waired wrote, so a scrub leaves an
// operator's own override in place. Three shapes qualify: the value this build
// writes, the pre-#408 static constant, and the window an older build would
// have derived on this host (prior).
//
// The last cannot recognise a value written for a DIFFERENT serving model than
// the one running at scrub time. The alternative — stamping an ownership
// marker into a file operators and MDM also own — is worse than leaving one
// inert key behind in that case.
func wairedOwnedMaxContextTokens(cur string, prior int) bool {
	if cur == DirectivesMaxContextTokensValue || cur == legacyDirectivesMaxContextTokensValue {
		return true
	}
	return prior > 0 && cur == strconv.Itoa(prior)
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
	return writeWithOptionsFor(runtime.GOOS, baseURL, opts)
}

// writeWithOptionsFor is WriteWithOptions for goos, which decides the hook
// command's shape. The seam lets a test on any OS produce the bytes a Linux
// host gets, which the awk copy in uninstall.sh has to read (waired-agent#1407).
func writeWithOptionsFor(goos, baseURL string, opts WriteOptions) (string, error) {
	path := resolvePath()
	if path == "" {
		return "", ErrUnsupportedOS
	}
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("claudemanaged: mkdir %s: %w", dir, err)
	}
	// MkdirAll's mode passes through the umask, and sudo keeps a caller's
	// stricter one: under the 027 umask a hardened host gives its
	// administrators, /etc/claude-code came out 0750 root:root and nobody but
	// root could read the file inside — while Claude Code reads it as whoever
	// runs `claude` (waired-agent#1419). Only a directory this call created; one
	// that was already there keeps whatever mode its owner gave it. Windows has
	// no mode bits to fix: the directory inherits Program Files' ACL.
	if errors.Is(statErr, fs.ErrNotExist) && goos != "windows" {
		if err := os.Chmod(dir, 0o755); err != nil {
			return "", fmt.Errorf("claudemanaged: chmod %s: %w", dir, err)
		}
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
	// #52: size the Waired rows via CLAUDE_CODE_MAX_CONTEXT_TOKENS when the
	// feature is on — 200704, the session every row without "[1m]" is
	// (waired-agent#1396). Overwritten unconditionally like the base URL: the
	// key exists because waired introduced it and means nothing without
	// waired's directive ids, and it needs nothing from the agent, so it is
	// written even before anything serves. Scrub our value when the feature
	// is off (an operator's own override survives; see
	// wairedOwnedMaxContextTokens for what "ours" can and cannot recognise).
	if opts.ModelRouteDirectives {
		env[maxContextTokensKey] = DirectivesMaxContextTokensValue
	} else if cur, ok := env[maxContextTokensKey].(string); ok && wairedOwnedMaxContextTokens(cur, opts.PriorContextWindow) {
		delete(env, maxContextTokensKey)
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
		ensureRefreshHook(goos, obj, opts.ModelPeerEntries)
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

// SetMaxContextTokensAt writes DirectivesMaxContextTokensValue into
// env.CLAUDE_CODE_MAX_CONTEXT_TOKENS of an EXISTING managed-settings file at
// path, leaving every other key — the base URL, the hooks, and whatever an
// operator or an MDM put there — exactly as it found them. It reports whether
// the file was rewritten.
//
// It is the top-up for a host whose file does not say 200704 yet: one routed
// before waired-agent#1396, which carries the window that build derived, or
// one routed by the browser wizard before anything served, where that build
// wrote nothing (waired-agent#796). Waired runs it from init, `waired link` and
// doctor's repair, so such a host is corrected without a `waired claude
// enable`. Like Write, it replaces whatever value is there.
//
// Deliberately narrow: it never creates the file (a host that was never routed
// gets nothing), never writes a base URL or a hook, and does nothing when the
// file already carries the value. The path is explicit for the #604 reason
// ViewAt exists: a caller outside this package must be able to point it
// somewhere other than the real root-owned file.
func SetMaxContextTokensAt(path string) (bool, error) {
	if path == "" {
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
	want := DirectivesMaxContextTokensValue
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

// Remove is RemoveWithOptions with no prior window — the caller either has no
// agent to ask or does not care. See RemoveOptions for what that costs.
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
			// #52: scrub our max-context-tokens value (an operator's own
			// override — any other value — is preserved). "Ours" is the
			// fixed value, and the window an older build derived on this
			// host (waired-agent#1246, #1396).
			if cur, ok := env[maxContextTokensKey].(string); ok && wairedOwnedMaxContextTokens(cur, opts.PriorContextWindow) {
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
