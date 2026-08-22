package setup

import (
	"context"
	"errors"
	"fmt"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
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
	Agents []integration.ApplyResult // per-adapter outcomes
}

// Integration runs phase 3 (step 10 of spec §5.1) and is also the
// single entry point used by `waired link`. The orchestration steps:
//
//  1. Resolve <state> paths.
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

	if _, err := integration.PathsFor(opts.StateDir); err != nil {
		return nil, err
	}

	adapters := opts.Adapters
	if len(adapters) == 0 {
		adapters = []integration.Adapter{claudecode.New(), opencode.New(), openclaw.New()}
	}
	mgr := integration.NewManager(adapters...)

	apply := integration.ApplyOptions{
		HomeDir:        opts.HomeDir,
		StateDir:       opts.StateDir,
		GatewayBaseURL: opts.GatewayBaseURL,
		Force:          opts.Force,
		WiredBinary:    opts.WiredBinary,
		NonInteractive: opts.NonInteractive,
		Prompt:         opts.Prompt,
		Logger:         opts.Logger,
	}
	results := mgr.ApplyAll(ctx, apply)

	return &IntegrationResult{Agents: results}, nil
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

// UninstallAll runs phase-3 cleanup: every adapter's Uninstall.
//
// `waired unlink` — which the uninstall scripts call — walks the
// REGISTERED adapters, so an integration deleted from this list would
// leave its files in the user's home forever, including through a full
// Waired uninstall (the #333 removal had to keep a sweep for exactly
// that reason). Withdraw an adapter by keeping its Uninstall, not by
// deleting the package.
func UninstallAll(ctx context.Context, opts IntegrationOptions) error {
	mgr := integration.NewManager(claudecode.New(), opencode.New(), openclaw.New())
	for _, r := range mgr.UninstallAll(ctx, integration.ApplyOptions{
		HomeDir:        opts.HomeDir,
		StateDir:       opts.StateDir,
		GatewayBaseURL: opts.GatewayBaseURL,
	}) {
		if r.Err != nil {
			return fmt.Errorf("setup: uninstall %s: %w", r.Agent, r.Err)
		}
	}
	return nil
}

// UninstallOne removes one agent's per-adapter artefacts.
func UninstallOne(ctx context.Context, agentID integration.AgentID, opts IntegrationOptions) error {
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

// HasAdapter reports whether this build carries an adapter for id — the
// check the elevated setup executor makes BEFORE it hands a target to
// the per-user hop, whose exit status cannot say why it failed.
//
// The wire's valid set and this build's adapter set are meant to be the
// same list, but they live in different modules and move in separate
// PRs (proto first, by rule), so for the window between those merges a
// target can be valid on the wire and absent here. That window must
// read as "skipped", not as a red coding-tools row nobody can clear —
// the waired#983 wedge the retirement of opencode was designed around.
func HasAdapter(id integration.AgentID) bool {
	_, err := pickAdapter(id)
	return err == nil
}

func pickAdapter(id integration.AgentID) (integration.Adapter, error) {
	switch id {
	case integration.AgentClaudeCode:
		return claudecode.New(), nil
	case integration.AgentOpenCode:
		return opencode.New(), nil
	case integration.AgentOpenClaw:
		return openclaw.New(), nil
	default:
		return nil, fmt.Errorf("%w: %s", integration.ErrAgentNotFound, id)
	}
}
