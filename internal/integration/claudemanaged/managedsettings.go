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
// ids not starting with "claude-", so it sizes the non-"claude-" local /model
// directive id ("anthropic-waired-local") to an honest ~256k window while never
// touching real "claude-*" ids — categorically different from the #771
// auto-compact backstop that capped 1M Anthropic sessions. On by default
// (opt-out via agentconfig); WriteWithOptions gates the actual write.
//
// It also installs a Stop hook (hooks.Stop) that runs `waired claude
// _fallback-hook` so a post-dispatch fallback to the real Anthropic API is
// visible in the Claude Code TUI (#580; see hook.go). Stop hooks array-merge
// across settings scopes, so a managed entry fires without clobbering the user's
// own hooks.
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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

const baseURLKey = "ANTHROPIC_BASE_URL"

// discoveryKey turns on Claude Code's gateway model discovery (v2.1.129+):
// at startup it queries {ANTHROPIC_BASE_URL}/v1/models and lists the returned
// ids in the /model picker. As of v2.1.207 (verified against the binary) the
// response's max_input_tokens does NOT feed the auto-compact window — the
// window comes from Claude Code's own per-model-id resolution — but the flag
// is kept: the picker entries are useful, and a max_input_tokens-consuming
// capability cache already exists in the binary behind a compile-time-off
// gate, so waired's route-aware /v1/models advertisement starts working the
// release it is enabled.
const discoveryKey = "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"

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

// SubagentModelID is the model id that labels Claude Code subagent
// traffic (#645/#646): managed settings will pin it via
// CLAUDE_CODE_SUBAGENT_MODEL so the gateway can classify requests as
// class "sub" by model id — the only robust signal Claude Code offers.
// The gateway treats any other id (including everything from setups
// that never wrote the label) as class "main". Exported because the
// agent's classifier and the intercept's passthrough rewrite must
// agree on the exact string.
const SubagentModelID = "waired/subagent"

// subagentModelKey is the Claude Code env var that pins every subagent
// spawn's model (resolution position 1 — above per-invocation model
// params and agent frontmatter). Note: an organisation availableModels
// allowlist would silently skip an unknown alias; waired does not set
// one.
const subagentModelKey = "CLAUDE_CODE_SUBAGENT_MODEL"

// maxContextTokensKey is Claude Code's per-session context-window override for
// model ids that do NOT start with "claude-" (verified against v2.1.207): for
// such an id the window is CLAUDE_CODE_MAX_CONTEXT_TOKENS when set, else the
// 200k default. It does NOT touch real "claude-*" ids, so — unlike the #771
// CLAUDE_CODE_AUTO_COMPACT_WINDOW backstop this package deliberately stopped
// writing — it can never cap a genuine 1M Anthropic session. waired writes it
// only for the model-route-directives feature (#52), to give the non-"claude-"
// local id ("anthropic-waired-local") an honest ~256k window; like every
// managed env it is frozen at Claude Code process start.
const maxContextTokensKey = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"

// directivesMaxContextTokensValue is the window waired advertises for the local
// directive id (#52): a little under the ~256k local engine window so Claude
// Code compacts proactively just before the gateway's per-request overflow 400
// would fire. Treated as waired-owned: set when the feature is on, scrubbed
// (only when it still carries this exact value, mirroring the auto-compact
// backstop scrub) when it is off, so an operator's own override survives.
const directivesMaxContextTokensValue = "250000"

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

// Path returns the absolute managed-settings.json path for this OS, or "" when
// unsupported.
func Path() string { return resolvePath() }

// WriteOptions carries the feature toggles a managed-settings write depends on
// beyond the base URL.
type WriteOptions struct {
	// ModelRouteDirectives mirrors agentconfig
	// InferenceConfig.ClaudeModelRouteDirectives (#52). When true, Write sets
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS so the non-"claude-" local /model id gets
	// an honest ~256k window; when false, Write scrubs the value waired wrote
	// (leaving an operator's own override alone), so toggling the feature off
	// and re-running `waired claude enable` cleans up after itself.
	ModelRouteDirectives bool
}

// Write merges env.ANTHROPIC_BASE_URL=baseURL and the subagent traffic
// label env.CLAUDE_CODE_SUBAGENT_MODEL=SubagentModelID (#646) into the OS
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
	env, _ := obj["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env[baseURLKey] = baseURL
	// #623: populate the /model picker from our route-aware /v1/models. Not a
	// credential, so the subscription auto-mode stays intact (same posture as
	// the base URL).
	env[discoveryKey] = "1"
	// #771: the static auto-compact window backstop is gone — it capped 1M
	// Anthropic sessions at 200k while the per-request 400 overflow guard
	// already protects sub-200k local windows. Scrub the value a pre-#771
	// waired wrote; an operator's own different value is left alone.
	if cur, ok := env[autoCompactWindowKey].(string); ok && cur == legacyAutoCompactWindowValue {
		delete(env, autoCompactWindowKey)
	}
	// Subagent labeling (#646): CLAUDE_CODE_SUBAGENT_MODEL is position 1
	// in Claude Code's subagent model resolution (above per-invocation
	// params and agent frontmatter), so every subagent request carries
	// this id and the gateway can classify it as class "sub". The
	// intercept rewrites the id back to a real Anthropic model on
	// passthrough legs. Unconditional overwrite, like the base URL.
	env[subagentModelKey] = SubagentModelID
	// #52: give the non-"claude-" local /model directive id an honest ~256k
	// window via CLAUDE_CODE_MAX_CONTEXT_TOKENS when the feature is on; scrub
	// our value when it is off (an operator's own override — any other value —
	// survives). Harmless when the feature is off and the key was never set.
	if opts.ModelRouteDirectives {
		env[maxContextTokensKey] = directivesMaxContextTokensValue
	} else if cur, ok := env[maxContextTokensKey].(string); ok && cur == directivesMaxContextTokensValue {
		delete(env, maxContextTokensKey)
	}
	obj["env"] = env

	// Install the Stop hook (#580) so a post-dispatch fallback is visible in the
	// Claude Code TUI. Rides the same merge-safe write as the base URL.
	ensureStopHook(obj)

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

// Remove strips env.ANTHROPIC_BASE_URL (only when it points at waired's loopback
// listener) from managed-settings.json, cleaning up an emptied env / object /
// file. It is a no-op (removed=false) when the file is absent, unparseable, or
// the key is missing or operator-owned. Best-effort: a pre-existing operator
// file with other keys is left otherwise untouched.
func Remove() (bool, error) {
	path := resolvePath()
	if path == "" {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return false, nil // not ours / unparseable — leave it alone
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
			delete(env, discoveryKey)
			if cur, ok := env[autoCompactWindowKey].(string); ok && cur == legacyAutoCompactWindowValue {
				delete(env, autoCompactWindowKey)
			}
			// #52: scrub our max-context-tokens value (an operator's own
			// override — any other value — is preserved).
			if cur, ok := env[maxContextTokensKey].(string); ok && cur == directivesMaxContextTokensValue {
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
	if path == "" {
		return false, ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	present = true
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil {
		return present, ""
	}
	if env, ok := obj["env"].(map[string]any); ok {
		if u, ok := env[baseURLKey].(string); ok {
			baseURL = u
		}
	}
	return present, baseURL
}

// SubagentModelAt reports the CLAUDE_CODE_SUBAGENT_MODEL value in the
// managed-settings file at path ("" when absent / unparseable / unset) —
// the #646 counterpart to ViewAt's base-URL view, kept as a separate
// helper so ViewAt's signature stays stable for its callers.
func SubagentModelAt(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil {
		return ""
	}
	if env, ok := obj["env"].(map[string]any); ok {
		if v, ok := env[subagentModelKey].(string); ok {
			return v
		}
	}
	return ""
}

// readObject parses path as a JSON object, returning an empty map when the file
// is absent or blank. A non-object / malformed file is an error so Write does
// not silently discard operator content.
func readObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claudemanaged: read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("claudemanaged: existing %s is not a JSON object: %w", path, err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}
