package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/browser"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
)

// daemonReachable reports whether a waired-agent daemon is answering the
// Local Management API at mgmtURL. It is a package var so tests can stub
// the probe to exercise both branches of runInit. A status code below
// 500 (including the unenrolled 200) counts as reachable; only a
// transport error or 5xx means "no usable daemon".
var daemonReachable = func(mgmtURL string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(mgmtURL + "/waired/v1/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// loginPollInterval is how often the login loop re-reads /login/status —
// and, with it, how often the sign-in gate gets to notice a keystroke. A
// var so tests can shrink it: the loop is now the thing under test for
// #308, and a second per tick is a second per test.
var loginPollInterval = time.Second

// daemonInitOpts is everything runInitViaDaemon needs from `waired init`.
//
// A struct rather than a parameter list: this started as six positional
// arguments and had grown to twelve, three of them adjacent strings and
// five adjacent bools — a shape where a silent swap at a call site
// compiles and changes what the run does. Named fields make each call
// site say which knob it is setting.
type daemonInitOpts struct {
	MgmtURL        string
	Control        string
	DeviceName     string
	GatewayBaseURL string
	// StateDir is the agent's state directory. Read for agent.json
	// (the Claude gateway port and the managed-settings write options),
	// not written: the daemon owns this directory.
	StateDir        string
	NoBrowser       bool
	NonInteractive  bool
	SkipIntegration bool
	// SkipClaudeRoute is --skip-claude-route (WAIRED_NO_CLAUDE_PROXY /
	// the installers' -SkipClaudeProxy). init is the single decider of
	// Claude Code routing, and this is the opt-out (#294).
	SkipClaudeRoute bool
	// AuthKey enrolls without a browser sign-in, for servers and
	// containers.
	AuthKey string
	// Reauth asks the daemon to re-authenticate a device that is already
	// signed in. Without it a Start on an active session is an idempotent
	// no-op, and this function resumes rather than printing a successful
	// sign-in for a run that renewed nothing (#175, #313).
	Reauth bool
	// AccountEmail is what the daemon reports it is enrolled as, when the
	// caller already asked. Used only to name the account in the resume
	// notice: the no-op answer carries no session and no email of its own.
	AccountEmail string
	Inference    daemonInitInference
	// Owner is the process-wide stdin reader, or nil off a TTY.
	Owner *stdinReader
}

// runInitViaDaemon drives the daemon-owned login MGMT API instead of
// enrolling locally (the Tailscale model). It POSTs /login/start, opens
// the browser on the first login URL, then polls /login/status until the
// daemon reaches a terminal phase. The running daemon owns the runtime
// and the state dir, so the CLI does no deploy here; the per-user
// coding-agent integration consent — and the Claude Code routing flip it
// covers — run once login is active, because both write outside the
// daemon's reach (a user's home, and a root-owned machine-wide file).
func runInitViaDaemon(o daemonInitOpts) error {
	mgmtURL, gatewayBaseURL := o.MgmtURL, o.GatewayBaseURL
	noBrowser, nonInteractive := o.NoBrowser, o.NonInteractive
	skipIntegration, owner, inf := o.SkipIntegration, o.Owner, o.Inference
	reauth, authKey := o.Reauth, o.AuthKey

	reqBody, _ := json.Marshal(management.LoginStartRequest{
		ControlURL: o.Control,
		DeviceName: o.DeviceName,
		AuthKey:    o.AuthKey,
		Reauth:     reauth,
	})
	out, err := httpPost(mgmtURL+"/waired/v1/login/start", reqBody)
	if err != nil {
		// A control plane predating auth keys rejects the unknown field
		// outright, which reads as "your key is malformed" unless we say
		// otherwise.
		return classifyAuthKeyError(fmt.Errorf("start login via daemon: %w", err), authKey != "")
	}
	var st management.LoginStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return fmt.Errorf("decode login start: %w", err)
	}
	// A daemon that is already signed in answers with its idempotent
	// no-op: phase active, no session. There is nothing to poll and
	// nothing to sign in to — the run picks up from there.
	resuming := false
	if st.SessionID == "" {
		if st.Phase != management.LoginPhaseActive {
			return fmt.Errorf("the background service did not start a sign-in (it reported %q).\n"+
				"  Run `waired doctor` to check the service, then try again", st.Phase)
		}
		// An agent too old to know about `reauth` ignores the field and
		// answers with the same no-op. Saying "no session id" there would
		// send the operator looking for a bug in a daemon that is working
		// exactly as it was built to (#175).
		if reauth {
			return errors.New("this device is signed in, but the background service is too old to renew that sign-in.\n" +
				"  Update Waired, then run `waired init` again")
		}
		// #313: this is the resume NAVI prescribes for a stuck setup. It
		// used to be reported as a protocol failure, which on Windows was
		// the only outcome `waired init` had on an enrolled device.
		resuming = true
		if st.AccountEmail == "" {
			st.AccountEmail = o.AccountEmail
		}
	}

	switch {
	case resuming:
		for _, line := range resumeLines(st.AccountEmail, authKey != "") {
			fmt.Println(line)
		}
	case authKey != "":
		fmt.Println(bold("Signing in with an auth key"))
	default:
		fmt.Println(bold("Sign in"))
	}

	// One reader owns stdin for the rest of this run. Everything below
	// that reads the keyboard — the sign-in gate, the browser-setup
	// takeover offer, the coding-agent question, the benchmark prompts —
	// consumes from it, because two readers over one fd is how an answer
	// meant for one question ended up answering another (#184, #185).
	//
	// Only on a terminal: a piped stdin belongs to the script driving
	// init, and reading ahead from it would swallow input meant for a
	// later command in that script. Off a TTY the prompts keep the
	// on-demand scanner they have always used. runInit owns the decision
	// and hands the owner down, so the two init journeys can never end up
	// with two readers between them (#223).
	stdin := promptReader(owner)
	// Resolved once, outside the loop, so the decision is stable.
	mode := resolveGateFn(noBrowser, nonInteractive, isTerminal(os.Stdin), browser.HasDisplay())

	var gate *loginGate
	lastPhase := management.LoginPhase("")
	// #308: this deadline now bounds the operator's think-time at the
	// Enter prompt too. It never used to: the gate blocked the loop, so
	// the clock only started once Enter had been pressed.
	deadline := time.Now().Add(12 * time.Minute)

	for {
		if st.LoginURL != "" && gate == nil {
			// gcloud-style gate: URL first, browser only on Enter (or
			// immediately when the session can't answer a prompt). It
			// returns rather than reading, and the loop polls it below —
			// blocking here is what made a browser-driven sign-in report
			// a failure on every wizard step (#308). See login_gate.go.
			gate = presentLoginURL(owner, os.Stdout, st.LoginURL, st.UserCode, mode)
		}

		switch st.Phase {
		case management.LoginPhaseActivating, management.LoginPhaseActive, management.LoginPhaseError:
			// The sign-in resolved without the Enter the gate offered.
			// Withdraw before anything else prints, so the phase line
			// below lands on its own line and not on the parked prompt.
			gate.Withdraw(os.Stdout)
		}

		if st.Phase != lastPhase {
			if st.Phase == management.LoginPhaseActivating {
				fmt.Println("Signed in — starting Waired on this device...")
			}
			lastPhase = st.Phase
		}

		switch st.Phase {
		case management.LoginPhaseActive:
			fmt.Printf("\n%s %s\n", emo("✅", "*"), bold("Device signed in"))
			if st.AccountEmail != "" {
				fmt.Printf("Logged in as: %s\n", st.AccountEmail)
			}
			fmt.Println("Waired is signed in and running in the background.")

			// waired#835 §9: attach as the setup executor BEFORE any stdin
			// prompt. The browser wizard may already be on screen, and the
			// two prompts below block on stdin — an executor attached after
			// them would arrive minutes late, or never.
			sess := attachSetupExecutor(mgmtURL, elevation.IsElevated())
			defer sess.Release()

			// waired#835 §11.2: the installer passes the inference
			// answers to `waired init`, but the daemon path never read
			// them (LoginStartRequest carries only a control URL and a
			// device name). Re-apply them through the management routes
			// that already own these three controls, before anything
			// below can act on a stale answer.
			applyDaemonInitInference(mgmtURL, inf, os.Stdout)

			budget, setupActive, enter, watch := awaitBrowserSetup(sess, owner, os.Stdout, nonInteractive, noBrowser)

			// §11: on this path init returned long before reaching the
			// standalone engine block, so nothing here could ever install
			// an engine and the wizard's first step could only report
			// permission_denied. As the elevated executor holding the
			// lease, do the install the browser just asked for. Blocking
			// is correct: the model pull below has nothing to pull with
			// until an engine exists.
			var engineErr error
			if setupActive {
				engineErr = runSetupEngineInstall(context.Background(), sess, os.Stdout)
			} else {
				// No wizard is driving, but this host may still want an
				// engine and have none — the default macOS install has
				// been landing here all along, and the §11.2 ordering
				// flip puts Linux and Windows here too. Condition is
				// "does the host want inference", read from the daemon.
				engineErr = ensureDaemonPathEngine(context.Background(), sess, mgmtURL, os.Stdout)
			}

			// #308: the engine step above can run for minutes, so the
			// browser setup may have started long after awaitSetupBudget's
			// grace gave up on one. Read the edge here, where setupActive
			// still decides what this terminal is allowed to say and ask.
			engineComing := engineArrivalPending(sess.State())
			if started, setupBudget, coming := watch.Poll(); started && !enter.Fired() {
				enter.Close(os.Stdout)
				setupActive, budget, engineComing = true, setupBudget, coming
			}

			if engineErr != nil {
				// #188: the install failed, so there is nothing for the
				// model wait to wait for and nothing for the benchmark to
				// measure. Say what happened once, here, instead of
				// parking the terminal on "Waiting for the AI engine to
				// start…" until the setup budget runs out.
				printEngineInstallFailure(os.Stdout, engineErr, setupActive)
			} else {
				// #756: the daemon pulls the bundled model in the background
				// after enroll, so the daemon-mediated init used to return while a
				// multi-GB download ran invisibly. Block in the foreground with the
				// same percentage progress bar the local path shows (main.go), then
				// benchmark the ready model. waitForBundledModel returns fast when
				// the daemon reports inference disabled / stopped / no engine, so
				// this never hangs an under-spec or gateway-only host.
				//
				// waired#939: say it once more before the longest wait of
				// the flow. The engine install above can take minutes, so
				// the warning printed at the handoff has scrolled away by
				// then — and this is the stretch where a terminal that looks
				// idle invites closing it.
				if setupActive && !enter.Fired() {
					writePrompt(os.Stdout, setupKeepTerminalOpenLine)
				}
				waitForBundledModel(mgmtURL, os.Stdout, isTerminal(os.Stdout), budget,
					engineComing, enter, watch)
			}
			// #308: a setup that started during the wait leaves this
			// terminal as the browser's executor, so it must stop asking
			// its own questions below (§4.2). Applied before the takeover
			// check, which overrides it: an operator who did take the
			// terminal back owns it regardless.
			if watch.Started() {
				setupActive = true
			}
			if enter.Fired() {
				// The operator took the terminal back. The lease is
				// deliberately KEPT (waired-agent#198): this used to
				// release it, and the wizard — which cannot tell a
				// deliberate handoff from a crash — reported
				// `executor_gone` and sent the operator back to a machine
				// that was in fact busy setting itself up. Claiming the
				// driver instead says which of the two happened, and stays
				// honest by construction: if this process dies the lease
				// expires and the claim dies with it.
				sess.TakeOver()
				setupActive = false
			}

			// §4.2: while the browser is driving setup, the terminal must not
			// ask its own questions. Both prompts below read stdin, and the
			// benchmark one can additionally offer to SWITCH the active model
			// — a second writer racing desired_model_id, and a recommendation
			// §20.6 says v1 must not make.
			//
			// #186: this block used to sit ABOVE the engine install, so in
			// terminal mode it interrupted a multi-GB download to ask about
			// coding tools, and a terminal that took over late was never
			// asked at all. Asking here fixes both — setupActive is settled
			// by now, and the download is done.
			// claudeManaged: can this process write the machine-wide
			// Claude Code managed settings at all? Elevation is the real
			// gate (the installers run init elevated); an OS with no
			// managed-settings location can never be routed.
			claudeManaged := claudeManagedEligibleFor(isElevatedFn(), claudemanaged.Path())
			// integConsent feeds the routing decision below. Both branches
			// that can set it are the same consent, asked in two places:
			// the wizard's coding-tool toggles, or the terminal question.
			integConsent := false
			if setupActive {
				// waired#935: the browser asks which coding tools to
				// connect, and this process is the only one that can write
				// them — the daemon runs as a service account with no
				// business in a user's home, and Claude Code's settings are
				// root-owned. Warn-only, like every other integration path:
				// sign-in already succeeded, and the step reports its own
				// failure to the wizard.
				if err := runSetupIntegrations(sess, os.Stdout, os.Stderr, setupIntegrationOpts{
					GatewayBaseURL:  gatewayBaseURL,
					StateDir:        o.StateDir,
					SkipClaudeRoute: o.SkipClaudeRoute,
				}); err != nil {
					fmt.Fprintf(os.Stderr,
						"warn: coding-tool setup had problems (%v); re-run later: waired link --force all\n", err)
				} else if sess.State().Integrations == nil {
					// No instruction at all — an older control plane, or a
					// wizard that has not asked yet.
					fmt.Println("You can set up your coding tools later from this terminal with `waired link all`.")
				}
				// The wizard's own claude-code toggle is the consent, and
				// the routing it promises is applied by runSetupIntegrations
				// (setup_integration.go) — not here. §4.2: this terminal
				// must not ask, so the block below stays out of its way.
			} else if skipIntegration {
				fmt.Println("Run `waired link <agent>` to (re)configure coding-agent integration if needed.")
			} else {
				consented, err := runPostLoginIntegration(postLoginIntegrationOpts{
					StepLabel:       emo("🔌", "*"),
					GatewayBaseURL:  gatewayBaseURL,
					NonInteractive:  nonInteractive,
					In:              stdin,
					Out:             os.Stdout,
					ErrOut:          os.Stderr,
					ClaudeManaged:   claudeManaged,
					SkipClaudeRoute: o.SkipClaudeRoute,
				})
				if err != nil {
					// Warn-only: login already succeeded; a broken integration
					// must not turn it into a failed init.
					fmt.Fprintf(os.Stderr,
						"warn: coding-agent integration had problems (%v); re-run later: waired link --force all\n", err)
				}
				integConsent = consented
			}

			var resp *management.BenchmarkRunResponse
			switch {
			case setupActive:
				// waired#939: the degraded wording. Everything this process
				// owed the setup is done and init is about to return, so the
				// keep-open instruction no longer applies — saying it here
				// would leave the operator guarding a terminal for nothing,
				// and the wizard would be telling them the opposite.
				fmt.Println(setupTerminalDoneLine)
			case engineErr != nil:
				// Nothing to measure: there is no engine.
			default:
				// #133: once the daemon has the model ready, benchmark it and
				// offer a lighter model if this host can't sustain the pick.
				resp, _ = benchmarkWithScanner(mgmtURL, nonInteractive, os.Stdout, stdin, isTerminal(os.Stdout))
			}
			// Claude Code request routing (#294). The installers deleted
			// their own post-init `waired claude enable` and forward the
			// opt-out here instead, making init the single decider — but
			// only the deleted standalone path ever wrote managed
			// settings, so every real install (all of which take this
			// daemon path) finished unrouted and --skip-claude-route had
			// nothing to skip.
			//
			// Asked HERE, after the engine install, the model download and
			// the benchmark: waired#772's deferred question, so a "yes"
			// flips the route at the moment the local stack can actually
			// serve. Skipped entirely while the wizard drives — its own
			// claude-code toggle already decided, and §4.2 forbids a
			// terminal prompt.
			claudeRouted := false
			if !setupActive {
				switch planClaudeRoute(claudeRouteFacts{
					integConsent:    integConsent,
					elevated:        isElevatedFn(),
					managedPath:     claudemanaged.Path(),
					skipClaudeRoute: o.SkipClaudeRoute,
					nonInteractive:  nonInteractive,
				}) {
				case claudeRouteAsk:
					claudeRouted = promptClaudeRouting(os.Stdout, stdin, o.StateDir)
				case claudeRouteApply:
					claudeRouted = routeClaudeNow(claudeRouteApplyOpts{
						StateDir: o.StateDir, In: stdin, AllowPrompt: false,
					}, os.Stdout)
				case claudeRouteNeedsElevation:
					// waired#749: say it instead of skipping silently — the
					// consent copy above already described the machine-wide
					// change this run turns out not to be able to make.
					printClaudeRouteElevationHint(os.Stdout)
				case claudeRouteNone:
					// No consent, an explicit opt-out, or an OS with no
					// managed-settings location: nothing to say.
				}
			}

			// #756: the daemon chose the inference role from this host's
			// hardware without an interactive prompt, so tell the user how to
			// inspect and change it afterward.
			printInferenceRoleGuidance(os.Stdout)
			if engineErr != nil {
				printDaemonEngineFailedBox(st.AccountEmail)
			} else {
				printDaemonSuccessBox(st.AccountEmail, outcomeFrom(resp), claudeRouted)
			}
			// Sign-in succeeded either way, so init succeeds either way:
			// the engine is a part of setup, not the point of it (#188).
			return nil
		case management.LoginPhaseError:
			if st.Error != "" {
				return fmt.Errorf("login failed: %s", st.Error)
			}
			return errors.New("login failed")
		}

		if time.Now().After(deadline) {
			return errors.New("login timed out waiting for the daemon")
		}
		// The sign-in step's side of the keyboard: open the browser on the
		// Enter the prompt gate offered, or answer the stray one the
		// print-only gate has no use for (#184). Both are non-blocking —
		// see login_gate.go.
		gate.Poll(os.Stdout)
		time.Sleep(loginPollInterval)

		body, err := httpGet(mgmtURL + "/waired/v1/login/status?session=" + url.QueryEscape(st.SessionID))
		if err != nil {
			return fmt.Errorf("poll login status: %w", err)
		}
		var next management.LoginStatus
		if err := json.Unmarshal(body, &next); err != nil {
			return fmt.Errorf("decode login status: %w", err)
		}
		st = next
	}
}

// printInferenceRoleGuidance tells the operator how to inspect and change the
// local inference role after a daemon-mediated init. Unlike the local init
// path, the daemon picks the role from the host's hardware with no interactive
// prompt (waired#756), so surface the commands that let the user revisit it.
// Only verified subcommands are listed.
func printInferenceRoleGuidance(out io.Writer) {
	writePrompt(out)
	writePrompt(out, dim("Inference role was set from this host's hardware. To inspect or change it:"))
	writePrompt(out, dim("  waired runtimes benchmark            re-check performance / switch to a lighter model"))
	writePrompt(out, dim("  waired models ls                     list installed and available models"))
	writePrompt(out, dim("  waired inference share on|off        expose (or stop exposing) this engine to mesh peers"))
	writePrompt(out, dim("  waired inference engine stop|start   power the local engine down / up"))
	writePrompt(out, dim("  re-run `waired init`                 reconfigure inference from scratch"))
}

// printEngineInstallFailure is the one block `waired init` prints when
// the engine install it just ran failed (#188).
//
// It exists because the failure used to be invisible from the terminal:
// runSetupEngineInstall reported the outcome to the daemon and returned
// nothing, so init walked into a model wait for an engine that was never
// coming and sat there for up to the whole setup budget. Everything the
// operator needs to recover is here — what failed, the exact elevated
// command that retries it, and (when a wizard is watching) the fact that
// the same failure is already on their screen.
func printEngineInstallFailure(out io.Writer, err error, setupActive bool) {
	writePrompt(out)
	writePromptf(out, "%s %s\n", emo("⚠️", "!"), bold("The AI engine could not be installed on this device."))
	writePromptf(out, "  %v\n", err)
	writePrompt(out)
	writePrompt(out, "  Sign-in worked — this device is signed in and running. Only local AI is missing.")
	writePromptf(out, "  Retry the install with: %s\n", cyan(elevation.Hint("waired init")))
	if setupActive {
		writePrompt(out, "  "+dim("The setup page in your browser shows this same failure."))
	}
	writePrompt(out)
}

// printDaemonEngineFailedBox is the summary for a run that signed the
// device in but could not install the engine. Deliberately not the
// success box: "everything completed successfully" over a failed engine
// install is how an operator ends up not realising local AI never
// arrived (#188).
func printDaemonEngineFailedBox(accountEmail string) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("Local AI is not installed yet; the command above finishes it."))
	boxWarn(os.Stdout, emo("⚠️", "!"), "Waired is signed in — local AI still needs installing", lines)
}

// printDaemonSuccessBox renders the final "Waired is ready" summary for the
// daemon-driven journey. The daemon owns the runtime, so we only surface the
// account, (when the benchmark ran) the measured throughput, and where
// Claude Code's requests now go.
//
// claudeRouted is reported either way (#294): "routed" is the whole point
// of a first install for a Claude Code user, and a box that stays silent
// when routing did not happen is how an operator walks away believing it
// did.
func printDaemonSuccessBox(accountEmail string, bench benchmarkOutcome, claudeRouted bool) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	if bench.Measured {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Model", green(fmt.Sprintf("%.0f tok/s", bench.Tokps))))
	}
	if claudeRouted {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Claude", green("routed through Waired")))
	} else {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Claude", dim("still using the Anthropic API")))
	}
	lines = append(lines, dim("Local inference is live via the waired-agent daemon."))
	lines = append(lines, dim("Point your coding agent at Waired and start building."))
	box(os.Stdout, emo("🎉", "*"), "Waired is ready — everything completed successfully!", lines)
}
