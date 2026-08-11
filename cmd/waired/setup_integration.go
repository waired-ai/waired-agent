package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/proto/signer"
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
func runSetupIntegrations(s *executorSession, out, errOut io.Writer, o setupIntegrationOpts) error {
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

	if err := applySetupIntegrations(ctx, targets, o, out, errOut); err != nil {
		writePromptf(out, "%s Coding-tool setup failed: %v\n", emo("⚠️", "!"), err)
		s.FailedStep(management.SetupStepIntegration, err.Error())
		return err
	}
	s.DoneStep(management.SetupStepIntegration)
	return nil
}

// runWizardIntegrations applies the browser's coding-tool instruction and
// reports whether there WAS one to apply.
//
// It exists because waired-agent#311 gave the apply two call sites. The
// ordinary one is between the engine install and the model download, which
// is the whole point of the reorder: the last step needing a human stops
// sitting behind the longest unattended wait of the flow. But two things
// can put the instruction out of reach at that moment — a browser setup
// that only commits during the download (#308), and a wizard that writes
// its engine and model a beat before its coding-tool answer — so the old
// site, after the wait, stays as the catch-up.
//
// The return value is what makes the pair safe: `false` means nothing was
// asked for, so the second call is still allowed to try; `true` means this
// run has applied the instruction and the second call must not repeat it.
// A failed apply still returns true — it happened, it reported itself to
// the wizard, and doing it twice would not make it work.
//
// Warn-only, like every other integration path: sign-in already succeeded,
// and the step reports its own failure to the wizard (waired#935).
func runWizardIntegrations(s *executorSession, setupActive bool, o setupIntegrationOpts) bool {
	if !setupActive || s.State().Integrations == nil {
		return false
	}
	if err := runSetupIntegrations(s, os.Stdout, os.Stderr, o); err != nil {
		fmt.Fprintf(os.Stderr,
			"warn: coding-tool setup had problems (%v); re-run later: waired link --force all\n", err)
	}
	return true
}

// reportTerminalIntegrations reports the coding-tool §7 row for the
// TERMINAL's own integration step (waired-agent#646).
//
// The wizard's apply has reported this row since waired#935; the terminal's
// never has, so a `waired init` that configured the coding tools left the
// row with no author at all. On a device carrying a leftover instruction
// the daemon then read the executor's exit as "it left before it got to
// this row" and reported a failure for work that had just succeeded
// (waired-agent#645).
//
// Silent unless the operator consented AND the apply came back clean:
// a declined question wrote nothing, and a half-configured machine is what
// applySetupIntegrations already refuses to report as done.
func reportTerminalIntegrations(s *executorSession, consented bool, err error) {
	if !s.Supported() || !consented || err != nil {
		return
	}
	s.IntegrationDone(terminalIntegrationTargets())
}

// terminalIntegrationTargets names what the terminal's coding-agent step
// configures on a clean run: every adapter the applier covers.
//
// Both terminal journeys go through integration.Manager.ApplyAll over this
// same adapter set — in process, or in a `waired link all` child under sudo
// — and both stop at the first failure. "No error" therefore means every
// one of these was written, which is what makes one list honest for both.
// Derived from the adapters rather than from the wire constants so it
// tracks what actually gets written; the filter keeps a retired adapter,
// should one linger, out of a claim the control plane would reject.
func terminalIntegrationTargets() []string {
	var out []string
	for _, a := range []integration.Adapter{claudecode.New(), openclaw.New()} {
		if id := string(a.ID()); signer.IsValidIntegrationTarget(id) {
			out = append(out, id)
		}
	}
	return out
}

// setupIntegrationOpts is what the wizard's apply needs beyond the target
// list itself.
type setupIntegrationOpts struct {
	GatewayBaseURL string
	// StateDir is read for the Claude gateway port and the
	// managed-settings write options.
	StateDir string
	// SkipClaudeRoute is --skip-claude-route. The wizard's claude-code
	// toggle and the CLI opt-out can disagree — an operator who passed
	// --skip-claude-proxy to the installer and then ticked Claude Code in
	// the browser. The command-line opt-out wins: it is the more explicit
	// instruction, and the conservative one.
	SkipClaudeRoute bool
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
func applySetupIntegrations(ctx context.Context, targets []string, o setupIntegrationOpts, out, errOut io.Writer) error {
	sudoUser, isSudo := invokingSudoUser()
	homeDir, _ := os.UserHomeDir()

	for _, target := range targets {
		id := integration.AgentID(target)
		// A withdrawn integration is skipped, never failed. The daemon
		// already filters retired targets out of the instruction, so this
		// only fires on version skew — a CLI newer than the daemon it is
		// driving, which is the ordinary state for the seconds around an
		// upgrade. Failing here would turn that into a red coding-tools
		// row for a target the operator cannot act on, which is precisely
		// the wedge waired-agent#333 retires the value to avoid.
		if signer.IsRetiredIntegrationTarget(target) {
			writePromptf(out, "%s Skipping %s — that integration was removed.\n", emo("ⓘ", "-"), target)
			continue
		}
		// The claude-code toggle is the only one that changes the whole
		// machine, and the wizard says so in as many words ("Changes where
		// Claude Code sends its requests for everyone on this computer —
		// an administrator-owned setting, not just yours"). Until #294 the
		// applier delivered only the per-user skills, so that sentence was
		// a promise nothing kept. The managed-settings write happens HERE,
		// in the elevated parent: the per-user hop below drops root by
		// design and could not write a root-owned machine-wide file.
		if target == signer.IntegrationClaudeCode {
			if err := applySetupClaudeRoute(o, out); err != nil {
				return fmt.Errorf("%s: %w", target, err)
			}
		}
		if isSudo {
			if err := runLinkAllAsUser(ctx, sudoUser, linkOneChildArgs(o.GatewayBaseURL, target), out, errOut); err != nil {
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
			GatewayBaseURL: o.GatewayBaseURL,
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

// applySetupClaudeRoute performs the wizard's claude-code routing flip.
//
// planClaudeRoute owns the decision, with integConsent already settled
// (this runs only because the toggle was on) and wizardDriving set, so it
// never asks — §4.2: while the browser drives, this terminal has no
// question to put on screen and nobody watching it.
//
// A run that cannot write the file is NOT an error: --skip-claude-route
// means the operator declined, and a non-elevated executor means this
// host has a step it can finish later with `waired claude enable`.
// Failing the wizard's whole integration row for either would turn a
// deliberate choice into a red step.
func applySetupClaudeRoute(o setupIntegrationOpts, out io.Writer) error {
	switch planClaudeRoute(claudeRouteFacts{
		integConsent:    true,
		elevated:        isElevatedFn(),
		managedPath:     claudemanaged.Path(),
		skipClaudeRoute: o.SkipClaudeRoute,
		wizardDriving:   true,
	}) {
	case claudeRouteApply:
		// nil reader: AllowPrompt is false, so nothing below reads it.
		_, err := applyClaudeRouteFn(claudeRouteApplyOpts{
			StateDir: o.StateDir, In: nil, AllowPrompt: false,
		})
		return err
	case claudeRouteNeedsElevation:
		printClaudeRouteElevationHint(out)
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
