package setup

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/integration"
)

// InitOptions bundles the inputs `waired init` collects from flags +
// env. The orchestrator threads these into the per-phase calls.
type InitOptions struct {
	// Phase 1 (enroll) inputs.
	ControlURL      string
	DeviceName      string
	Endpoint        string
	StateDir        string
	HTTPClient      EnrollHTTPClient // a thin alias to allow fakes
	OnLoginURL      func(loginURL, userCode string)
	OnLoginComplete func(accountEmail, networkName string)
	ClientVersion   string

	// Phase 2 (deploy) inputs.
	Inference agentconfig.InferenceConfig

	// ConfigureInference, when non-nil, runs AFTER Enroll succeeds and
	// BEFORE Deploy; its return value replaces Inference for the deploy
	// phase. This lets cmd/waired run the stdin-reading local-inference
	// prompt only once sign-in + registration have completed (so the
	// browser/login step is what the operator sees first), while keeping
	// the setup/ package stdin-free. The enroll result is passed through
	// so the caller can print the "registered / persisted" status before
	// prompting. A returned error aborts Init fail-fast, before any Deploy
	// mutation. It is invoked regardless of SkipDeploy so the caller can
	// still print status / persist config when deploy is skipped. nil =>
	// Inference is used unchanged.
	ConfigureInference func(ctx context.Context, enroll *EnrollResult) (agentconfig.InferenceConfig, error)

	// Phase 3 (integration) inputs.
	HomeDir           string
	GatewayBaseURL    string
	NonInteractive    bool
	IntegrationPrompt integration.Prompter
	IntegrationLogger integration.Logger

	// ConfigureIntegration, when non-nil, runs AFTER Deploy and BEFORE
	// the integration phase; its decision controls whether and how the
	// phase runs. Like ConfigureInference, this keeps the setup/ package
	// stdin-free: cmd/waired wires the TTY consent prompt here. A
	// returned error aborts Init fail-fast, before any integration
	// mutation. It is NOT invoked when SkipIntegration is set (renew /
	// --skip-integration). nil => legacy behaviour: apply with Detect()
	// gating.
	ConfigureIntegration func(ctx context.Context) (IntegrationDecision, error)

	// WiredBinary is the absolute path of the running waired binary,
	// forwarded to IntegrationOptions.WiredBinary (adapters that
	// generate launchers — the VSCode shim — embed it).
	WiredBinary string

	// Skip toggles.
	SkipDeploy      bool
	SkipIntegration bool

	// AllowMutations gates the side-effecting parts of Deploy (today:
	// `ollama pull` of the bundled model). cmd/waired/runInit sets it
	// to true so a fresh install pre-pulls the bundled weights; library
	// callers default to false so importing setup.Init never mutates
	// the host.
	AllowMutations bool

	// DeployProgressSink, when non-nil, receives one PullEvent per pull
	// progress update (model name + embedded download.Progress). The CLI
	// passes a rate-limited stdout printer here that renders an aggregated
	// download bar.
	DeployProgressSink func(PullEvent)
}

// InitResult mirrors what `waired init` prints to stdout.
type InitResult struct {
	Enroll      *EnrollResult
	Deploy      *DeployResult
	Integration *IntegrationResult
}

// IntegrationDecision is what the ConfigureIntegration hook returns.
// The zero value preserves the legacy behaviour: run the phase with
// Detect() gating and no VSCode consent.
type IntegrationDecision struct {
	// Skip skips the integration phase entirely — either the operator
	// declined, or the caller applies it out-of-process (the sudo →
	// invoking-user hop in cmd/waired).
	Skip bool
	// Force applies every adapter even when Detect() is negative, so
	// the integration activates once the coding agent is installed.
	Force bool
}

// enrollFn is the enroll entry point Init calls. Production points it at
// Enroll; tests override it to exercise Init's orchestration (the
// ConfigureInference hook ordering / fail-fast) without standing up a
// fake Control Plane.
var enrollFn = Enroll

// integrationFn is the integration entry point Init calls. Production
// points it at Integration; tests override it to capture the
// IntegrationOptions Init builds without touching the host.
var integrationFn = Integration

// Init is the orchestrator: enroll → deploy → integration. Default
// policy is fail-fast: any non-skipped phase returning a non-nil error
// aborts the rest. The caller (cmd/waired/main.go) translates the
// error into a non-zero exit; callers that want to push through
// failures must do so via the explicit `--skip-*` flags.
func Init(ctx context.Context, opts InitOptions) (*InitResult, error) {
	if err := validateInitOptions(&opts); err != nil {
		return nil, err
	}

	res := &InitResult{}

	enroll, err := enrollFn(ctx, EnrollOptions{
		ControlURL:      opts.ControlURL,
		DeviceName:      opts.DeviceName,
		Endpoint:        opts.Endpoint,
		StateDir:        opts.StateDir,
		HTTPClient:      opts.HTTPClient.toStdlib(),
		OnLoginURL:      opts.OnLoginURL,
		OnLoginComplete: opts.OnLoginComplete,
		ClientVersion:   opts.ClientVersion,
	})
	if err != nil {
		return res, fmt.Errorf("enroll: %w", err)
	}
	res.Enroll = enroll

	// Resolve the deploy-time inference config. The hook (when supplied)
	// runs after enroll and before deploy — see ConfigureInference. It runs
	// even when SkipDeploy is set so the caller can still print post-enroll
	// status and persist the operator's choice.
	inference := opts.Inference
	if opts.ConfigureInference != nil {
		cfg, err := opts.ConfigureInference(ctx, enroll)
		if err != nil {
			return res, fmt.Errorf("configure inference: %w", err)
		}
		inference = cfg
	}

	if !opts.SkipDeploy {
		dep, err := Deploy(ctx, DeployOptions{
			StateDir:       opts.StateDir,
			Inference:      inference,
			AllowMutations: opts.AllowMutations,
			ProgressSink:   opts.DeployProgressSink,
		})
		if err != nil {
			return res, fmt.Errorf("deploy: %w", err)
		}
		res.Deploy = dep
	}

	if !opts.SkipIntegration {
		var dec IntegrationDecision
		if opts.ConfigureIntegration != nil {
			d, err := opts.ConfigureIntegration(ctx)
			if err != nil {
				return res, fmt.Errorf("configure integration: %w", err)
			}
			dec = d
		}
		if !dec.Skip {
			integ, err := integrationFn(ctx, IntegrationOptions{
				HomeDir:        opts.HomeDir,
				StateDir:       opts.StateDir,
				GatewayBaseURL: opts.GatewayBaseURL,
				NonInteractive: opts.NonInteractive,
				Force:          dec.Force,
				WiredBinary:    opts.WiredBinary,
				Prompt:         opts.IntegrationPrompt,
				Logger:         opts.IntegrationLogger,
			})
			if err != nil {
				return res, fmt.Errorf("integration: %w", err)
			}
			res.Integration = integ
			// Fail-fast on any per-agent error too — Init's contract is
			// "everything's set up, or the operator gets a clean error
			// to act on". Skipped adapters (Detect=false) are not errors.
			for _, ar := range integ.Agents {
				if ar.Err != nil {
					return res, fmt.Errorf("integration: %s: %w", ar.Agent, ar.Err)
				}
			}
		}
	}

	return res, nil
}

// validateInitOptions enforces required-field invariants before Init
// touches anything stateful. Empty HomeDir / GatewayBaseURL fall back
// to discovered defaults so callers in cmd/waired don't have to
// duplicate the resolution.
func validateInitOptions(opts *InitOptions) error {
	if opts.ControlURL == "" {
		return errors.New("setup: control URL is required")
	}
	if opts.StateDir == "" {
		return errors.New("setup: state dir is required")
	}
	if opts.Endpoint == "" {
		return errors.New("setup: endpoint is required")
	}
	if opts.HomeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			opts.HomeDir = h
		}
	}
	if opts.GatewayBaseURL == "" {
		opts.GatewayBaseURL = "http://127.0.0.1:9473"
	}
	return nil
}
