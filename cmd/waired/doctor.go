package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// runDoctor implements `waired doctor [--fix] [--no-interactive]`.
//
// Output style mirrors Claude Code's /doctor: each finding is rendered
// with ✓ / ⚠ / ✗ icons plus a one-line subject and detail. On a TTY,
// after the diagnostic block we prompt "Press f to fix" — pressing f
// re-runs setup.Integration to repair anything fixable. On a non-TTY,
// we exit non-zero when any finding is StatusFail (CI-friendly).
//
// `--fix` skips the prompt and runs the repair unconditionally;
// `--no-interactive` suppresses the prompt even on a TTY.
func newDoctorCmd() *cobra.Command {
	var stateDir, gatewayBaseURL, mgmtURL string
	var fix, noInteractive bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose Waired setup; press 'f' to repair anything fixable.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctorBody(stateDir, gatewayBaseURL, mgmtURL, fix, noInteractive)
		},
	}
	addStateDirFlag(cmd, &stateDir, "directory holding identity / secrets / integrations ledger")
	cmd.Flags().StringVar(&gatewayBaseURL, "gateway-base-url", defaultGatewayURL,
		"Local Gateway base URL — the doctor probes /v1/models against this")
	cmd.Flags().StringVar(&mgmtURL, "mgmt", defaultMgmtURL,
		"Local Management API base URL — the doctor probes /waired/v1/status")
	cmd.Flags().BoolVar(&fix, "fix", false,
		"re-apply the integration to fix anything fixable; skips the interactive prompt")
	cmd.Flags().BoolVar(&noInteractive, "no-interactive", false,
		"suppress the 'Press f to fix' prompt even on a TTY")
	return cmd
}

func runDoctorBody(stateDirVal, gatewayBaseURLVal, mgmtURLVal string, fixVal, noInteractiveVal bool) error {
	stateDir := &stateDirVal
	gatewayBaseURL := &gatewayBaseURLVal
	mgmtURL := &mgmtURLVal
	fix := &fixVal
	noInteractive := &noInteractiveVal

	homeDir, _ := os.UserHomeDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tray := checkTray()
	findings := collectDoctorFindings(ctx, homeDir, *stateDir, *gatewayBaseURL, *mgmtURL, tray, checkService(ctx))
	hasFail := false
	for _, f := range findings {
		fmt.Println(formatFinding(f))
		if f.Status == integration.StatusFail {
			hasFail = true
		}
	}

	plan := planDoctorFix(hasFail, tray.Repair, *fix, *noInteractive, isTerminal(os.Stdin))

	if plan.Prompt {
		fmt.Println()
		if !pressedF(os.Stdin) {
			plan.Integration, plan.Tray = false, false
		}
	}
	if plan.Integration {
		fmt.Println("Running repair (waired link all)...")
		if err := repairWithUse(ctx, homeDir, *stateDir, *gatewayBaseURL); err != nil {
			return err
		}
	}
	if plan.Tray {
		if err := repairTrayHost(ctx, tray.Repair, os.Stdout); err != nil {
			// Warn-only: the tray is a convenience, and the finding it
			// repairs never contributed to the exit code. Print what went
			// wrong and the manual commands rather than failing the run.
			fmt.Fprintf(os.Stderr, "warn: tray host repair failed: %v\n", err)
		}
	}
	if plan.Integration || plan.Tray {
		fmt.Println("Done. Re-run `waired doctor` to verify.")
		return nil
	}

	if hasFail && !*fix {
		// CI-friendly: non-zero when there's nothing the operator can
		// claim is fine. Soft warnings (StatusWarn / StatusSkip) do
		// not contribute.
		return fmt.Errorf("waired doctor: %d findings need attention (see above)", countFails(findings))
	}
	return nil
}

// doctorFixPlan is what runDoctorBody does after printing the findings.
type doctorFixPlan struct {
	// Prompt asks "Press f to fix" before running anything. When the answer
	// is no, Integration and Tray are both dropped.
	Prompt bool
	// Integration re-applies the coding-agent integration (waired link all).
	Integration bool
	// Tray installs / enables the SNI host extension.
	Tray bool
}

// planDoctorFix decides the fix flow. Pure, and split out of runDoctorBody so
// the matrix is table-tested without a TTY, a session bus, or a repair actually
// running (CLAUDE.md §Test discipline).
//
// Two things changed here for #295. The prompt used to be offered only when
// some finding had failed, and the repair was always "waired link all" — which
// cannot install a GNOME extension. Now a fixable tray warning also earns the
// prompt, and each half of the repair is selected independently, so `f` on a
// host whose only problem is the tray does not also re-link every coding agent.
//
// What did NOT change: a tray warning still never makes `waired doctor` exit
// non-zero (see trayFindingFromResult). Fixable is not the same as failing.
func planDoctorFix(hasFail bool, tray trayhost.RepairAction, forced, noInteractive, tty bool) doctorFixPlan {
	fixable := tray.Fixable()
	switch {
	case forced:
		// --fix skips the prompt and repairs whatever is repairable. It runs
		// the integration unconditionally (its historical behaviour: it is
		// idempotent, and --fix predates there being anything else to fix).
		return doctorFixPlan{Integration: true, Tray: fixable}
	case noInteractive, !tty:
		return doctorFixPlan{}
	case hasFail:
		return doctorFixPlan{Prompt: true, Integration: true, Tray: fixable}
	case fixable:
		// Nothing failed, but the tray can be repaired — offer just that.
		return doctorFixPlan{Prompt: true, Tray: true}
	default:
		return doctorFixPlan{}
	}
}

// repairTrayHost carries out a tray-host repair plan: install the AppIndicator
// extension when it is missing (privileged; PlanRepair guarantees this host
// already runs GNOME), then enable it for the desktop user (unprivileged).
func repairTrayHost(ctx context.Context, action trayhost.RepairAction, out *os.File) error {
	if !action.Fixable() {
		return nil
	}
	_, _ = fmt.Fprintln(out, "Repairing the system tray host...")
	if action.NeedsPrivilege() {
		if err := trayhost.Install(ctx, out); err != nil {
			return err
		}
	}
	if err := trayhost.Enable(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "  Enabled. Log out and back in (required on Wayland) to show the tray icon.")
	return nil
}

// collectDoctorFindings gathers every finding for one run. tray and svc are
// passed in rather than probed here so the session bus and the service manager
// are each queried exactly once per run, and so tests can pass the zero values
// and stay independent of whatever desktop or service state the runner happens
// to have.
func collectDoctorFindings(ctx context.Context, homeDir, stateDir, gatewayURL, mgmtURL string, tray trayDoctor, svc servicediag.Result) []integration.AuditFinding {
	var out []integration.AuditFinding

	// Token presence + permission check. PathsUnder computes the layout
	// without touching the filesystem, so a non-root read of a root-owned
	// state dir surfaces the real EACCES from os.Stat below rather than a
	// chmod EPERM from PathsFor's SecureDir (#633).
	paths, err := integration.PathsUnder(stateDir)
	if err != nil {
		out = append(out, integration.AuditFinding{
			Status: integration.StatusFail, Subject: "state directory",
			Detail: err.Error(),
		})
	} else {
		switch _, err := os.Stat(paths.GatewayToken); {
		case err == nil:
			out = append(out, integration.AuditFinding{
				Status: integration.StatusOK, Subject: "gateway token",
				Detail: paths.GatewayToken,
			})
		case os.IsPermission(err):
			// Distinguish EACCES from a genuinely absent token — a
			// root-owned state dir read non-root is a permission problem
			// (fix with elevation), not a "run `waired link`" situation.
			out = append(out, integration.AuditFinding{
				Status: integration.StatusFail, Subject: "gateway token",
				Detail: fmt.Sprintf("permission denied reading %s — %s", paths.GatewayToken, elevationHint("")),
			})
		default:
			out = append(out, integration.AuditFinding{
				Status: integration.StatusFail, Subject: "gateway token",
				Detail: fmt.Sprintf("missing: %s — run `waired link` to create", paths.GatewayToken),
			})
		}
	}

	// Sign-in health, straight from persisted state so it answers with
	// the daemon down (waired-agent#318).
	if f := signInFinding(stateDir); f.Subject != "" {
		out = append(out, f)
	}

	// Whether a signed-in device actually connected. Needs the daemon,
	// so it comes from the management API rather than disk.
	if f := connectionFinding(ctx, mgmtURL); f.Subject != "" {
		out = append(out, f)
	}

	// Pause/resume phase. Surfaces an explicit warn finding when the
	// agent is paused so the user sees `waired resume` in the doctor
	// output rather than just a vague "Local Gateway HTTP 503".
	if f := phaseFinding(stateDir); f.Subject != "" {
		out = append(out, f)
	}

	// Per-adapter audit.
	mgr := integration.NewManager(claudecode.New(), openclaw.New())
	apply := integration.ApplyOptions{HomeDir: homeDir, StateDir: stateDir, GatewayBaseURL: gatewayURL}
	if all, err := mgr.AuditAll(ctx, apply); err == nil {
		out = append(out, all...)
	}

	// Why the agent is down, when it is (#315). Ordered before the live probes
	// so the explanation precedes the "unreachable" line it explains, rather
	// than trailing it. Emits nothing when the service is healthy or when the
	// platform produced no evidence.
	if f := serviceFindingFromResult(svc); f.Subject != "" {
		out = append(out, f)
	}

	// Live probes.
	out = append(out, probeHTTP(ctx, "Local Gateway", gatewayURL+"/v1/models"))
	out = append(out, probeHTTP(ctx, "waired-agent management", mgmtURL+"/waired/v1/status"))

	// Phase 9 observability findings — engine readiness, mesh peer
	// counts, and recent-fallback rate. Emits zero findings when the
	// management probe above already reported the daemon unreachable
	// (probeObservability swallows transport errors). Older daemons
	// surface a single StatusSkip explaining the upgrade path.
	out = append(out, probeObservability(ctx, mgmtURL)...)

	// Linux desktop tray host (waired#493): on GNOME the waired-tray SNI icon
	// does not render without an AppIndicator host extension. Surface a warn
	// finding (with the install/enable/re-login hint) when no SNI host is
	// present. Empty Subject — NotApplicable on servers / macOS / Windows, and
	// the zero value tests pass — is skipped.
	if tray.Finding.Subject != "" {
		out = append(out, tray.Finding)
	}

	return out
}

// phaseFinding inspects <state>/runtime/state and reports the agent's
// current pause/resume mode. Returns an empty finding (caller skips)
// when the state file is missing or stale — the live probe further
// down will report the underlying daemon-not-running condition with a
// more useful message.
func phaseFinding(stateDir string) integration.AuditFinding {
	s, err := state.Read(stateDir)
	if err != nil {
		return integration.AuditFinding{}
	}
	if s.Phase == state.PhasePaused {
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "waired phase",
			Detail:  "paused — new shells will use api.anthropic.com directly. Run `waired resume` to restore overlay routing.",
		}
	}
	if !s.Effective(time.Now(), state.DefaultStaleAfter) {
		return integration.AuditFinding{}
	}
	return integration.AuditFinding{
		Status:  integration.StatusOK,
		Subject: "waired phase",
		Detail:  "active — overlay routing in effect",
	}
}

// probeHTTP issues a GET, treats 200 / 401 / 403 (the latter two when
// the gateway token is enforced) as "alive", and anything else as fail.
// Network errors → StatusFail with the underlying error.
func probeHTTP(ctx context.Context, label, url string) integration.AuditFinding {
	cl := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := cl.Do(req)
	if err != nil {
		return integration.AuditFinding{
			Status: integration.StatusFail, Subject: label,
			Detail: fmt.Sprintf("unreachable: %v — start with `waired-agent`", err),
		}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden:
		return integration.AuditFinding{
			Status: integration.StatusOK, Subject: label,
			Detail: fmt.Sprintf("HTTP %d at %s", resp.StatusCode, url),
		}
	default:
		return integration.AuditFinding{
			Status: integration.StatusFail, Subject: label,
			Detail: fmt.Sprintf("HTTP %d at %s", resp.StatusCode, url),
		}
	}
}

// formatFinding renders one line in Claude Code /doctor style.
func formatFinding(f integration.AuditFinding) string {
	icon := "?"
	switch f.Status {
	case integration.StatusOK:
		icon = "✓"
	case integration.StatusWarn:
		icon = "⚠"
	case integration.StatusFail:
		icon = "✗"
	case integration.StatusSkip:
		icon = "·"
	}
	if f.Detail == "" {
		return fmt.Sprintf("%s %s", icon, f.Subject)
	}
	return fmt.Sprintf("%s %s — %s", icon, f.Subject, f.Detail)
}

func countFails(findings []integration.AuditFinding) int {
	n := 0
	for _, f := range findings {
		if f.Status == integration.StatusFail {
			n++
		}
	}
	return n
}

// pressedF is a minimal "Press f to fix" reader. It prints the prompt,
// reads one line, and returns true when the input starts with f or F.
// Other input (or EOF) returns false. We do NOT raw-read keys to keep
// the dependency surface zero — single-key UX is a nice-to-have.
func pressedF(in *os.File) bool {
	_, _ = fmt.Fprintf(os.Stdout, "Press f to fix [f/N]: ")
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "f" || s == "fix"
}

func repairWithUse(ctx context.Context, homeDir, stateDir, gatewayURL string) error {
	res, err := setup.Integration(ctx, setup.IntegrationOptions{
		HomeDir:        homeDir,
		StateDir:       stateDir,
		GatewayBaseURL: gatewayURL,
		NonInteractive: !isTerminal(os.Stdin),
	})
	if err != nil {
		return err
	}
	for _, ar := range res.Agents {
		if ar.Err != nil {
			return fmt.Errorf("repair: %s: %w", ar.Agent, ar.Err)
		}
	}
	return nil
}
