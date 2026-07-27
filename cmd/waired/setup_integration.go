package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// The browser wizard's coding-agent toggles are applied HERE, by the
// elevated CLI holding the executor lease (waired#935), and deliberately
// not by the daemon. Two of the three targets write into the invoking
// user's home, and the daemon runs as a service account that has no
// business there; the third writes root-owned managed settings. Making
// the daemon do either would turn it into a privilege bridge for any
// local process that can reach its unauthenticated management API.
//
// This process already holds both halves: root (it was started with
// sudo / from an elevated prompt) and the identity of the human who
// started it (SUDO_USER), which is the same pair `waired init`'s own
// integration phase uses.

// setupIntegrationBudget bounds the whole apply. Each target writes a
// handful of small files; a minute per target is generous, and the cap
// exists so a wedged child cannot hold the setup window open.
const setupIntegrationBudget = 3 * time.Minute

// runSetupIntegrations applies the integrations the wizard asked for and
// reports the outcome on the `integration` step.
//
// Returns nil when there is nothing to do: no instruction (an older
// control plane, or a device nobody ran the wizard on) and "asked, all
// toggles off" both leave the machine untouched. The daemon distinguishes
// those two on the wire — it reports `skipped` for the second — which is
// what stops "never asked" from reading as "asked and satisfied".
func runSetupIntegrations(s *executorSession, out, errOut io.Writer, gatewayBaseURL string) error {
	if !s.Supported() {
		return nil
	}
	st := s.State()
	if st.Integrations == nil {
		return nil
	}
	targets := *st.Integrations
	if len(targets) == 0 {
		// Every toggle was off. Nothing to write, and nothing to report:
		// the daemon already serves this row as `skipped` from the
		// instruction itself, without needing the executor to confirm it.
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupIntegrationBudget)
	defer cancel()

	s.Progress(management.SetupStepIntegration, "", 0, 0, 0)
	writePromptf(out, "%s %s\n", emo("🔌", "*"),
		bold(fmt.Sprintf("Setting up your coding tools (%s)…", strings.Join(targets, ", "))))

	if err := applySetupIntegrations(ctx, targets, gatewayBaseURL, out, errOut); err != nil {
		writePromptf(out, "%s Coding-tool setup failed: %v\n", emo("⚠️", "!"), err)
		s.FailedStep(management.SetupStepIntegration, err.Error())
		return err
	}
	s.DoneStep(management.SetupStepIntegration)
	return nil
}

// applySetupIntegrations writes the named integrations for the invoking
// user, one target at a time.
//
// Under sudo each target goes through the existing per-user hop rather
// than being applied in-process: an in-process apply would land in
// /root, which is the defect the hop exists for. Without sudo the
// current user IS the target and the in-process path is correct.
//
// One target at a time, and the FIRST failure stops the run: the wizard
// has one row for this step and one error to show, and continuing past a
// failure would leave a half-configured machine reported as either
// wholly done or wholly failed. Neither would be true.
func applySetupIntegrations(ctx context.Context, targets []string, gatewayBaseURL string, out, errOut io.Writer) error {
	sudoUser, isSudo := invokingSudoUser()
	homeDir, _ := os.UserHomeDir()

	for _, target := range targets {
		id := integration.AgentID(target)
		if isSudo {
			if err := runLinkAllAsUser(ctx, sudoUser, linkOneChildArgs(gatewayBaseURL, target), out, errOut); err != nil {
				return fmt.Errorf("%s: %w", target, err)
			}
			continue
		}
		if homeDir == "" {
			return fmt.Errorf("%s: cannot resolve the home directory to configure", target)
		}
		res, err := setup.IntegrationOne(ctx, id, setup.IntegrationOptions{
			HomeDir:        homeDir,
			StateDir:       defaultStateDir(),
			GatewayBaseURL: gatewayBaseURL,
			NonInteractive: true,
			WiredBinary:    wairedBinaryPath(),
		})
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		for _, ar := range res.Agents {
			if ar.Err != nil {
				return fmt.Errorf("%s: %w", ar.Agent, ar.Err)
			}
		}
	}
	return nil
}

// linkOneChildArgs builds the argv for a per-user `waired link <target>`
// child. Same shape and the same flags-before-target rule as
// linkAllChildArgs (stdlib flag parsing stops at the first non-flag
// argument), for one named agent rather than all of them.
func linkOneChildArgs(gatewayBaseURL, target string) []string {
	return []string{"link", "--force", "--no-prompt", "--gateway-base-url", gatewayBaseURL, target}
}
