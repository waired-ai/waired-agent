package setup

import (
	"context"
	"errors"
	"fmt"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/retired"
)

// IntegrationOptions are the inputs phase 3 needs.
//
// The integration writes only per-user config: the Claude Code skills and
// the OpenClaw plugin (which carries the gateway URL + token). Claude
// request routing on Linux is handled by the transparent proxy, set up
// separately as root; there is no shell alias or env file.
type IntegrationOptions struct {
	HomeDir        string
	StateDir       string
	GatewayBaseURL string // e.g. "http://127.0.0.1:9473"
	NonInteractive bool
	// Force makes per-adapter Apply skip Detect() (used by
	// `waired link claude-code` when the user is explicit).
	Force bool
	// WiredBinary is the absolute path of the running waired binary.
	WiredBinary string
	// Adapters lets callers (tests, dry-run) inject a custom
	// adapter set. Empty = the production default
	// (claudecode + openclaw).
	Adapters []integration.Adapter
	// Prompt + Logger are forwarded into ApplyOptions.
	Prompt integration.Prompter
	Logger integration.Logger
}

// IntegrationResult mirrors the per-agent ApplyResult.
type IntegrationResult struct {
	GatewayToken string                    // not echoed by callers; just for tests / debug
	Agents       []integration.ApplyResult // per-adapter outcomes
}

// Integration runs phase 3 (step 10 of spec §5.1) and is also the
// single entry point used by `waired link`. The orchestration steps:
//
//  1. Resolve <state> paths, load/create the gateway token.
//  2. Run every registered Adapter.Apply, collecting results. Errors
//     are surfaced per-agent — fail-fast policy lives at the
//     orchestrator level (Init), so this function returns nil on
//     "some adapters failed" and the caller decides whether to error.
func Integration(ctx context.Context, opts IntegrationOptions) (*IntegrationResult, error) {
	if opts.HomeDir == "" {
		return nil, errors.New("setup: integration: empty HomeDir")
	}
	if opts.StateDir == "" {
		return nil, errors.New("setup: integration: empty StateDir")
	}
	if opts.GatewayBaseURL == "" {
		return nil, errors.New("setup: integration: empty GatewayBaseURL")
	}

	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		return nil, err
	}
	tok, err := integration.LoadOrCreateGatewayToken(paths.GatewayToken)
	if err != nil {
		return nil, fmt.Errorf("setup: gateway token: %w", err)
	}

	adapters := opts.Adapters
	if len(adapters) == 0 {
		adapters = []integration.Adapter{claudecode.New(), openclaw.New()}
	}
	mgr := integration.NewManager(adapters...)

	apply := integration.ApplyOptions{
		HomeDir:        opts.HomeDir,
		StateDir:       opts.StateDir,
		GatewayBaseURL: opts.GatewayBaseURL,
		GatewayToken:   tok,
		Force:          opts.Force,
		WiredBinary:    opts.WiredBinary,
		NonInteractive: opts.NonInteractive,
		Prompt:         opts.Prompt,
		Logger:         opts.Logger,
	}
	results := mgr.ApplyAll(ctx, apply)

	return &IntegrationResult{
		GatewayToken: tok,
		Agents:       results,
	}, nil
}

// IntegrationOne is the `waired link <agent>` entry point: same
// orchestration but only one adapter.
func IntegrationOne(ctx context.Context, agentID integration.AgentID, opts IntegrationOptions) (*IntegrationResult, error) {
	one, err := pickAdapter(agentID)
	if err != nil {
		return nil, err
	}
	opts.Adapters = []integration.Adapter{one}
	opts.Force = true // explicit `waired link` skips Detect gating.
	return Integration(ctx, opts)
}

// UninstallAll runs phase-3 cleanup: every adapter's Uninstall, plus the
// sweeps for integrations Waired has withdrawn.
//
// The retired sweeps are what stops a removal from stranding files.
// `waired unlink` — which the uninstall scripts call — walks the
// REGISTERED adapters, so an integration deleted from that list would
// leave its plugin in the user's home forever, including through a full
// Waired uninstall. See internal/integration/retired.
func UninstallAll(ctx context.Context, opts IntegrationOptions) error {
	mgr := integration.NewManager(claudecode.New(), openclaw.New())
	for _, r := range mgr.UninstallAll(ctx, integration.ApplyOptions{
		HomeDir:        opts.HomeDir,
		StateDir:       opts.StateDir,
		GatewayBaseURL: opts.GatewayBaseURL,
	}) {
		if r.Err != nil {
			return fmt.Errorf("setup: uninstall %s: %w", r.Agent, r.Err)
		}
	}
	if err := retired.SweepOpenCode(opts.HomeDir, opts.StateDir); err != nil {
		return fmt.Errorf("setup: uninstall opencode (retired): %w", err)
	}
	return nil
}

// UninstallOne removes one agent's per-adapter artefacts.
//
// A withdrawn integration is still removable by name: `waired unlink
// opencode` is the instruction of somebody who has the files and wants
// them gone, and answering "no such agent" would be both unhelpful and
// untrue of their disk.
func UninstallOne(ctx context.Context, agentID integration.AgentID, opts IntegrationOptions) error {
	if agentID == retired.OpenCodeAgentID {
		return retired.SweepOpenCode(opts.HomeDir, opts.StateDir)
	}
	one, err := pickAdapter(agentID)
	if err != nil {
		return err
	}
	mgr := integration.NewManager(one)
	res := mgr.UninstallOne(ctx, agentID, integration.ApplyOptions{
		HomeDir:        opts.HomeDir,
		StateDir:       opts.StateDir,
		GatewayBaseURL: opts.GatewayBaseURL,
	})
	return res.Err
}

// ErrAgentRetired is returned when the named coding agent is one Waired
// used to integrate with and no longer does. Distinct from
// ErrAgentNotFound so `waired link` can say which it is: "we removed
// this" is a different answer from "no such thing", and a user who typed
// a name that used to work deserves the first one.
var ErrAgentRetired = errors.New("setup: integration with this coding agent was removed")

func pickAdapter(id integration.AgentID) (integration.Adapter, error) {
	switch id {
	case integration.AgentClaudeCode:
		return claudecode.New(), nil
	case integration.AgentOpenClaw:
		return openclaw.New(), nil
	case retired.OpenCodeAgentID:
		return nil, fmt.Errorf("%w: %s (waired-agent#333; `waired unlink %s` still removes an existing one)",
			ErrAgentRetired, id, id)
	default:
		return nil, fmt.Errorf("%w: %s", integration.ErrAgentNotFound, id)
	}
}
