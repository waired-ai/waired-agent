//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
)

// legs is the ordered leg table. Adding a coding agent = one entry here.
//
// It takes the Env so every leg can carry the id it drives as a plain
// string: the assertion compares the daemon's record of the last requested
// model against it, and a record left by the PREVIOUS leg must not be able
// to satisfy this one.
func legs(e Env) []Leg {
	return []Leg{claudeLeg(e), claudeRealAnthropicIDLeg(), claudeUnresolvableIDLeg(), openCodeLeg(e), openClawLeg(e)}
}

// claudeLeg drives the Claude managed-settings loopback proxy (:9472). No
// configure step: the intercept proxy IS the surface Claude Code's
// ANTHROPIC_BASE_URL points at, so a curl there exercises the real
// intercept → gateway → model path (and the fail-open guard).
//
// The tiny alias resolves in the catalog (proto/catalog/bundled names it),
// so this leg is the ordinary local path and never reaches the
// unknown-model mapping — that is claudeUnresolvableIDLeg's job.
func claudeLeg(e Env) Leg {
	return Leg{
		Name:       "claude",
		ExpectKind: "anthropic",
		Expect:     outcomeLocal,
		Model:      e.TinyAlias,
		Drive: func(ctx context.Context, e Env) (driveResponse, error) {
			return driveAnthropic(ctx, e.ClaudeURL, e.TinyAlias)
		},
	}
}

// claudeRealAnthropicIDLeg proves the OPPOSITE of what it used to: a model
// id the real Anthropic API serves pins the turn upstream, overriding the
// per-class policy in both directions, because naming a model in /model is
// naming where it runs (waired-agent#1091, owner ruling 2026-08-28).
//
// It asserted local serving until then — the #600 mapping — and #1091
// changed what it watches without touching it. The Linux lane stayed green
// throughout: the blackhole makes the now-correct upstream route
// unreachable, #665 degrades the turn back to local, and the ring records
// decision=local. So this leg is checked against the daemon's routing
// record rather than against the transport (waired-agent#1141).
//
// The "[1m]" suffix is load-bearing here too: normalizeModelID strips it
// before the id is recognised as Anthropic-owned at all.
func claudeRealAnthropicIDLeg() Leg {
	const model = "claude-fable-5[1m]"
	return Leg{
		Name:       "claude-real-anthropic-id",
		ExpectKind: "anthropic",
		Expect:     outcomeUpstream,
		Model:      model,
		Drive: func(ctx context.Context, e Env) (driveResponse, error) {
			return driveAnthropic(ctx, e.ClaudeURL, model)
		},
	}
}

// claudeUnresolvableIDLeg keeps the #600 mapping covered. The Anthropic ids
// Claude Code sends name no catalog model, so an alias miss must resolve to
// something servable instead of 404ing into the auto-fallback (the
// local_status_404 class).
//
// It exists because rewriting claudeRealAnthropicIDLeg against #1091 would
// otherwise take that coverage to ZERO: the tiny alias resolves in the
// catalog, so no other leg reaches ResolveUnknownModel, and after #1091 the
// real-id leg only did so on the one lane where the blackhole forced the
// degrade — by accident of the test rig.
//
// The subagent label is the realistic unresolvable id on this surface: it
// is what managed settings pin Claude Code's subagents to (#646), it is
// never in the catalog, and it is not Anthropic-owned, so it follows the
// per-class policy to local rather than being pinned upstream.
func claudeUnresolvableIDLeg() Leg {
	return Leg{
		Name:       "claude-unresolvable-id",
		ExpectKind: "anthropic",
		Expect:     outcomeLocal,
		Model:      claudemanaged.SubagentModelID,
		// The daemon classifies this id as subagent traffic and
		// deliberately keeps it out of the routing record, so the record
		// must not be read for this leg. The fail-open header check still
		// applies, which is what keeps it from going blind under the
		// blackhole.
		SubagentClass: true,
		Drive: func(ctx context.Context, e Env) (driveResponse, error) {
			return driveAnthropic(ctx, e.ClaudeURL, claudemanaged.SubagentModelID)
		},
	}
}

// openCodeLeg writes the real OpenCode provider plugin (proving the config
// surface that the "Provider not found" / #481 class breaks) into an isolated
// HOME, then drives the OpenAI-compatible request the plugin targets against
// the local gateway (:9473).
func openCodeLeg(e Env) Leg {
	return Leg{
		Name:       "opencode",
		ExpectKind: "openai",
		Expect:     outcomeLocal,
		Model:      e.TinyAlias,
		Configure: func(ctx context.Context, e Env) (func(), error) {
			home, cleanup, err := writeAgentConfig(ctx, opencode.New(), e)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(opencode.PluginFile(home)); err != nil {
				cleanup()
				return nil, fmt.Errorf("opencode plugin not written: %w", err)
			}
			return cleanup, nil
		},
		Drive: func(ctx context.Context, e Env) (driveResponse, error) {
			return driveOpenAI(ctx, e.DataPlaneURL, e.TinyAlias)
		},
	}
}

// openClawLeg writes the real OpenClaw provider plugin + openclaw.json (proving
// that config surface) into an isolated HOME, then drives the OpenAI-compatible
// request the plugin targets against the same no-token data-plane gateway.
// (OpenClaw is not bundled; the real openclaw binary end-to-end is #518.)
func openClawLeg(e Env) Leg {
	return Leg{
		Name:       "openclaw",
		ExpectKind: "openai",
		Expect:     outcomeLocal,
		Model:      e.TinyAlias,
		Configure: func(ctx context.Context, e Env) (func(), error) {
			home, cleanup, err := writeAgentConfig(ctx, openclaw.New(), e)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(openclaw.PluginEntryFile(home)); err != nil {
				cleanup()
				return nil, fmt.Errorf("openclaw plugin not written: %w", err)
			}
			return cleanup, nil
		},
		Drive: func(ctx context.Context, e Env) (driveResponse, error) {
			return driveOpenAI(ctx, e.DataPlaneURL, e.TinyAlias)
		},
	}
}

// writeAgentConfig runs a tool adapter's real Apply into a throwaway HOME so the
// config-writer (plugin render, openclaw.json merge) is exercised end to end,
// pointed at the loopback gateway. Returns the HOME and a cleanup.
func writeAgentConfig(ctx context.Context, a integration.Adapter, e Env) (string, func(), error) {
	home, err := os.MkdirTemp("", "waired-integ-"+string(a.ID())+"-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	// Always use a throwaway state dir under the temp HOME — never the real
	// (root-owned) daemon state dir: this test runs as the unprivileged CI
	// user, and the adapters only write a plugin + a dummy gateway token here
	// (the plugins target the no-token local gateway, so the token value is
	// irrelevant to routing).
	stateDir := filepath.Join(home, ".config", "waired")
	if err := a.Apply(ctx, integration.ApplyOptions{
		HomeDir:        home,
		StateDir:       stateDir,
		GatewayBaseURL: "http://127.0.0.1:9473",
		// A non-empty token is required by Apply; the gateway the plugins
		// target is no-token, so the value is irrelevant to routing.
		Force:          true,
		NonInteractive: true,
	}); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s Apply: %w", a.ID(), err)
	}
	return home, cleanup, nil
}
