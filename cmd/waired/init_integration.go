package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/openclaw"
	"github.com/waired-ai/waired-agent/internal/integration/opencode"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// This file implements the coding-agent integration consent step shared
// by `waired init`'s standalone and daemon-driven journeys.
//
// Policy (docs/decisions.md): the integration is no longer gated on
// Detect() — the operator is asked exactly once, default Yes, and a Yes
// force-applies every adapter so the integration activates the moment
// the coding agent is installed. Detection results are shown as
// informational context only. Non-interactive runs enable the
// integration too (--skip-integration is the opt-out). The integration
// writes only per-user config (Claude Code skills + OpenCode plugin); it
// never edits shell rc files. Claude request routing is the Claude Code
// managed settings (ANTHROPIC_BASE_URL), set up separately as root at init time.

// agentDetection is one adapter's Detect() outcome reduced to what the
// consent prompt renders.
type agentDetection struct {
	ID     integration.AgentID
	Found  bool
	Detail string // first Detect note, e.g. "claude on PATH: /usr/bin/claude"
}

// detectIntegrationAgents runs Detect for the default adapter set
// against homeDir. Informational only — never gates the consent.
func detectIntegrationAgents(ctx context.Context, homeDir string) []agentDetection {
	var out []agentDetection
	for _, a := range []integration.Adapter{claudecode.New(), opencode.New(), openclaw.New()} {
		d := agentDetection{ID: a.ID()}
		if det, err := a.Detect(ctx, integration.ApplyOptions{HomeDir: homeDir}); err == nil {
			d.Found = det.Found
			if len(det.Notes) > 0 {
				d.Detail = det.Notes[0]
			}
		}
		out = append(out, d)
	}
	return out
}

// integrationConsentInput bundles what promptIntegrationConsent renders.
type integrationConsentInput struct {
	StepLabel      string // "🔌 [3b/4]" under runInit; a plain header otherwise
	Detections     []agentDetection
	NonInteractive bool
	SudoTarget     string // non-empty: integration lands in this user's home, not root's
	// ClaudeManaged is true when `waired init` will write the system-wide Claude
	// Code managed settings (elevated init; #488). It makes the consent disclose
	// that ANTHROPIC_BASE_URL is set for all Claude Code users on this machine.
	ClaudeManaged bool
}

// printAgentDetections renders the per-agent detection status lines
// shown above the consent prompts (init and `waired link all`). The agent id
// is padded before colouring so the columns stay aligned under color (ANSI
// escapes have no display width).
func printAgentDetections(out io.Writer, dets []agentDetection) {
	bullet := emo("•", "-")
	for _, d := range dets {
		status := "not detected — can be set up now; activates once installed"
		if d.Found {
			status = "detected"
			if d.Detail != "" {
				status = "detected — " + d.Detail
			}
		}
		writePromptf(out, "    %s %s %s\n", bullet, cyan(fmt.Sprintf("%-12s", string(d.ID))), status)
	}
}

// promptIntegrationConsent prints the per-agent detection status and
// asks the single integration consent question (default Yes). The Yes
// covers the per-user integration the link path writes — the Claude Code
// skills, the OpenCode plugin, and the OpenClaw plugin — plus, when
// ClaudeManaged is set, consent to write the system-wide Claude Code managed
// settings (ANTHROPIC_BASE_URL -> local gateway, no credential; #488).
// Non-interactive resolves to Yes without reading stdin.
//
// Detection is informational only: a Yes force-applies every adapter even
// when the agent is not installed yet (Detect()=false), so the integration
// activates the moment the user later installs OpenCode / OpenClaw.
func promptIntegrationConsent(in io.Reader, out io.Writer, inp integrationConsentInput) bool {
	writePromptf(out, "%s %s%s%s%s%s%s\n", inp.StepLabel,
		bold("Coding-agent integration"), dim("  —  "),
		product("Claude Code"), dim(" · "), product("OpenCode"), dim(" · ")+product("OpenClaw"))

	writePrompt(out)
	writePrompt(out, "  "+dim("Detected on this machine:"))
	printAgentDetections(out, inp.Detections)
	if inp.SudoTarget != "" {
		writePromptf(out, "\n  %s\n",
			yellow(fmt.Sprintf("Running under sudo — this will be set up for user %q, not root.", inp.SudoTarget)))
	}

	writePrompt(out)
	writePrompt(out, "  "+dim("Waired will install (each activates the moment its agent is installed):"))
	b := emo("•", "-")
	// item renders "<product> <kind>" then pads by DISPLAY width (ANSI-agnostic)
	// so the paths line up under color too. The single space between name and
	// kind keeps "Claude Code skills" / "OpenCode plugin" contiguous for the
	// substring assertions; product() is plain text off-TTY.
	item := func(name, kind, path string) {
		label := product(name) + " " + kind
		pad := max(20-displayWidth(label), 1)
		writePromptf(out, "    %s %s%s%s\n", b, label, strings.Repeat(" ", pad), dim(path))
	}
	item("Claude Code", "skills", "~/.claude/skills/")
	item("OpenCode", "plugin", "~/.config/opencode/plugin/waired.js")
	item("OpenClaw", "plugin", "~/.openclaw/plugins/waired/")

	if inp.ClaudeManaged {
		writePrompt(out)
		writePromptf(out, "  For %s it also writes system-wide %s so\n", product("Claude Code"), bold("managed settings"))
		writePromptf(out, "  %s points at your local gateway — %s, so your\n", bold("ANTHROPIC_BASE_URL"), bold("no credential"))
		writePrompt(out, "  claude.ai subscription and auto-mode keep working. Local inference serves")
		writePrompt(out, "  requests and falls back to the real Anthropic API when unavailable, so claude")
		writePromptf(out, "  never breaks. Reverse anytime with %s.\n", cyan("sudo waired claude disable"))
	}

	if inp.NonInteractive {
		writePromptf(out, "\n  %s\n",
			dim("non-interactive: enabling coding-agent integration (pass --skip-integration to opt out)"))
		return true
	}
	writePrompt(out)
	sc := bufio.NewScanner(in)
	ok := ynPrompt(out, sc, "Set up coding-agent integration?", true)
	if !ok {
		writePromptf(out, "  Skipped. Set up the per-user integration anytime with: %s\n", cyan("waired link"))
		writePrompt(out, "  "+dim("(Claude request routing is configured separately with `sudo waired claude enable`.)"))
	}
	return ok
}

// invokingSudoUser reports the non-root user who ran `sudo waired
// init`. euid must be 0 and SUDO_USER set to a non-root name; anything
// else (a real root login, or no elevation at all) has no hop target
// and the integration applies to the current user as usual.
func invokingSudoUser() (string, bool) {
	return invokingSudoUserAt(runtime.GOOS, os.Geteuid(), os.Getenv("SUDO_USER"))
}

// invokingSudoUserAt is the testable core of invokingSudoUser. sudo is
// a Unix concept: on Windows there is no hop target (euid is -1 there
// anyway, but the goos check makes that explicit).
func invokingSudoUserAt(goos string, euid int, sudoUser string) (string, bool) {
	if goos != "linux" && goos != "darwin" {
		return "", false
	}
	if euid != 0 {
		return "", false
	}
	if sudoUser == "" || sudoUser == "root" {
		return "", false
	}
	return sudoUser, true
}

// sudoUserHome resolves the home directory of the sudo-invoking user.
// Best-effort: with CGO disabled user.Lookup only reads /etc/passwd, so
// NSS/LDAP users can miss — callers fall back to the process home for
// the (informational) detection lines; the hop itself resolves the user
// via runuser/sudo which do full NSS.
func sudoUserHome(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("user %s has no home directory", username)
	}
	return u.HomeDir, nil
}

// linkAllChildArgs builds the argv for the per-user `waired link all`
// child the sudo hop spawns. It applies only the non-routing per-user
// integration (Claude skills + OpenCode plugin); Claude
// request routing is the Claude Code managed settings, set up separately as
// root at init time.
//
// Flags MUST precede the "all" target: stdlib flag parsing stops at the
// first non-flag argument, so anything after "all" would be silently
// ignored.
func linkAllChildArgs(gatewayBaseURL string) []string {
	return []string{"link", "--force", "--no-prompt", "--gateway-base-url", gatewayBaseURL, "all"}
}

// scrubbedChildEnv drops env vars that would point the per-user child
// at root's state/config (a stray `sudo -E` leak). runuser / sudo -H
// set HOME for the target user themselves; TERM/LANG/LC_* pass through
// so the child's output renders like the parent's.
func scrubbedChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") ||
			strings.HasPrefix(kv, "WAIRED_STATE_DIR=") ||
			strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runLinkAllAsUser runs `waired link all …` as username, so HOME, the
// per-user state dir (ledger + gateway token), and file ownership all
// resolve for the invoking user instead of root. runuser (util-linux,
// Essential on Debian) is preferred — it resolves the user via NSS and
// needs no sudoers entry; `sudo -u <user> -H` is the fallback. stdin is
// closed (consents ride in childArgs); stdout/stderr stream through so
// the child's summary appears inline.
func runLinkAllAsUser(ctx context.Context, username string, childArgs []string, out, errW io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve waired binary: %w", err)
	}
	var argv []string
	if p, lookErr := exec.LookPath("runuser"); lookErr == nil {
		argv = append([]string{p, "-u", username, "--", self}, childArgs...)
	} else if p, lookErr := exec.LookPath("sudo"); lookErr == nil {
		argv = append([]string{p, "-u", username, "-H", "--", self}, childArgs...)
	} else {
		return fmt.Errorf("neither runuser nor sudo is available to switch to user %s", username)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = scrubbedChildEnv(os.Environ())
	cmd.Stdin = nil
	cmd.Stdout = out
	cmd.Stderr = errW
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}

// postLoginIntegrationOpts parameterises runPostLoginIntegration.
type postLoginIntegrationOpts struct {
	StepLabel      string
	GatewayBaseURL string
	NonInteractive bool
	In             io.Reader
	Out            io.Writer
	ErrOut         io.Writer
}

// runPostLoginIntegration is the consent → apply-or-hop → summary
// sequence for journeys where setup.Init's in-process integration phase
// does not run: the daemon-driven login. It asks for consent (default
// Yes), then either applies in-process as the current user or — under
// sudo — delegates to `waired link all` as the invoking user. Errors are
// returned for the caller to treat warn-only (login already succeeded).
func runPostLoginIntegration(o postLoginIntegrationOpts) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	homeDir, _ := os.UserHomeDir()
	sudoUser, isSudo := invokingSudoUser()
	targetHome := homeDir
	if isSudo {
		if h, err := sudoUserHome(sudoUser); err == nil {
			targetHome = h
		}
	}

	if !promptIntegrationConsent(o.In, o.Out, integrationConsentInput{
		StepLabel:      o.StepLabel,
		Detections:     detectIntegrationAgents(ctx, targetHome),
		NonInteractive: o.NonInteractive,
		SudoTarget:     sudoUser,
	}) {
		return nil
	}

	if isSudo {
		writePromptf(o.Out, "%s %s\n", emo("🔌", "*"),
			bold(fmt.Sprintf("Setting up coding-agent integration for user %q…", sudoUser)))
		return runLinkAllAsUser(ctx, sudoUser, linkAllChildArgs(o.GatewayBaseURL), o.Out, o.ErrOut)
	}

	res, err := setup.Integration(ctx, setup.IntegrationOptions{
		HomeDir:        homeDir,
		StateDir:       defaultStateDir(),
		GatewayBaseURL: o.GatewayBaseURL,
		NonInteractive: o.NonInteractive,
		Force:          true,
		WiredBinary:    wairedBinaryPath(),
	})
	if err != nil {
		return err
	}
	printIntegrationSummary(res)
	for _, ar := range res.Agents {
		if ar.Err != nil {
			return fmt.Errorf("integration: %s: %w", ar.Agent, ar.Err)
		}
	}
	printSetupHelper("all", helperPrintOptions{
		HomeDir:     homeDir,
		WiredBinary: wairedBinaryPath(),
		Interactive: false,
	}, o.Out, o.In)
	return nil
}
