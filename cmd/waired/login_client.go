package main

import (
	"bufio"
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
func runInitViaDaemon(mgmtURL, control, deviceName string, noBrowser, nonInteractive, skipIntegration bool, gatewayBaseURL string) error {
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
	opened := false
	lastPhase := management.LoginPhase("")
	deadline := time.Now().Add(12 * time.Minute)

	for {
		if st.LoginURL != "" && !opened {
			opened = true
			// gcloud-style gate: URL first, browser only on Enter (or
			// immediately when the session can't answer a prompt). See
			// login_gate.go.
			presentLoginURL(os.Stdin, os.Stdout, st.LoginURL, st.UserCode,
				resolveBrowserGate(noBrowser, nonInteractive, isTerminal(os.Stdin), browser.HasDisplay()))
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
			if skipIntegration {
				fmt.Println("Run `waired link <agent>` to (re)configure coding-agent integration if needed.")
			} else if err := runPostLoginIntegration(postLoginIntegrationOpts{
				StepLabel:      emo("🔌", "*"),
				GatewayBaseURL: gatewayBaseURL,
				NonInteractive: nonInteractive,
				In:             os.Stdin,
				Out:            os.Stdout,
				ErrOut:         os.Stderr,
			}); err != nil {
				// Warn-only: login already succeeded; a broken integration
				// must not turn it into a failed init.
				fmt.Fprintf(os.Stderr,
					"warn: coding-agent integration had problems (%v); re-run later: waired link --force all\n", err)
			}
			// #756: the daemon pulls the bundled model in the background
			// after enroll, so the daemon-mediated init used to return while a
			// multi-GB download ran invisibly. Block in the foreground with the
			// same percentage progress bar the local path shows (main.go), then
			// benchmark the ready model. waitForBundledModel returns fast when
			// the daemon reports inference disabled / stopped / no engine, so
			// this never hangs an under-spec or gateway-only host.
			waitForBundledModel(mgmtURL, os.Stdout, isTerminal(os.Stdout))
			// #133: once the daemon has the model ready, benchmark it and
			// offer a lighter model if this host can't sustain the pick.
			resp, _ := benchmarkWithScanner(mgmtURL, nonInteractive, os.Stdout, bufio.NewScanner(os.Stdin), isTerminal(os.Stdout))
			// #756: the daemon chose the inference role from this host's
			// hardware without an interactive prompt, so tell the user how to
			// inspect and change it afterward.
			printInferenceRoleGuidance(os.Stdout)
			printDaemonSuccessBox(st.AccountEmail, outcomeFrom(resp))
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
