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
	// AuthOnlyRefresh marks a run that is rotating an already-enrolled
	// device's credentials (`waired init --force-reauth` on a signed-in
	// host). Such a run does not re-ask the terminal's coding-agent
	// question — the device answered it once — but it DOES apply a
	// coding-tools instruction the control plane is still holding and
	// this device has never written (waired-agent#987).
	AuthOnlyRefresh bool
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
	authOnlyRefresh := o.AuthOnlyRefresh
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
			// No phase name in the copy: "unenrolled" / "logging_in" are
			// this protocol's words, not the operator's, and the previous
			// wording ("no login session id") is the whole reason #313
			// read as a protocol bug rather than a working daemon.
			return errors.New("the background service did not start a sign-in.\n" +
				"  Run `waired init` again; if it keeps happening, `waired doctor` says what is wrong with the service")
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
			gate = presentLoginURL(owner, os.Stdout, st.LoginURL, st.UserCode, o.Control, mode)
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
				fmt.Printf("Signed in as: %s\n", st.AccountEmail)
			}
			fmt.Println("Waired is signed in and running in the background.")

			// The re-run gate (#782). Before the executor lease, before
			// any instruction is applied, and before the first prompt:
			// everything below this line either asks a question whose
			// default changes the host, or takes a lease that makes this
			// process the thing the browser wizard is waiting on. Answering
			// "no, leave it alone" has to happen while neither is true yet.
			//
			// `reauth` is deliberately NOT part of the stated intent, and
			// was until waired-agent#803 made the difference visible: it is
			// true when the DAEMON reports the sign-in expired, which is a
			// fact about the credentials rather than a request to
			// reconfigure the host. Re-authentication has already completed
			// by the time this runs — it happens in the sign-in loop above —
			// so declining here leaves a host that is authenticated again
			// and otherwise untouched. That is what main.go's own
			// `reauth && renewing` branch means by an auth-only refresh
			// leaving "whatever hardware / integration state is already on
			// disk" alone; the speed gates are simply the part of that state
			// `skipIntegration` does not cover.
			if !confirmSetupRerun(os.Stdout, stdin, rerunFactsFor(mgmtURL, !nonInteractive, !inf.empty())) {
				return nil
			}

			// waired#835 §9: attach as the setup executor BEFORE any stdin
			// prompt. The browser wizard may already be on screen, and the
			// two prompts below block on stdin — an executor attached after
			// them would arrive minutes late, or never.
			sess := attachSetupExecutor(mgmtURL, elevation.IsElevated())
			defer sess.Release()
			// #746: an attach that failed for anything but "this daemon
			// is older than the routes" leaves the setup steps below
			// silently inert. Say so once, here, where the reason is
			// still in hand.
			reportAttachNote(os.Stdout, sess)

			// waired#835 §11.2: the installer passes the inference
			// answers to `waired init`, but the daemon path never read
			// them (LoginStartRequest carries only a control URL and a
			// device name). Re-apply them through the management routes
			// that already own these three controls, before anything
			// below can act on a stale answer.
			applyDaemonInitInference(mgmtURL, inf, os.Stdout)

			budget, setupActive, enter, watch := awaitBrowserSetup(sess, owner, os.Stdout, nonInteractive, noBrowser, authKey != "" && !resuming)

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
				// waired-agent#586: tell the daemon the model question is
				// coming, BEFORE the engine step — the fallback download
				// dispatches the moment the host-speed measurement lands,
				// which is always before a human has answered the picker
				// below, and there is no way to cancel it afterwards. The
				// daemon bounds the claim server-side, so an interrupted
				// init converges to the fallback like an abandoned wizard.
				// Skipped when the picker itself will be: an explicit pin
				// answers the question, and --non-interactive keeps the
				// daemon's auto-selection.
				if !nonInteractive && inf.ModelID == "" {
					postModelChoicePending(mgmtURL, true)
				}
				// No wizard is driving, but this host may still want an
				// engine and have none — the default macOS install has
				// been landing here all along, and the §11.2 ordering
				// flip puts Linux and Windows here too. Condition is
				// "does the host want inference", read from the daemon.
				engineErr = ensureDaemonPathEngine(context.Background(), sess, mgmtURL, os.Stdout,
					inf, nonInteractive, stdin)
			}

			// #308: the engine step above can run for minutes, so the
			// browser setup may have started long after awaitSetupBudget's
			// grace gave up on one. Read the edge here, where setupActive
			// still decides what this terminal is allowed to say and ask.
			// The engine step above has run to completion by now, whichever
			// branch took it, so this process is not waiting for an install
			// it is itself performing (#778). Only a claim held by ANOTHER
			// lease keeps the arrival window open.
			engineComing := engineArrivalPendingAfterInstall(sess.State(), true)
			if started, setupBudget, coming := watch.Poll(); started && !enter.Fired() {
				enter.Close(os.Stdout)
				setupActive, budget, engineComing = true, setupBudget, coming
			}

			integOpts := setupIntegrationOpts{
				GatewayBaseURL:  gatewayBaseURL,
				StateDir:        o.StateDir,
				SkipClaudeRoute: o.SkipClaudeRoute,
			}
			// waired-agent#311: the coding tools go HERE, between the
			// engine install and the model download, not after both.
			//
			// This is the one step that still needs a human and their
			// administrator rights, and it used to sit behind the longest
			// unattended wait in the flow — tens of gigabytes, sometimes
			// hours. People walked away during the download and came back
			// to a wizard blocked on coding tools, on a machine that was
			// otherwise finished. Front-loading it makes the transfer the
			// tail, which is the part nobody has to watch.
			//
			// Deliberately ahead of the engineErr branch below: these are
			// files in a home directory, and a host whose engine install
			// failed can still have its coding tools connected.
			// A re-auth run applies a stored instruction nobody has
			// written yet. setupActive answers "is a browser driving this
			// run", which is the right gate for a handoff and the wrong
			// one for this: an instruction stored before the run started
			// is stale BY CONSTRUCTION (the daemon never watched it
			// change), so on a re-auth neither this call nor the terminal
			// question below reached it, and the row ended
			// failed/executor_gone with the plugin unwritten
			// (waired-agent#987). Owner ruling waired-agent#599 (2026-08-09)
			// puts the rest of the replay — engine, host speed, benchmark
			// — on this same path already.
			applyStored := authOnlyRefresh && !skipIntegration && sess.State().IntegrationsPending
			integrationsRan := runWizardIntegrations(sess, setupActive || applyStored, integOpts)

			// modelWait carries WHY the wait ended, so the summary below
			// can tell "signed in, local AI is running" apart from
			// "signed in, the engine on this device is dead" (#310). It
			// stays zero when the install already failed — that outcome
			// has its own box, and no wait ran to have an opinion.
			var modelWait modelWaitResult
			// terminalDoneSaid stops the "nothing more is needed from this
			// terminal" line being printed twice: it is now said as soon as
			// the executor's work is over, which on the ordinary path is
			// before the download rather than after it.
			terminalDoneSaid := false
			if engineErr != nil {
				// #188: the install failed, so there is nothing for the
				// model wait to wait for and nothing for the benchmark to
				// measure. Say what happened once, here, instead of
				// parking the terminal on "Waiting for the AI engine to
				// start…" until the setup budget runs out.
				//
				// Skipping the wait is right for the opt-out too — no engine
				// is coming either way — but the failure block is not: the
				// arm already said the install was skipped and named the
				// variable, and following the operator's own instruction with
				// printEngineInstallFailure's headline reports their decision
				// back to them as a fault (#551). The closing box says what
				// this host ended up with.
				//
				// Both of those strings are grepped by the installtest
				// harnesses and pinned by harness-failure-strings-guard.sh, so
				// they are deliberately NOT quoted here: a comment holding the
				// literal is enough to keep that guard green through a rename.
				if !errors.Is(engineErr, errEngineOptOut) {
					printEngineInstallFailure(os.Stdout, engineErr, setupActive)
				}
				// The model question is not coming — no engine means no
				// picker — so withdraw the #586 claim registered above.
				postModelChoicePending(mgmtURL, false)
			} else {
				// #756: the daemon pulls the bundled model in the background
				// after enroll, so the daemon-mediated init used to return while a
				// multi-GB download ran invisibly. Block in the foreground with the
				// same percentage progress bar the local path shows (main.go), then
				// benchmark the ready model. waitForBundledModel returns fast when
				// the daemon reports inference disabled / stopped / no engine, so
				// this never hangs a gateway-only host, or one below the recommended spec,.
				//
				// waired#939 asks for one line here, before the longest
				// wait of the flow: the engine install above can take
				// minutes, so the warning printed at the handoff has
				// scrolled away, and this is the stretch where a terminal
				// that looks idle invites closing it.
				//
				// WHICH line depends on whether this process still owes the
				// setup anything (waired-agent#311). Once the coding tools
				// are written above, it does not — the download belongs to
				// the background service — so "keep this open" would be an
				// instruction with nothing behind it, which is exactly the
				// restatement #939 asks to degrade. Closing the terminal
				// here is safe because the finished row survives the lease:
				// waired-agent#312 persists it.
				if setupActive && !enter.Fired() {
					if integrationsRan {
						writePrompt(os.Stdout, setupTerminalDoneLine)
						terminalDoneSaid = true
					} else {
						writePrompt(os.Stdout, setupKeepTerminalOpenLine)
					}
				}
				// Install-flow step 6 (waired-agent#585): wait for the
				// one-time host-speed measurement here — between the
				// engine install and the model wait, which is when the
				// daemon takes it (#496/#579) — and ask when it misses
				// the budget, instead of the cutoff defaulting local AI
				// off with the terminal sitting right there. Gated off
				// while a browser wizard drives (§4.2), and stillMine
				// re-checks at prompt time for one that started during
				// the wait.
				keptOn := true
				if !setupActive {
					keptOn = confirmHostSpeedBudget(mgmtURL, inf, nonInteractive, stdin, os.Stdout,
						func() bool { return !watch.Started() })
				}
				// Install-flow model step (waired-agent#586): the picker,
				// between the speed question and the model wait — the wait
				// below then watches the download the answer started.
				// Skipped while a browser wizard drives (§4.2) and when
				// local AI just went off (asking which model to download
				// right after would be the flow contradicting itself);
				// both skips withdraw the pending-question claim.
				var picked modelPickerOutcome
				if !setupActive {
					if keptOn {
						picked = runInitModelPicker(mgmtURL, nonInteractive, inf.ModelID, stdin, os.Stdout,
							func() bool { return !watch.Started() })
					} else {
						postModelChoicePending(mgmtURL, false)
					}
				}
				if picked.none {
					// #586: nothing is coming, and that is the choice — the
					// picker already printed what it means, and the closing
					// box reads this host as configured, not pending.
				} else {
					// #306: report on the model the WIZARD chose. Deliberately
					// not gated on setupActive — that is a snapshot taken
					// before this wait, and the whole point of the watch above
					// is that a browser setup can commit minutes into it. The
					// target self-gates on setupDriving per read instead.
					modelWait = waitForBundledModel(mgmtURL, os.Stdout, isTerminal(os.Stdout), budget,
						engineComing, enter, watch, newModelTarget(sess))
				}
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
			// #186: the TERMINAL's own questions used to sit ABOVE the
			// engine install, so in terminal mode they interrupted a
			// multi-GB download to ask about coding tools, and a terminal
			// that took over late was never asked at all. Asking here fixes
			// both — setupActive is settled by now, and the download is
			// done. That reasoning is about a prompt someone has to answer,
			// so it still holds; the wizard's own instruction needs nobody
			// and was applied before the download (waired-agent#311).
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
				// The last chance to apply the wizard's instruction. The
				// call before the download is the ordinary one; this catches
				// the two cases it cannot:
				//
				//   - a browser setup that only committed DURING the wait,
				//     so setupActive was false up there (#308);
				//   - a wizard that wrote its engine and model first and its
				//     coding-tool answer a moment later, so the instruction
				//     was not on the wire yet.
				//
				// runWizardIntegrations is a no-op once it has run, so the
				// tools are never written twice.
				if !integrationsRan && !runWizardIntegrations(sess, setupActive, integOpts) {
					// No instruction at all, even now — an older control
					// plane, or a wizard that never asked.
					fmt.Println("You can set up your coding tools later from this terminal with `waired link all`.")
				}
				// The wizard's own claude-code toggle is the consent, and
				// the routing it promises is applied by runSetupIntegrations
				// (setup_integration.go) — not here. §4.2: this terminal
				// must not ask, so the block below stays out of its way.
			} else if skipIntegration || authOnlyRefresh {
				// Nothing was asked here, by flag or because this run only
				// rotated credentials. The hint is for the case where
				// nothing was WRITTEN either: a re-auth that applied the
				// stored instruction above has already reported its row,
				// and pointing that operator at a repair command would
				// describe work that just succeeded.
				if !integrationsRan {
					fmt.Println("Run `waired link <agent>` to (re)configure coding-agent integration if needed.")
				}
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
				// Report the row the wizard's own apply already reports
				// (waired-agent#646/#645). This is the SAME §7 step, done by
				// the other surface, and until now only the browser-driven
				// apply said so — so a terminal-driven init left the
				// coding-tools row with no author. On a device carrying a
				// leftover instruction the daemon then read the executor's
				// exit as "it left before it got to this row" and reported a
				// failure for work that had just succeeded.
				//
				// Only on consent, and only on a clean apply: a declined
				// question wrote nothing, and a half-configured machine is
				// what the wizard's own applier refuses to report as done.
				reportTerminalIntegrations(sess, consented, err)
				integConsent = consented
			}

			var resp *management.BenchmarkRunResponse
			// benchFailed is "the benchmark ran and the engine could not
			// complete a generation", never "no benchmark happened" — see
			// waitForBenchmark's ranAndFailed.
			var benchFailed bool
			switch benchmarkPlanFor(setupActive, engineErr, modelWait) {
			case benchSkipSetupDriving:
				// waired#939: the degraded wording. Everything this process
				// owed the setup is done and init is about to return, so the
				// keep-open instruction no longer applies — saying it here
				// would leave the operator guarding a terminal for nothing,
				// and the wizard would be telling them the opposite.
				//
				// Skipped when it was already said before the download, which
				// is the ordinary path now that the coding tools run there
				// (waired-agent#311). This branch is for the run that only
				// finished its share down here: a browser setup that
				// committed during the wait, or an instruction that arrived
				// late.
				if !terminalDoneSaid {
					fmt.Println(setupTerminalDoneLine)
				}
			case benchSkipNoEngine, benchSkipEngineDown, benchSkipModelNotReady:
				// Nothing to measure, and the wait above already said why.
			case benchRun:
				// #133: once the daemon has the model ready, benchmark it and
				// offer a lighter model if this host can't sustain the pick.
				resp, benchFailed, _ = benchmarkWithScanner(mgmtURL, nonInteractive, os.Stdout, stdin, isTerminal(os.Stdout))
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
			if !setupActive {
				switch planClaudeRoute(claudeRouteFacts{
					integConsent:    integConsent,
					elevated:        isElevatedFn(),
					managedPath:     claudemanaged.Path(),
					skipClaudeRoute: o.SkipClaudeRoute,
					nonInteractive:  nonInteractive,
				}) {
				case claudeRouteAsk:
					// The verdict these two report is not captured: the closing
					// card reads managed settings instead (waired-agent#796), so
					// a write that did not land cannot be reported as one that
					// did.
					promptClaudeRouting(os.Stdout, stdin, o.StateDir)
				case claudeRouteApply:
					routeClaudeNow(claudeRouteApplyOpts{
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

			// waired-agent#796, second half: the wizard applies the Claude Code
			// route before the model download (waired-agent#311), so the window
			// was unresolvable then and the key was left out. It resolves now.
			topUpClaudeWindow(o.StateDir)

			// #756: the daemon chose the inference role from this host's
			// hardware without an interactive prompt, so tell the user how to
			// inspect and change it afterward.
			printInferenceRoleGuidance(os.Stdout)
			summary := daemonSummary{
				accountEmail:  st.AccountEmail,
				engineErr:     engineErr,
				engineFailure: modelWait.engineFailure,
				modelPending:  modelWait.pending,
				noModelChosen: modelWait.noModelChosen,
				benchFailed:   benchFailed,
				bench:         outcomeFrom(resp),
				// waired-agent#796: the card reports the state of the machine,
				// read from managed settings — not what this run happened to
				// do. routedHere is only ever assigned inside `if !setupActive`,
				// so a browser-wizard install (the path every real install
				// takes) always closed by claiming it had left Claude Code on
				// the Anthropic API over a machine it had just routed. Reading
				// the file is also what makes this card and `waired claude
				// status` structurally unable to disagree, which was the
				// symptom, and it drops the card's dependency on setupActive —
				// a flag that means "the wizard is driving" until a takeover
				// makes it mean "it was".
				claudeRouted: claudeCardRouted(o.StateDir),
				hostSpeed:    fetchHostSpeed(o.MgmtURL),
			}
			printDaemonSummaryBox(os.Stdout, summary)
			// Sign-in succeeded, so this is never a failed init: #188's rule
			// stands, and errLocalAIDown is not an error in the "waired:
			// something went wrong" sense — main.go gives it its own exit
			// code and prints nothing, because the box above already said it
			// in the words a person reads.
			//
			// What it buys is the one thing an installer could not otherwise
			// learn: exit 0 meant "signed in", full stop, so install.sh had
			// to choose between calling a successful sign-in a failure and
			// printing 🎉 over a device with no local AI. It printed the 🎉
			// (#310).
			return summary.exitErr()
		case management.LoginPhaseError:
			if st.Error != "" {
				// Classified here too, not only on the /login/start
				// response above. The daemon runs enrollment on its own
				// goroutine and reports the failure as TEXT in
				// LoginStatus.Error (cmd/waired-agent/login.go's fail),
				// so an old control plane rejecting the auth_key field
				// surfaces on THIS path, wearing controlclient's
				// "create login session: status 400: …" rather than the
				// daemon-path prefix.
				//
				// That is the case an auth key exists for — an
				// unattended fresh install — and it was reaching
				// operators as a raw JSON decoder error, which reads as
				// "your key is malformed" and sends them off to
				// regenerate a key that was never wrong (#728).
				return classifyAuthKeyError(fmt.Errorf("login failed: %s", st.Error), authKey != "")
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
	writePromptf(out, "%s %s\n", emo("⚠️", "!"), bold("The inference engine could not be installed on this device."))
	writePromptf(out, "  %v\n", err)
	writePrompt(out)
	writePrompt(out, "  Sign-in worked — this device is signed in and running. Only local inference is missing.")
	writePromptf(out, "  Retry the install with: %s\n", cyan(elevation.Hint("waired init")))
	if setupActive {
		writePrompt(out, "  "+dim("The setup page in your browser shows this same failure."))
	}
	writePrompt(out)
}

// benchSkip is whether `waired init` measures this host, and when it does
// not, which of the four reasons applies.
//
// A named decision rather than a switch inline in the login flow, for the
// reason exitErr gives about itself: it is one rule that several things
// have to agree on, and the only way to test it is to be able to call it.
type benchSkip int

const (
	// benchRun: the engine is installed, it stayed up, and the model wait
	// reached ready — the one state in which there is something to measure.
	benchRun benchSkip = iota
	// benchSkipSetupDriving: §4.2, the browser owns the questions. The
	// benchmark prompt reads stdin and can offer to switch the active
	// model, which is a second writer racing desired_model_id.
	benchSkipSetupDriving
	// benchSkipNoEngine: nothing to measure, because there is no engine
	// (#188). Includes the opt-out, where there was never going to be one.
	benchSkipNoEngine
	// benchSkipEngineDown: the engine is installed and will not stay up,
	// so benchmarking would only add its own refusal to a failure the
	// wait has already explained in full (#310).
	benchSkipEngineDown
	// benchSkipModelNotReady: the model wait ended without a ready model
	// and SAID so, handing the terminal back — "it will finish in the
	// background. Run `waired status` to watch progress".
	//
	// Until #569 this state fell through to the benchmark, and
	// waitForBenchmark re-ran the whole readiness wait on a fresh
	// benchPollDeadline of its own: up to ten more minutes on the very
	// download init had just handed to the background, ending on
	// whatever that second wait happened to conclude. On one nightly leg
	// that was "everything completed successfully! / Local inference is
	// live via the waired-agent daemon", printed over a host whose model
	// did not finish arriving until the following init.
	benchSkipModelNotReady
)

// benchmarkPlanFor is that rule. Order is a contract, and it is the same
// order printDaemonSummaryBox uses, because the two describe one run: the
// more specific reason wins, and a reason that made the next one
// unreachable comes first.
func benchmarkPlanFor(setupActive bool, engineErr error, w modelWaitResult) benchSkip {
	switch {
	case setupActive:
		return benchSkipSetupDriving
	case engineErr != nil:
		return benchSkipNoEngine
	case w.engineFailure != "":
		return benchSkipEngineDown
	case !w.ready:
		return benchSkipModelNotReady
	}
	return benchRun
}

// daemonSummary is everything the closing box is chosen from.
type daemonSummary struct {
	accountEmail string
	// engineErr is the engine INSTALL failing (#188).
	engineErr error
	// engineFailure is the installed engine refusing to stay up (#310),
	// as reported by the model wait. Empty on every other outcome,
	// including the ones where the wait ended not-ready for a reason
	// that is not a fault.
	engineFailure string
	// benchFailed is the benchmark RUNNING and the engine failing to
	// complete a generation (#29), which is a third outcome and not a
	// weaker form of either field above: the engine installed, it stayed
	// up, and it still cannot serve. Never set for a benchmark that was
	// skipped — a routing-only node and an external endpoint both skip it
	// by design, and #203 is explicit that a skip must not read as a
	// fault (waired-ai/waired-agent#552).
	benchFailed bool
	// modelPending is the model wait ending with local AI still on its
	// way — still downloading, engine still coming up, terminal handed
	// back (modelWaitResult.pending, #569). Nothing failed, so it never
	// reaches exitErr; what it changes is the box, because the success
	// box's "Local inference is live via the waired-agent daemon" is a
	// claim this host cannot support yet.
	//
	// Deliberately not "the wait returned not-ready": that is also the
	// honest answer on a gateway-only host, which must keep the success
	// box. The wait sets this only where it knows better.
	modelPending bool
	// noModelChosen is the model wait ending because this host never had a
	// model to wait for (modelWaitResult.noModelChosen, waired-agent#736).
	// Like modelPending it changes only the box, and for the same reason:
	// "Local inference is live" is a claim a host with nothing to serve
	// cannot support either.
	noModelChosen bool
	bench         benchmarkOutcome
	claudeRouted  bool
	// hostSpeed is what one coding question cost on this machine, as the
	// daemon measured it during this install (waired-ai/waired-agent#496,
	// reported here per waired#1099). nil when nothing was measured — an
	// engine that never came up, a host whose probe model would not
	// download — and the summary simply does not mention speed.
	//
	// It carries `TurnedInferenceOff`, which is the one case the ordinary
	// success box gets WRONG: that box says "Local inference is live via
	// the waired-agent daemon", and on a host the measurement cut it is
	// not live and was never going to be.
	hostSpeed *management.HostSpeedStatus
}

// exitErr is what `waired init` returns for this outcome: errLocalAIDown
// when sign-in worked but this device has no local AI — the engine could
// not be installed (#188), or it installed and would not stay up (#310).
//
// One predicate for both, because outside this file they are one fact.
// The two boxes differ only in which command to run next; an installer,
// an exit code and an operator all care about the same thing.
//
// Deliberately NOT "the model wait returned not-ready": that is also the
// honest answer on a gateway-only host with inference disabled, on a
// takeover, and on a budget that simply elapsed. Only a STATED fault
// counts, which is the same rule the box above is chosen by — and the
// reason they are derived from one struct rather than decided twice.
//
// modelPending is that rule's newest case and does not appear below: a
// download that has not finished is not a device with no local AI, it is
// a device a few minutes from having some, and an installer that exited 3
// on it would report a failed install to everyone on a slow link (#569).
//
// And an engineErr is not automatically a fault: engine installs turned
// off on this host is an instruction the operator gave, so it exits 0
// (#551). See engineOptOut.
func (s daemonSummary) exitErr() error {
	// benchFailed joins the other two rather than being a warning with a
	// zero exit: to an installer "local AI is down" is a statement about
	// whether the thing can answer a request, and an engine that runs but
	// cannot complete one answers no. Reachable only from a STATED
	// failure, never from a skipped benchmark, so a gateway-only host and
	// an external endpoint keep exiting 0 (#310's rule, #552's case).
	if s.engineOptOut() {
		return nil
	}
	if s.engineErr != nil || s.engineFailure != "" || s.benchFailed {
		return errLocalAIDown
	}
	return nil
}

// engineOptOut reports whether the only reason this host has no local AI
// is the one its operator configured (#551).
//
// Same rule for the box and for the exit code, in one place, for the
// reason exitErr gives above: an installer branching on 3 and a person
// reading the box must not be told different things about one run.
//
// The two engine faults are checked as well — not because either can
// happen alongside an opt-out today (no engine was installed, so nothing
// can be down and nothing can be benchmarked), but so that this cannot
// become the arm that swallows a real fault if one ever does.
func (s daemonSummary) engineOptOut() bool {
	return errors.Is(s.engineErr, errEngineOptOut) &&
		s.engineFailure == "" && !s.benchFailed
}

// printDaemonSummaryBox renders the one box `waired init` ends on.
//
// Split out from the caller because this is a decision, not a print: it
// used to be `if engineErr != nil`, which asks only whether the engine
// could be INSTALLED. An engine that installed and then died left the
// condition false, so a device whose local AI never came up ended its
// setup on "everything completed successfully!" (#310).
//
// Order is a contract. The install failing is the more specific answer —
// there is no engine to be down — and its box names the command that
// finishes the install, which would be the wrong instruction on a host
// where the files are already there. The benchmark arm comes last of the
// three for the same reason: it is the only one that needed an engine
// that installed AND stayed up in order to be reached at all.
//
// The opt-out arm sits above the three faults because it is not one of
// them: it is the same engineErr the install arm reads, refined by asking
// whether the operator asked for this (#551). engineOptOut clears the
// other two itself, so a real fault still outranks it.
//
// It also sits above the measurement's own box (waired#1099), and that
// order is the load-bearing one, because those two are the only endings
// that are not faults and a reader could mistake either for the other.
// The remedies decide it: that box says `waired inference on`, which on
// a host that will not install an engine flips a toggle and leaves the
// operator with no local AI and no idea why. Naming the opt-out is the
// only one of the two that is actionable here.
//
// The still-setting-up arm (#569) comes last of all, immediately above the
// success box, because it is the weakest claim on the page: it says only
// that local AI has not arrived YET, and every arm above it knows
// something more specific about why it never will. It sits below the
// measurement's box in particular for the reason that ordering always
// turns on — the remedies. That box says `waired inference on`; telling
// an operator to wait for a download the daemon abandoned would be advice
// about nothing. They cannot collide today (a host the measurement
// switched off answers `disabled`, which the wait does not call pending),
// so the order is what keeps that true rather than what depends on it.
// claudeSummaryLine renders the closing card's `Claude` row.
//
// One function because the same two strings sat in six boxes, so
// waired-agent#796 had to be checked in all six by reading — and reading is what
// missed that the row's input was a variable only ever assigned on one of the
// two paths through init. The wording is unchanged; #796 is a fix to what the
// row is told, not to what it says.
func claudeSummaryLine(routed bool) string {
	if routed {
		return fmt.Sprintf("%-9s %s", "Claude", green("routed through Waired"))
	}
	return fmt.Sprintf("%-9s %s", "Claude", dim("still using the Anthropic API"))
}

func printDaemonSummaryBox(out io.Writer, s daemonSummary) {
	switch {
	case s.engineOptOut():
		printDaemonEngineOptOutBox(out, s.accountEmail, s.claudeRouted)
	case s.engineErr != nil:
		printDaemonEngineFailedBox(out, s.accountEmail)
	case s.engineFailure != "":
		printDaemonEngineDownBox(out, s.accountEmail)
	case s.benchFailed:
		printDaemonBenchmarkFailedBox(out, s.accountEmail, s.claudeRouted)
	case s.hostSpeed != nil && s.hostSpeed.TurnedInferenceOff:
		printDaemonTooSlowBox(out, s)
	case s.modelPending:
		printDaemonSettingUpBox(out, s.accountEmail, s.claudeRouted)
	case s.noModelChosen:
		printDaemonNoModelBox(out, s.accountEmail, s.claudeRouted, s.hostSpeed)
	default:
		printDaemonSuccessBox(out, s.accountEmail, s.bench, s.claudeRouted, s.hostSpeed)
	}
}

// printDaemonSettingUpBox is the summary for a run that signed the device
// in and left local AI still on its way: the model was still downloading
// when init's window closed, the engine had not finished coming up, or
// the operator took the terminal back and the agent carried on (#569).
//
// box, not boxWarn, and it names no repair command: nothing is broken and
// there is nothing for the operator to do. It exists because the success
// box ends on "Local inference is live via the waired-agent daemon", and
// on this host that sentence is not true yet — the same defect #310 and
// #552 each fixed one reason earlier, reached here through a download
// rather than through a fault.
//
// The reason is deliberately not repeated. All three endings that set
// modelPending print their own account immediately before this box, and
// they are the ones that know which of the three it was.
func printDaemonSettingUpBox(out io.Writer, accountEmail string, claudeRouted bool) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	lines = append(lines, claudeSummaryLine(claudeRouted))
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("Waired is still setting local inference up in the background; the line above says what it's waiting on."))
	lines = append(lines, dim("Watch it with: waired status"))
	box(out, emo("✅", "*"), "Waired is signed in — local inference is still setting up here", lines)
}

// printDaemonTooSlowBox is the summary for a host the install-time
// measurement judged too slow to run local AI usefully
// (waired-ai/waired-agent#496), so the daemon left it off.
//
// A box of its own, ahead of the success box, because the success box
// makes two claims that are false here: "everything completed
// successfully" and "Local inference is live via the waired-agent
// daemon". Nothing failed — the sign-in worked, the device is on the
// network, and it can use the models on the operator's other computers —
// but the one thing this box exists to prevent is someone walking away
// believing local AI is running when the machine decided it should not.
//
// NOT an error box, and it does not change the exit code: #465's off is
// a DEFAULT with a working opt-in, not a refusal (waired-ai/waired#1056),
// and an installer must not read it as a failed install.
//
// The wording is the same claim `waired inference status` makes, because
// they are the same fact — see hostSpeedTurnLine.
func printDaemonTooSlowBox(out io.Writer, s daemonSummary) {
	var lines []string
	if s.accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", s.accountEmail))
	}
	lines = append(lines, fmt.Sprintf("%-9s %s", "Speed", dim(hostSpeedTurnLine(s.hostSpeed))))
	lines = append(lines, claudeSummaryLine(s.claudeRouted))
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("Local inference starts off here; it can still use your other computers' models."))
	lines = append(lines, dim("Turn it on anyway with `waired inference on`."))
	box(out, emo("🎉", "*"), "Waired is ready — local inference starts off on this computer", lines)
}

// printDaemonBenchmarkFailedBox is the summary for a run whose engine
// installed, started, took the model — and then could not complete a
// single generation (#29).
//
// It is a third box rather than a suppressed line in the success box.
// This run really did end with "[!] Local inference could not complete a
// test generation: HTTP 500" followed by "Waired is ready — everything
// completed successfully!" and "Local inference is live via the
// waired-agent daemon", three lines apart, which is the same defect #188
// and #310 each fixed one layer earlier
// (waired-ai/waired-agent#552, run 31164150206).
//
// The engine's reason is not repeated: waitForBenchmark printed it in
// full moments ago, along with where to look next.
func printDaemonBenchmarkFailedBox(out io.Writer, accountEmail string, claudeRouted bool) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	lines = append(lines, claudeSummaryLine(claudeRouted))
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("The inference engine here could not answer a test request; the reason is above."))
	boxWarn(out, emo("⚠️", "!"), "Waired is signed in — local inference is not answering yet", lines)
}

// errLocalAIDown makes the outcome above scriptable. main.go maps it to
// exitLocalAIDown and prints nothing for it.
//
// A sentinel rather than a message: nothing reads this text — the boxes
// are the user-facing account, and an installer branches on the number.
var errLocalAIDown = errors.New("signed in, but local inference is not running on this device")

// printDaemonEngineFailedBox is the summary for a run that signed the
// device in but could not install the engine. Deliberately not the
// success box: "everything completed successfully" over a failed engine
// install is how an operator ends up not realising local AI never
// arrived (#188).
func printDaemonEngineFailedBox(out io.Writer, accountEmail string) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("The inference engine is not installed yet; the command above finishes it."))
	boxWarn(out, emo("⚠️", "!"), "Waired is signed in — the inference engine still needs installing", lines)
}

// printDaemonEngineOptOutBox is the summary for a run that signed the
// device in on a host where engine installs are turned off (#551).
//
// box, not boxWarn, and no "could not" anywhere in it: everything the
// operator asked this run to do, it did. The three warn boxes above all
// describe something that went wrong and name the command that repairs
// it; this one describes a host that is finished, and names the command
// that would ADD local AI to it if the operator ever wants it.
//
// Not the success box either. That one ends on "Local inference is live
// via the waired-agent daemon", which is exactly the sentence this host
// cannot support — the #310 shape, one reason along.
//
// And deliberately NOT worded like printDaemonTooSlowBox, which is the
// other "local AI is off and nothing failed" ending (waired#1099). Those
// two are one glance apart and their REMEDIES are opposites: that box
// says `waired inference on`, which on a host that will not install an
// engine turns a toggle and produces no local AI at all. So this title
// names the cause rather than the symptom, and the ordering above puts
// it first.
func printDaemonEngineOptOutBox(out io.Writer, accountEmail string, claudeRouted bool) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	lines = append(lines, claudeSummaryLine(claudeRouted))
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("No local inference here; it can still use your other computers' models."))
	lines = append(lines, dim("Add local inference later with: waired runtimes install ollama"))
	box(out, emo("✅", "*"), "Waired is signed in — engine installs are turned off here", lines)
}

// printDaemonEngineDownBox is the summary for a run that signed the
// device in over an engine that IS installed and would not stay up
// (#310) — the case #188's box cannot describe, because nothing needs
// installing and the command it points at would find its work done.
//
// The engine's own reason is deliberately not repeated here: the wait
// printed it in full moments ago, and it is routinely a multi-line
// stderr tail that a single-line box frame cannot hold.
func printDaemonEngineDownBox(out io.Writer, accountEmail string) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	lines = append(lines, dim("The inference engine on this device isn't starting; `waired doctor` says why."))
	boxWarn(out, emo("⚠️", "!"), "Waired is signed in — local inference isn't running", lines)
}

// printDaemonNoModelBox is the summary for a host that finished setup with
// no model chosen for it (waired-agent#736).
//
// It exists because the success box below states "Local inference is live
// via the waired-agent daemon" unconditionally, and on this host it is
// not: the engine is up and has nothing to serve. Before #736 this ending
// was reported as pending and got the still-setting-up box, which was
// wrong the other way — it promised work in the background that no one
// had started.
//
// box, not boxWarn, and the same shape as the engine-opt-out box above:
// nothing failed here. A host can be signed in, on the network, and
// deliberately without a local model.
//
// The Speed line rides along when the daemon measured one. That figure is
// the host-cutoff probe's, not a chosen model's, so it survives having no
// model — and it is the one number that says whether picking a model here
// is worth doing.
func printDaemonNoModelBox(out io.Writer, accountEmail string, claudeRouted bool, hostSpeed *management.HostSpeedStatus) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	if hostSpeedTurnLine(hostSpeed) != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Speed", green(hostSpeedTurnLine(hostSpeed))))
	}
	lines = append(lines, claudeSummaryLine(claudeRouted))
	lines = append(lines, dim("Signed in and running — this device is on your network."))
	// The route back is not repeated: the wait printed it immediately
	// above, the same way the still-setting-up box leaves its reason to
	// the line that precedes it.
	lines = append(lines, dim("No model is set up here, so local inference has nothing to answer with yet."))
	box(out, emo("✅", "*"), "Waired is signed in — no model chosen for this computer", lines)
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
func printDaemonSuccessBox(out io.Writer, accountEmail string, bench benchmarkOutcome, claudeRouted bool, hostSpeed *management.HostSpeedStatus) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	// Two different measurements, and they are not interchangeable.
	// `Model` is the chosen model's decode rate on this host; `Speed` is
	// what one whole coding question costs, measured on a small stand-in
	// before the model was downloaded. The second is the one an operator
	// can compare against another computer, and it is the only one a host
	// that declined the benchmark has.
	// Asked as "is there a figure" rather than of one field: a host judged
	// from the prefill bound alone leaves TurnSeconds at zero and fills
	// TurnFloorSeconds (waired-agent#579).
	if hostSpeedTurnLine(hostSpeed) != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Speed", green(hostSpeedTurnLine(hostSpeed))))
	}
	if bench.Measured {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Model", green(fmt.Sprintf("%.0f tok/s", bench.Tokps))))
	}
	lines = append(lines, claudeSummaryLine(claudeRouted))
	lines = append(lines, dim("Local inference is live via the waired-agent daemon."))
	lines = append(lines, dim("Point your coding agent at Waired and start building."))
	box(out, emo("🎉", "*"), "Waired is ready — everything completed successfully!", lines)
}
