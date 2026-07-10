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
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
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

	findings := collectDoctorFindings(ctx, homeDir, *stateDir, *gatewayBaseURL, *mgmtURL)
	hasFail := false
	for _, f := range findings {
		fmt.Println(formatFinding(f))
		if f.Status == integration.StatusFail {
			hasFail = true
		}
	}

	switch {
	case *fix:
		fmt.Println("\nRunning repair (waired link all)...")
		return repairWithUse(ctx, homeDir, *stateDir, *gatewayBaseURL)
	case *noInteractive:
	case isTerminal(os.Stdin):
		if hasFail {
			fmt.Println()
			if pressedF(os.Stdin) {
				fmt.Println("Running repair (waired link all)...")
				if err := repairWithUse(ctx, homeDir, *stateDir, *gatewayBaseURL); err != nil {
					return err
				}
				fmt.Println("Done. Re-run `waired doctor` to verify.")
				return nil
			}
		}
	}

	if hasFail && !*fix {
		// CI-friendly: non-zero when there's nothing the operator can
		// claim is fine. Soft warnings (StatusWarn / StatusSkip) do
		// not contribute.
		return fmt.Errorf("waired doctor: %d findings need attention (see above)", countFails(findings))
	}
	return nil
}

func collectDoctorFindings(ctx context.Context, homeDir, stateDir, gatewayURL, mgmtURL string) []integration.AuditFinding {
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

	// Pause/resume phase. Surfaces an explicit warn finding when the
	// agent is paused so the user sees `waired resume` in the doctor
	// output rather than just a vague "Local Gateway HTTP 503".
	if f := phaseFinding(stateDir); f.Subject != "" {
		out = append(out, f)
	}

	// Per-adapter audit.
	mgr := integration.NewManager(claudecode.New(), opencode.New(), openclaw.New())
	apply := integration.ApplyOptions{HomeDir: homeDir, StateDir: stateDir, GatewayBaseURL: gatewayURL}
	if all, err := mgr.AuditAll(ctx, apply); err == nil {
		out = append(out, all...)
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

	// Linux desktop tray host (#493): on GNOME the waired-tray SNI icon does
	// not render without an AppIndicator host extension. Surface a warn finding
	// (with the install/enable/re-login hint) when no SNI host is present.
	// Empty Subject — NotApplicable on servers / macOS / Windows — is skipped.
	if f := trayFindingFromResult(trayhost.Check()); f.Subject != "" {
		out = append(out, f)
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
