package claudecode

// Where a Claude Code subagent's turn runs (waired-agent#1186).
//
// waired used to answer this by pinning a model id of its own —
// `waired/subagent` — as CLAUDE_CODE_SUBAGENT_MODEL in machine-wide managed
// settings, purely so the gateway could tell a subagent's turn from the main
// conversation's. The id meant nothing to the real Anthropic API, so every
// passthrough leg had to rewrite it back into a real model before the request
// could leave. That was the one place waired put a model id on the wire the
// user never typed.
//
// Claude Code documents the same variable as the way to CHOOSE where
// subagents run (https://code.claude.com/docs/en/sub-agents), and stamps its
// own attribution header on a subagent's request, which is what the gateway
// classifies on now. So the variable goes back to meaning what it says, and
// waired writes it only when the operator asks for it.
//
// Two values, and no third. Either subagents follow whatever Claude Code
// resolves for them — the main model, or a model their own definition pins —
// or they all run on Waired. "Main on Waired, subagents on Anthropic" is not
// a switch: someone who wants that pins a real Anthropic model in the agent
// definition, which is the documented way and already works
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 6).
//
// It writes the USER's settings.json, not managed settings. The switch is a
// per-person preference, it needs no elevation, and writing it machine-wide
// would override a value an organisation may have set for everyone.

import (
	"encoding/json"
	"fmt"
)

const (
	subagentModelEnvKey = "CLAUDE_CODE_SUBAGENT_MODEL"
	// subagentForceEnvKey is what makes the choice hold. Without it an agent
	// definition's own `model:` wins and that subagent runs wherever its
	// definition says — measured against Claude Code 2.1.261 on 2026-09-06:
	// with a definition pinning claude-opus-4-8 and the variable naming
	// `waired`, the subagent's request carried claude-opus-4-8; with
	// _FORCE=1 it carried `waired`. So "subagents on Waired" that did not
	// write this would quietly not apply to the agents most likely to have
	// been given a model on purpose.
	subagentForceEnvKey = "CLAUDE_CODE_SUBAGENT_MODEL_FORCE"
	envKey              = "env"
)

// SubagentPlacement is where subagents run.
type SubagentPlacement int

const (
	// SubagentFollow: waired sets nothing. A subagent carries whatever
	// Claude Code resolved for it and runs where that id says.
	SubagentFollow SubagentPlacement = iota
	// SubagentWaired: every subagent runs on Waired.
	SubagentWaired
	// SubagentForeign: the variable is set to something that is not one of
	// waired's ids. Somebody else chose that, so waired reports it and
	// leaves it alone.
	SubagentForeign
	// SubagentUnreadable: the settings file is not JSON waired can parse.
	SubagentUnreadable
)

// DetectSubagentPlacement reads the switch out of a settings file, and
// returns the value found when it is not ours to change.
func DetectSubagentPlacement(path string) (SubagentPlacement, string) {
	m, err := readSettings(path)
	if err != nil {
		return SubagentUnreadable, ""
	}
	env, ok := settingsEnv(m)
	if !ok {
		return SubagentUnreadable, ""
	}
	cur := env[subagentModelEnvKey]
	switch {
	case cur == "":
		return SubagentFollow, ""
	case IsWairedModelID(cur):
		return SubagentWaired, cur
	default:
		return SubagentForeign, cur
	}
}

// SetSubagentPlacement writes the switch, and reports whether the file
// changed. A foreign value is refused rather than replaced.
//
// wairedID is the Waired model id subagents should carry — the any-node row,
// resolved by the caller so this package does not have to know which
// spellings this build offers.
func SetSubagentPlacement(path string, want SubagentPlacement, wairedID string) (changed bool, err error) {
	switch cur, val := DetectSubagentPlacement(path); cur {
	case SubagentUnreadable:
		return false, fmt.Errorf("claudecode: %s is not settings waired can read; "+
			"waired left it alone", path)
	case SubagentForeign:
		return false, fmt.Errorf("claudecode: %s already sets %s=%s; waired left it alone",
			path, subagentModelEnvKey, val)
	}
	if want == SubagentWaired && wairedID == "" {
		return false, fmt.Errorf("claudecode: no Waired model id to point subagents at")
	}

	m, err := readSettings(path)
	if err != nil {
		return false, err
	}
	env, ok := settingsEnv(m)
	if !ok {
		return false, fmt.Errorf("claudecode: %s has an %q that is not an object", path, envKey)
	}
	before := len(env)
	prevModel, prevForce := env[subagentModelEnvKey], env[subagentForceEnvKey]
	if want == SubagentWaired {
		env[subagentModelEnvKey] = wairedID
		env[subagentForceEnvKey] = "1"
	} else {
		delete(env, subagentModelEnvKey)
		// The force flag is only ever ours: it does nothing without the
		// model, and waired is what wrote the pair. Removing it with the
		// model is what keeps "follow" honest.
		delete(env, subagentForceEnvKey)
	}
	if env[subagentModelEnvKey] == prevModel && env[subagentForceEnvKey] == prevForce &&
		len(env) == before {
		return false, nil
	}
	if len(env) == 0 {
		delete(m, envKey)
	} else {
		enc, err := json.Marshal(env)
		if err != nil {
			return false, fmt.Errorf("claudecode: encode %s: %w", envKey, err)
		}
		m[envKey] = enc
	}
	return true, writeSettings(path, m)
}

// settingsEnv decodes the settings file's `env` block as a flat string map,
// which is the only shape Claude Code accepts there. ok=false means the key
// is present and is something else, which waired will not overwrite.
func settingsEnv(m map[string]json.RawMessage) (map[string]string, bool) {
	raw, present := m[envKey]
	if !present {
		return map[string]string{}, true
	}
	env := map[string]string{}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}
	return env, true
}
