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

// runInitViaDaemon drives the daemon-owned login MGMT API instead of
// enrolling locally (the Tailscale model). It POSTs /login/start, opens
// the browser on the first login URL, then polls /login/status until the
// daemon reaches a terminal phase. The running daemon owns the runtime
// and the state dir, so the CLI does no deploy here; the per-user
// coding-agent integration consent runs once login is active (it lands
// in the user's home, which the daemon never touches).
func runInitViaDaemon(mgmtURL, control, deviceName string, noBrowser, nonInteractive, skipIntegration bool, gatewayBaseURL string, owner *stdinReader, inf daemonInitInference) error {
	reqBody, _ := json.Marshal(management.LoginStartRequest{
		ControlURL: control,
		DeviceName: deviceName,
	})
	out, err := httpPost(mgmtURL+"/waired/v1/login/start", reqBody)
	if err != nil {
		return fmt.Errorf("start login via daemon: %w", err)
	}
	var st management.LoginStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return fmt.Errorf("decode login start: %w", err)
	}
	if st.SessionID == "" {
		return errors.New("daemon did not return a login session id")
	}

	fmt.Println(bold("Sign in"))

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
	gate := resolveBrowserGate(noBrowser, nonInteractive, isTerminal(os.Stdin), browser.HasDisplay())

	opened := false
	lastPhase := management.LoginPhase("")
	deadline := time.Now().Add(12 * time.Minute)

	for {
		if st.LoginURL != "" && !opened {
			opened = true
			// gcloud-style gate: URL first, browser only on Enter (or
			// immediately when the session can't answer a prompt). See
			// login_gate.go.
			presentLoginURL(stdin, os.Stdout, st.LoginURL, st.UserCode, gate)
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

			budget, setupActive, enter := awaitBrowserSetup(sess, owner, os.Stdout, nonInteractive, noBrowser)

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
					engineArrivalPending(sess.State()), enter)
			}
			if enter.Fired() {
				// The operator took the terminal back: stop being the
				// executor (the wizard switches to "run this here") and
				// resume the normal CLI tail.
				sess.Release()
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
			if setupActive {
				fmt.Println("You can set up your coding tools later from this terminal with `waired link all`.")
			} else if skipIntegration {
				fmt.Println("Run `waired link <agent>` to (re)configure coding-agent integration if needed.")
			} else if err := runPostLoginIntegration(postLoginIntegrationOpts{
				StepLabel:      emo("🔌", "*"),
				GatewayBaseURL: gatewayBaseURL,
				NonInteractive: nonInteractive,
				In:             stdin,
				Out:            os.Stdout,
				ErrOut:         os.Stderr,
			}); err != nil {
				// Warn-only: login already succeeded; a broken integration
				// must not turn it into a failed init.
				fmt.Fprintf(os.Stderr,
					"warn: coding-agent integration had problems (%v); re-run later: waired link --force all\n", err)
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
			// #756: the daemon chose the inference role from this host's
			// hardware without an interactive prompt, so tell the user how to
			// inspect and change it afterward.
			printInferenceRoleGuidance(os.Stdout)
			if engineErr != nil {
				printDaemonEngineFailedBox(st.AccountEmail)
			} else {
				printDaemonSuccessBox(st.AccountEmail, outcomeFrom(resp))
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
		// #184: on the print-only gate nothing above reads stdin, so an
		// Enter pressed out of muscle memory — the other two gates open a
		// browser with it — used to sit in the buffer until the takeover
		// offer answered it, silently switching setup to the terminal at
		// the moment the user was asking for a browser. Answer it here,
		// where it was pressed. Non-blocking on purpose: the link may
		// well be opened on a phone, and a terminal parked on a read
		// would never see the sign-in complete.
		if opened && gate == gatePrintOnly {
			if _, typed := owner.Poll(); typed {
				fmt.Println(dim("Nothing to press here — waiting for you to sign in with the link above."))
			}
		}
		time.Sleep(time.Second)

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
// account and (when the benchmark ran) the measured throughput — the box
// otherwise matches the standalone printInitSuccessBox.
func printDaemonSuccessBox(accountEmail string, bench benchmarkOutcome) {
	var lines []string
	if accountEmail != "" {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Account", accountEmail))
	}
	if bench.Measured {
		lines = append(lines, fmt.Sprintf("%-9s %s", "Model", green(fmt.Sprintf("%.0f tok/s", bench.Tokps))))
	}
	lines = append(lines, dim("Local inference is live via the waired-agent daemon."))
	lines = append(lines, dim("Point your coding agent at Waired and start building."))
	box(os.Stdout, emo("🎉", "*"), "Waired is ready — everything completed successfully!", lines)
}
