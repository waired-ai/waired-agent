//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
)

// legs is the ordered leg table. Adding a coding agent = one entry here.
func legs() []Leg {
	return []Leg{claudeLeg(), claudeModelMapLeg(), openCodeLeg(), openClawLeg()}
}

// claudeLeg drives the Claude managed-settings loopback proxy (:9472). No
// configure step: the intercept proxy IS the surface Claude Code's
// ANTHROPIC_BASE_URL points at, so a curl there exercises the real
// intercept → gateway → model path (and the fail-open guard).
func claudeLeg() Leg {
	return Leg{
		Name:       "claude",
		ExpectKind: "anthropic",
		Drive: func(ctx context.Context, e Env) (int, []byte, error) {
			return driveAnthropic(ctx, e.ClaudeURL, e.TinyAlias)
		},
	}
}

// claudeModelMapLeg proves the Claude surface serves locally when the client
// sends a real Anthropic model id that is never in the catalog: the gateway
// must map it to the device-active model (#600) instead of 404ing into the
// auto-fallback (the local_status_404 class). Any catalog-unresolvable id
// exercises the path; the realistic literal also pins suffix handling ("[1m]").
func claudeModelMapLeg() Leg {
	return Leg{
		Name:       "claude-anthropic-model-id",
		ExpectKind: "anthropic",
		Drive: func(ctx context.Context, e Env) (int, []byte, error) {
			return driveAnthropic(ctx, e.ClaudeURL, "claude-fable-5[1m]")
		},
	}
}

// openCodeLeg writes the real OpenCode provider plugin (proving the config
// surface that the "Provider not found" / #481 class breaks) into an isolated
// HOME, then drives the OpenAI-compatible request the plugin targets against
// the no-token data-plane gateway (:9479).
func openCodeLeg() Leg {
	return Leg{
		Name:       "opencode",
		ExpectKind: "openai",
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
		Drive: func(ctx context.Context, e Env) (int, []byte, error) {
			return driveOpenAI(ctx, e.DataPlaneURL, e.TinyAlias)
		},
	}
}

// openClawLeg writes the real OpenClaw provider plugin + openclaw.json (proving
// that config surface) into an isolated HOME, then drives the OpenAI-compatible
// request the plugin targets against the same no-token data-plane gateway.
// (OpenClaw is not bundled; the real openclaw binary end-to-end is #518.)
func openClawLeg() Leg {
	return Leg{
		Name:       "openclaw",
		ExpectKind: "openai",
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
		Drive: func(ctx context.Context, e Env) (int, []byte, error) {
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
	// (the plugins target the no-token :9479 data plane, so the token value is
	// irrelevant to routing).
	stateDir := filepath.Join(home, ".config", "waired")
	if err := a.Apply(ctx, integration.ApplyOptions{
		HomeDir:        home,
		StateDir:       stateDir,
		GatewayBaseURL: "http://127.0.0.1:9473",
		// A non-empty token is required by Apply; the data-plane (:9479) the
		// plugins target is no-token, so the value is irrelevant to routing.
		GatewayToken:   "waired-integration-dummy-token",
		Force:          true,
		NonInteractive: true,
	}); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s Apply: %w", a.ID(), err)
	}
	return home, cleanup, nil
}
