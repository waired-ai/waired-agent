package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
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
			gatewayBaseURL = resolveGatewayBaseURL(cmd, mgmtURL, stateDir, gatewayBaseURL)
			return runDoctorBody(stateDir, gatewayBaseURL, mgmtURL, fix, noInteractive)
		},
	}
	addStateDirFlag(cmd, &stateDir, "directory holding identity / secrets / integrations ledger")
	cmd.Flags().StringVar(&gatewayBaseURL, "gateway-base-url", defaultGatewayURL,
		"local gateway base URL — the doctor probes /v1/models against this; defaults to this host's configured port")
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

	procHome, _ := os.UserHomeDir()
	home := doctorHomeFor(runtime.GOOS, os.Geteuid(), os.Getenv("SUDO_USER"), procHome, sudoUserHome)
	if notice := home.notice(); notice != "" {
		fmt.Fprintln(stdout, notice)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tray := checkTray()
	findings, engine := collectDoctorFindings(ctx, home.Dir, *stateDir, *gatewayBaseURL, *mgmtURL, tray,
		checkService(ctx, *stateDir), checkClaude(home.Dir))
	hasFail := false
	for _, f := range findings {
		fmt.Fprintln(stdout, formatFinding(f))
		if f.Status == integration.StatusFail {
			hasFail = true
		}
	}

	plan := planDoctorFix(hasFail, tray.Repair, engine.Repair, *fix, *noInteractive, isTerminal(os.Stdin))

	if plan.Prompt {
		fmt.Fprintln(stdout)
		if !pressedF(os.Stdin) {
			plan.Integration, plan.Tray, plan.Engine = false, false, false
		}
	}
	if plan.Integration {
		fmt.Fprintln(stdout, "Running repair (waired link all)...")
		if err := repairWithUse(ctx, home, *stateDir, *gatewayBaseURL); err != nil {
			return err
		}
	}
	if plan.Tray {
		if err := repairTrayHost(ctx, tray.Repair, stdout); err != nil {
			// Warn-only: the tray is a convenience, and the finding it
			// repairs never contributed to the exit code. Print what went
			// wrong and the manual commands rather than failing the run.
			fmt.Fprintf(stderr, "warn: tray host repair failed: %v\n", err)
		}
	}
	if plan.Engine {
		if err := repairEngine(*mgmtURL, engine.Reason, stdout); err != nil {
			// Warn-only, for the tray's reason: the engine finding is a
			// StatusWarn that never contributed to the exit code, and a
			// repair that could not run must not turn a warning into a
			// failure. The daemon's own sentence is in the error.
			fmt.Fprintf(stderr, "warn: %v\n", err)
		}
	}
	if plan.Integration || plan.Tray || plan.Engine {
		fmt.Fprintln(stdout, "Done. Re-run `waired doctor` to verify.")
		return nil
	}

	if hasFail && !*fix {
		// CI-friendly: non-zero when there's nothing the operator can
		// claim is fine. Soft warnings (StatusWarn / StatusSkip) do
		// not contribute.
		return fmt.Errorf("waired doctor: %s (see above)", findingsSummary(countFails(findings)))
	}
	return nil
}

// findingsSummary phrases the closing count. Split out and pluralised
// because "1 findings need attention" was the literal output on a host
// with a single failure (#652); "4 findings" was already correct, so the
// bug only ever showed on the case an operator is most likely to hit.
func findingsSummary(n int) string {
	if n == 1 {
		return "1 finding needs attention"
	}
	return fmt.Sprintf("%d findings need attention", n)
}

// doctorHome is whose home directory this doctor run inspects.
//
// The doctor's checks split across two different notions of "which user"
// and they used to disagree. The token / sign-in / phase checks follow
// --state-dir, whose default is euid-dependent (initStateDirMode); the
// skill / plugin / config checks follow a home directory, which used to be
// the *process* home. Under `sudo waired doctor` that is /root, so the
// second half reported four missing integrations and a failing exit code
// for a host whose integrations were all fine (#650).
//
// It only showed up on Linux. macOS sudo keeps HOME by default, so the
// process home there already WAS the invoking user's — the darwin doctor
// looked correct by accident, not by design.
type doctorHome struct {
	// Dir is the home directory to inspect.
	Dir string
	// SudoUser is the invoking human when this run is elevated via sudo,
	// otherwise "".
	SudoUser string
	// Fellback records that SudoUser's home could not be looked up and Dir
	// is the process home instead. The doctor says so rather than quietly
	// reporting on root's files as if they were the user's.
	Fellback bool
}

// notice is the line printed above the findings when the run is elevated.
// Empty for an ordinary unelevated run, which needs no explanation.
func (h doctorHome) notice() string {
	switch {
	case h.SudoUser == "":
		return ""
	case h.Fellback:
		return fmt.Sprintf("Running under sudo — could not resolve the home directory of user %q, "+
			"so the checks below cover %s. Run `waired doctor` without sudo to check your own setup.",
			h.SudoUser, h.Dir)
	default:
		return fmt.Sprintf("Running under sudo — checking the setup for user %q, not root.", h.SudoUser)
	}
}

// doctorHomeFor is the pure decision behind the home directory the doctor
// inspects. It takes the facts rather than reading them so all three
// platforms are table-tested on every CI leg (CLAUDE.md §Test discipline);
// the sudo half is delegated to invokingSudoUserAt, which `waired init`
// already uses for the same question.
//
// lookup resolves a username to a home directory (sudoUserHome in
// production). It can fail for an NSS/LDAP user under a CGO-less build —
// see sudoUserHome's comment — so a failure falls back to the process home
// and is recorded, not swallowed.
func doctorHomeFor(goos string, euid int, sudoUser, procHome string, lookup func(string) (string, error)) doctorHome {
	u, isSudo := invokingSudoUserAt(goos, euid, sudoUser)
	if !isSudo {
		return doctorHome{Dir: procHome}
	}
	h, err := lookup(u)
	if err != nil || h == "" {
		return doctorHome{Dir: procHome, SudoUser: u, Fellback: true}
	}
	return doctorHome{Dir: h, SudoUser: u}
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
	// Engine asks the daemon to start the serving engine — the repair the
	// browser wizard has been sending people here for (waired-agent#1170).
	Engine bool
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
// The engine arm (waired-agent#1170) is the same shape and relies on the same
// rule — engineFinding is a StatusWarn, and it stays one.
func planDoctorFix(hasFail bool, tray trayhost.RepairAction, engine, forced, noInteractive, tty bool) doctorFixPlan {
	fixable := tray.Fixable()
	switch {
	case forced:
		// --fix skips the prompt and repairs whatever is repairable. It runs
		// the integration unconditionally (its historical behaviour: it is
		// idempotent, and --fix predates there being anything else to fix).
		return doctorFixPlan{Integration: true, Tray: fixable, Engine: engine}
	case noInteractive, !tty:
		return doctorFixPlan{}
	case hasFail:
		return doctorFixPlan{Prompt: true, Integration: true, Tray: fixable, Engine: engine}
	case fixable, engine:
		// Nothing failed, but something warn-level can be repaired — offer
		// just that, without re-linking every coding agent.
		return doctorFixPlan{Prompt: true, Tray: fixable, Engine: engine}
	default:
		return doctorFixPlan{}
	}
}

// repairTrayHost carries out a tray-host repair plan: install the AppIndicator
// extension when it is missing (privileged; PlanRepair guarantees this host
// already runs GNOME), then enable it for the desktop user (unprivileged).
func repairTrayHost(ctx context.Context, action trayhost.RepairAction, out io.Writer) error {
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

// collectDoctorFindings gathers every finding for one run. tray, svc and claude
// are passed in rather than probed here so the session bus, the service manager
// and the Claude Code settings files are each queried exactly once per run, and
// so tests can pass the zero values and stay independent of whatever desktop,
// service or Claude Code state the runner happens to have.
func collectDoctorFindings(ctx context.Context, homeDir, stateDir, gatewayURL, mgmtURL string, tray trayDoctor, svc servicediag.Result, claude claudeDoctor) ([]integration.AuditFinding, engineDoctor) {
	var out []integration.AuditFinding

	// No gateway-token check: the gateway carries no credential
	// (waired-ai/waired#1277), so there is no file whose absence would
	// mean anything. The Local Gateway probe further down answers the
	// question this row used to stand in for.
	if _, err := integration.PathsUnder(stateDir); err != nil {
		out = append(out, integration.AuditFinding{
			Status: integration.StatusFail, Subject: "state directory",
			Detail: err.Error(),
		})
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

	// The disagreement between the two checks above (#800). Each is
	// answering honestly about the source it reads, and the fault lives
	// only in the gap: the daemon is signed in, the disk is not.
	//
	// "the disk is not" has more than one shape, and only one of them is
	// the fault (#1005) — see stateDiskAnswerFor.
	if view := daemonIdentity(mgmtURL); view != nil {
		answer, sysDir := stateDiskAnswerHere(stateDir)
		if f := stateDirFinding(answer, true, view.Enrolled, sysDir, runtime.GOOS); f.Subject != "" {
			out = append(out, f)
		}
	}

	// Whether the network and this device agree on its key. Placed next
	// to the connection check because it is the failure that looks
	// exactly like a healthy connection from every other angle.
	if f := deviceKeyFinding(ctx, mgmtURL); f.Subject != "" {
		out = append(out, f)
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
	obsFindings, engine := probeObservability(ctx, mgmtURL)
	out = append(out, obsFindings...)

	// Linux desktop tray host (waired#493): on GNOME the waired-tray SNI icon
	// does not render without an AppIndicator host extension. Surface a warn
	// finding (with the install/enable/re-login hint) when no SNI host is
	// present. Empty Subject — NotApplicable on servers / macOS / Windows, and
	// the zero value tests pass — is skipped.
	if tray.Finding.Subject != "" {
		out = append(out, tray.Finding)
	}

	// Claude Code entries waired wrote that this computer's shell cannot run
	// (waired-agent#787). Emits nothing for a machine waired never enabled
	// Claude Code on, so doctor stays quiet where the question does not apply.
	out = append(out, claudeCommandFindings(runtime.GOOS, claude)...)

	return out, engine
}

// unreadableFinding decides what to say when a check could not read what
// it needed.
//
// The distinction that matters is permission versus absence. An absent
// file means the check has nothing to report and something else in the
// output already explains why (the live probe, the gateway-token line) —
// staying quiet there is deliberate. A permission error means the check
// did not run at all, and before #651 that was indistinguishable from a
// pass: an unprivileged `waired doctor` printed 13 checks and exited 0
// while the elevated run printed 15 and found problems in two of them.
//
// A doctor that omits a check should say so. The row is StatusSkip, so it
// renders as `·` and — like every other skip — does not contribute to the
// exit code (countFails). What changes is only that the omission is
// visible; the verdict is unchanged.
//
// The second return reports whether there is anything to print at all.
func unreadableFinding(subject string, err error) (integration.AuditFinding, bool) {
	if !errors.Is(err, fs.ErrPermission) {
		return integration.AuditFinding{}, false
	}
	return integration.AuditFinding{
		Status:  integration.StatusSkip,
		Subject: subject,
		Detail:  "needs elevation to check; " + elevationHint("waired doctor"),
	}, true
}

// phaseFinding inspects <state>/runtime/state and reports the agent's
// current pause/resume mode. Returns an empty finding (caller skips)
// when the state file is missing or stale — the live probe further
// down will report the underlying daemon-not-running condition with a
// more useful message. A state dir this run may not read is reported as
// a skipped check rather than dropped (#651).
func phaseFinding(stateDir string) integration.AuditFinding {
	s, err := state.Read(stateDir)
	if err != nil {
		if f, ok := unreadableFinding("waired phase", err); ok {
			return f
		}
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
	_, _ = fmt.Fprintf(stdout, "Press f to fix [f/N]: ")
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "f" || s == "fix"
}

// repairWithUse re-applies the per-user integration for whoever the
// findings were about.
//
// Under sudo it delegates to `waired link all` running AS that user, the
// same hop runPostLoginIntegration uses. Applying in-process would write
// root-owned files into the user's ~/.claude and ~/.openclaw, and would
// create the ledger and gateway token under root's state dir — so before
// #650, when the diagnosis looked at /root, pressing `f` at least repaired
// the same /root it had just complained about. Now that the diagnosis
// follows the invoking user, the repair has to follow them too, ownership
// and all.
func repairWithUse(ctx context.Context, home doctorHome, stateDir, gatewayURL string) error {
	if home.SudoUser != "" && !home.Fellback {
		// ascii: a child process's streams. It is `waired` again, run as the
		// invoking user, and it folds its own output.
		return runLinkAllAsUser(ctx, home.SudoUser, linkAllChildArgs(gatewayURL), os.Stdout, os.Stderr)
	}
	res, err := setup.Integration(ctx, setup.IntegrationOptions{
		HomeDir:        home.Dir,
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
