package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// benchPollDeadline bounds how long `waired init` waits for the model to
// finish downloading + the engine to come up before it gives up on the
// interactive-performance check. Generous because a cold first pull of a
// multi-GB model over a slow link can take many minutes; the user can
// always re-run `waired runtimes benchmark` later. A var (not const) so
// tests can shrink it.
var benchPollDeadline = 10 * time.Minute

// benchPollInterval is the gap between status / benchmark polls. A var so
// tests can shrink it to stay fast.
var benchPollInterval = 3 * time.Second

// benchNoEngineGrace bounds how long we tolerate the transient `no_engine`
// state before treating it as terminal. On a fresh bundled install the
// engine binary is still being installed / the child still being brought
// up at the first polls, so /status briefly reports `no_engine` even though
// the engine is on its way (issue #489). We keep polling through that
// window; only if `no_engine` outlives the grace do we conclude the engine
// will never come up (e.g. the bundled runtime is genuinely missing) and
// skip. A cold engine bring-up (binary extract + StartupReadyTimeout) fits
// comfortably inside this; a var so tests can shrink it.
var benchNoEngineGrace = 3 * time.Minute

// benchHTTP is the status-aware client used by the benchmark prompt.
// The /benchmark POST blocks while the daemon warms the model up (up
// to 180 s for a cold multi-GB load) plus the 30 s measurement, so the
// client timeout must comfortably exceed both.
var benchHTTP = &http.Client{Timeout: 240 * time.Second}

// promptBenchmarkRecommendation runs the issue #133 post-install
// interactive-performance check: it asks the daemon to benchmark the
// active model and, when throughput is below the interactive floor and a
// lighter model fits, prompts the user to switch. It NEVER switches
// without confirmation; --non-interactive prints the recommendation but
// does not auto-accept.
//
// It is best-effort: any transport / not-configured / timeout condition
// prints an informational line (or nothing) and returns nil so it never
// blocks `waired init` from succeeding.
func promptBenchmarkRecommendation(mgmtURL string, nonInteractive bool, out io.Writer, in io.Reader, tty bool) error {
	_, err := benchmarkWithScanner(mgmtURL, nonInteractive, out, bufio.NewScanner(in), tty)
	return err
}

// benchmarkOutcome carries the just-measured throughput up to the final
// success summary. The zero value means "no measurement" — the benchmark was
// skipped, the daemon was unreachable, or an older daemon didn't report tok/s.
type benchmarkOutcome struct {
	Measured bool
	Tokps    float64
}

// outcomeFrom reduces a benchmark response to the summary-facing measurement.
func outcomeFrom(resp *management.BenchmarkRunResponse) benchmarkOutcome {
	if resp == nil || resp.MeasuredTokps <= 0 {
		return benchmarkOutcome{}
	}
	return benchmarkOutcome{Measured: true, Tokps: resp.MeasuredTokps}
}

// benchmarkWithScanner is the body of promptBenchmarkRecommendation,
// taking an already-constructed scanner so a caller that already prompted
// on the same stdin (offerBenchmark's "run benchmark now?" gate) can share
// one scanner instead of layering two bufio readers over os.Stdin. tty
// selects the in-place progress rendering of the post-accept download wait.
// It returns the raw benchmark response (nil when no measurement could be
// obtained) so the caller can surface the throughput in the final success
// summary; the error is always nil today (every give-up path is
// best-effort) but kept for future use.
func benchmarkWithScanner(mgmtURL string, nonInteractive bool, out io.Writer, sc *bufio.Scanner, tty bool) (*management.BenchmarkRunResponse, error) {
	resp, ok := waitForBenchmark(mgmtURL, out)
	if !ok {
		return nil, nil // already explained inside waitForBenchmark
	}

	if rec := resp.Recommendation; rec != nil && !rec.Dismissed {
		// Special case: the only lighter step-down is the tiny, below-floor
		// 0.5B. There's nothing lighter to fall back to, so instead of the
		// neutral "switch to a lighter model" flow, confirm whether to keep
		// local inference at all (drop to the 0.5B) or turn it off. Default No.
		if isBundledModelBelowFloor(rec.ToModelID) {
			return tinyBenchmarkDisableFlow(mgmtURL, nonInteractive, out, sc, tty, rec, resp)
		}

		// Below the interactive floor → lighter-model flow (issue #133).
		from := modelWithQuality(rec.FromModelID, rec.FromVariantID)
		to := modelWithQuality(rec.ToModelID, rec.ToVariantID)
		writePromptf(out, "\n%s Local inference is slow: %s measured %.0f tok/s, below the %.0f tok/s interactive floor.\n",
			emo("🐢", "!"), from, rec.MeasuredTokps, rec.FloorTokps)
		writePromptf(out, "Recommend switching %s → %s; the lighter model should run more smoothly on this hardware.\n",
			from, to)

		if nonInteractive {
			writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to switch interactively.\n",
				from)
			return resp, nil
		}

		// Default Yes: stepping down is cheap and the host is struggling.
		if !ynPrompt(out, sc, fmt.Sprintf("Switch to %s?", to), true) {
			if err := dismissRecommendation(mgmtURL, rec.FromVariantID, rec.ToVariantID); err != nil {
				writePromptf(out, "warn: could not record your choice: %v\n", err)
			} else {
				writePromptf(out, "Keeping %s. You can switch later from the tray or `waired runtimes benchmark`.\n",
					from)
			}
			return resp, nil
		}
		switchAndWait(mgmtURL, rec.ToModelID, to, out, sc, tty)
		return resp, nil
	}

	// At or above the floor: a 200 means the daemon ran a real
	// generation — this doubles as the end-to-end "local inference
	// works" smoke test. The response doesn't carry the benchmarked
	// model's identity, so name it from /inference/status (waired#773);
	// fall back to the model-less wording when that can't be resolved.
	if resp.MeasuredTokps > 0 {
		if modelID, variantID := activeModelForDisplay(mgmtURL); modelID != "" {
			writePromptf(out, "%s Local inference works — %s measured %.0f tok/s on this host.\n",
				emo("✅", "[ok]"), modelWithQuality(modelID, variantID), resp.MeasuredTokps)
		} else {
			writePromptf(out, "%s Local inference works — measured %.0f tok/s on this host.\n",
				emo("✅", "[ok]"), resp.MeasuredTokps)
		}
	} else {
		// Older daemon without measured_tokps on the wire.
		writePrompt(out, emo("✅", "[ok]")+" Local inference works — interactive performance looks good on this host.")
	}

	if rec := resp.Upgrade; rec != nil && !rec.Dismissed {
		from := modelWithQuality(rec.FromModelID, rec.FromVariantID)
		to := modelWithQuality(rec.ToModelID, rec.ToVariantID)
		writePromptf(out, "\n%s This host has headroom: %s is predicted to run at ~%.0f tok/s here (vs %.0f tok/s measured on %s).\n",
			emo("⬆", "^"), to, rec.PredictedTokps, rec.MeasuredTokps, from)

		if nonInteractive {
			writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to switch interactively.\n",
				from)
			return resp, nil
		}

		// Default No: an upgrade pulls a multi-GB download — the opposite
		// trade-off of the lighter flow. The switch itself applies live
		// (waired#812), so only the download is called out here.
		if !ynPrompt(out, sc, fmt.Sprintf("Switch to %s? (downloads the model)", to), false) {
			if err := dismissRecommendation(mgmtURL, rec.FromVariantID, rec.ToVariantID); err != nil {
				writePromptf(out, "warn: could not record your choice: %v\n", err)
			} else {
				writePromptf(out, "Keeping %s. You can switch later from the tray or `waired runtimes benchmark`.\n",
					from)
			}
			return resp, nil
		}
		switchAndWait(mgmtURL, rec.ToModelID, to, out, sc, tty)
	}
	return resp, nil
}

// switchAndWait accepts the recommendation and, when the target model still
// needs a download, foreground-waits for it with progress — the machine
// should be usable when the flow returns (waired#774). A pending Enter
// backgrounds the wait; the agent owns the pull either way.
func switchAndWait(mgmtURL, modelID, label string, out io.Writer, sc *bufio.Scanner, tty bool) {
	pmr, err := acceptRecommendation(mgmtURL, modelID)
	if err != nil {
		writePromptf(out, "warn: could not switch model: %v\n", err)
		return
	}
	if !pmr.Downloading {
		writePromptf(out, "Switching to %s (already downloaded).\n", label)
		return
	}
	writePromptf(out, "Switching to %s — downloading it now. Press Enter anytime to continue in the background.\n", label)
	el := listenForEnter(sc)
	waitForModelSwitch(mgmtURL, modelID, out, tty, el)
	el.Drain(out)
}

// tinyBenchmarkDisableFlow is the benchmark-time counterpart of the install
// spec-check dialog: the active model benchmarked below the interactive floor
// and the ONLY lighter step-down is the tiny, below-floor 0.5B. Rather than the
// neutral "switch to a lighter model" flow, it confirms whether to keep local
// inference by dropping to that very-low-quality model, or turn it off. Default
// No → disable local inference; the node keeps working as a gateway/relay.
func tinyBenchmarkDisableFlow(
	mgmtURL string, nonInteractive bool, out io.Writer, sc *bufio.Scanner, tty bool,
	rec *management.BenchmarkRecommendation, resp *management.BenchmarkRunResponse,
) (*management.BenchmarkRunResponse, error) {
	from := modelWithQuality(rec.FromModelID, rec.FromVariantID)
	label := modelWithQuality(rec.ToModelID, rec.ToVariantID)
	writePromptf(out, "\n%s Local inference is slow here: %s measured %.0f tok/s, below the %.0f tok/s\n",
		emo("⚠", "!"), from, rec.MeasuredTokps, rec.FloorTokps)
	writePromptf(out, "   interactive floor. The only lighter model left is %s, whose coding\n", label)
	writePrompt(out, "   quality is very low and generally not worth running locally.")

	if nonInteractive {
		writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to revisit.\n", from)
		return resp, nil
	}

	// Two-line question so the default and the "No disables it" clarifier read
	// as one prompt; ynPrompt appends the [y/N] (default: No) hint.
	q := "Drop to that model and keep local inference?\n" +
		"  No turns local inference off — Waired still works as a gateway/relay."
	if ynPrompt(out, sc, q, false) {
		switchAndWait(mgmtURL, rec.ToModelID, label, out, sc, tty)
		return resp, nil
	}
	if err := disableLocalInference(mgmtURL); err != nil {
		writePromptf(out, "warn: could not disable local inference: %v\n", err)
	} else {
		writePrompt(out, "Local inference disabled — Waired keeps working as a gateway/relay.")
	}
	return resp, nil
}

// disableLocalInference POSTs the management soft-disable, which persists the
// desired-inference toggle so it survives daemon restarts.
func disableLocalInference(mgmtURL string) error {
	_, err := httpPost(mgmtURL+"/waired/v1/inference/disable", nil)
	return err
}

// offerBenchmark is the end-of-init interactive performance check + smoke
// test. After init starts the daemon, it waits briefly for the Management
// API, then asks whether to run a quick benchmark; on yes it runs a real
// inference (proving the path works end-to-end) and offers a lighter model
// if throughput is below the interactive floor. Non-interactive callers
// run it report-only (never switches). Best-effort: never errors / blocks.
func offerBenchmark(mgmtURL string, nonInteractive bool, out io.Writer, sc *bufio.Scanner, tty bool) benchmarkOutcome {
	if !waitDaemonReachable(mgmtURL, 15*time.Second) {
		writePrompt(out, emo("💡", "Tip:")+" once the agent is running, run `waired runtimes benchmark` to check interactive performance.")
		return benchmarkOutcome{}
	}
	if nonInteractive {
		resp, _ := benchmarkWithScanner(mgmtURL, true, out, sc, tty)
		return outcomeFrom(resp)
	}
	if !ynPrompt(out, sc, "Run a quick performance benchmark now?", true) {
		writePrompt(out, "Skipped. Run `waired runtimes benchmark` anytime to check performance.")
		return benchmarkOutcome{}
	}
	writePrompt(out, dim("Benchmarking local inference — warming up the model, please wait…"))
	// Reuse the same scanner for the (possible) model-switch prompt so we
	// don't layer two bufio readers over stdin.
	resp, _ := benchmarkWithScanner(mgmtURL, false, out, sc, tty)
	return outcomeFrom(resp)
}

// waitDaemonReachable polls the Management API until it answers or the
// timeout elapses; returns true once reachable. Used to give the
// just-started daemon a moment to bind before the benchmark probe.
func waitDaemonReachable(mgmtURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if daemonReachable(mgmtURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForBenchmark polls the daemon until the engine + active model are
// ready, then runs the benchmark and returns the full response (with
// the measurement plus any lighter/upgrade suggestion). ok=false means
// "could not obtain a result" (daemon too old, model never readied
// within the deadline, terminal pull failure) — the caller should
// treat that as a non-error skip.
func waitForBenchmark(mgmtURL string, out io.Writer) (resp *management.BenchmarkRunResponse, ok bool) {
	deadline := time.Now().Add(benchPollDeadline)
	announcedWait := false
	announcedEngine := false
	// noEngineDeadline is armed lazily on the first `no_engine` observation
	// and disarmed once any other state is seen, so a transient startup
	// `no_engine` is waited out but a host whose engine never comes up still
	// gives up after the grace rather than spinning to the full deadline.
	var noEngineDeadline time.Time
	engineSeen := false
	for {
		// Try the benchmark; the handler returns 425 until the engine and
		// model are both ready.
		status, body, err := benchPost(mgmtURL+"/waired/v1/inference/benchmark", nil)
		switch {
		case err != nil:
			// Transport error — the daemon isn't reachable (not started
			// yet, or restarting). Tell the user how to act instead of
			// returning silently (the `waired runtimes benchmark` complaint).
			writePromptf(out, "Could not reach the waired-agent service at %s (%v).\nStart it, then run `waired runtimes benchmark`.\n", mgmtURL, err)
			return nil, false
		case status == http.StatusNotFound:
			// Older daemon without the benchmark endpoint.
			writePrompt(out, "This waired-agent build doesn't support benchmarking yet; skipping.")
			return nil, false
		case status == http.StatusOK:
			var r management.BenchmarkRunResponse
			if jErr := json.Unmarshal(body, &r); jErr != nil {
				writePromptf(out, "Benchmark returned an unreadable response (%v); skipping.\n", jErr)
				return nil, false
			}
			return &r, true
		case status == http.StatusTooEarly:
			// Engine / model not ready yet. Consult /status to distinguish
			// "still loading" from a terminal failure so we don't spin for
			// the full deadline on a host that will never come up.
			state := inferenceSubsystemState(mgmtURL)
			switch state {
			case "pull_failed":
				writePrompt(out, "Model download failed; skipping the interactive-performance check.")
				return nil, false
			case "no_engine":
				// On a fresh bundled install the engine is still being
				// brought up at the first polls, so `no_engine` is transient
				// (issue #489). Wait it out within a grace window rather than
				// skipping immediately; only conclude the engine will never
				// come up once the grace elapses with no engine ever seen.
				if !engineSeen {
					if noEngineDeadline.IsZero() {
						noEngineDeadline = time.Now().Add(benchNoEngineGrace)
					}
					if time.Now().After(noEngineDeadline) {
						writePrompt(out, "No inference engine available; skipping the interactive-performance check.")
						return nil, false
					}
					if !announcedEngine {
						writePrompt(out, "Waiting for the inference engine to start before benchmarking… "+
							dim("(this can take a minute)"))
						announcedEngine = true
					}
				}
			case "":
				// /status was unreachable this tick — don't conclude the
				// engine is up (that would disarm the no_engine grace); just
				// keep polling and let the next tick re-read the state.
			default:
				// Engine is up (some non-no_engine state): disarm the
				// no_engine grace so a later blip can't cut the wait short,
				// and fall through to the model-download wait.
				engineSeen = true
				if !announcedWait {
					writePrompt(out, "Waiting for the model to finish downloading before benchmarking… "+
						dim("(this can take a few minutes)"))
					announcedWait = true
				}
			}
		default:
			// Unexpected status — surface it (don't block init) instead of
			// exiting silently.
			writePromptf(out, "Benchmark unavailable (HTTP %d); skipping.\n", status)
			return nil, false
		}

		if time.Now().After(deadline) {
			writePrompt(out, "Model not ready in time; run `waired runtimes benchmark` later to check performance.")
			return nil, false
		}
		time.Sleep(benchPollInterval)
	}
}

// inferenceSubsystemState GETs /inference/status and returns the
// subsystem_state, or "" on any error.
func inferenceSubsystemState(mgmtURL string) string {
	st, ok := fetchInferenceStatus(mgmtURL)
	if !ok {
		return ""
	}
	return st.SubsystemState
}

// activeModelForDisplay resolves the just-benchmarked (active) model from
// /inference/status for the no-recommendation "works" line — the benchmark
// response itself doesn't name the model (waired#773). Empty on any error
// (old daemon, unreachable); callers fall back to model-less wording.
func activeModelForDisplay(mgmtURL string) (modelID, variantID string) {
	st, ok := fetchInferenceStatus(mgmtURL)
	if !ok || st.Active == nil {
		return "", ""
	}
	return st.Active.ModelID, st.Active.VariantID
}

// acceptRecommendation POSTs the switch and returns the daemon's response —
// Downloading tells the caller whether a foreground download wait is worth
// starting (waired#774). A response an old daemon can't marshal decodes to
// the zero value (Downloading=false), which degrades to the pre-#774
// fire-and-forget behavior.
func acceptRecommendation(mgmtURL, modelID string) (management.PreferredModelResponse, error) {
	body, _ := json.Marshal(management.PreferredModelRequest{ModelID: modelID})
	respBody, err := httpPost(mgmtURL+"/waired/v1/inference/preferred-model", body)
	if err != nil {
		return management.PreferredModelResponse{}, err
	}
	var pmr management.PreferredModelResponse
	_ = json.Unmarshal(respBody, &pmr)
	return pmr, nil
}

func dismissRecommendation(mgmtURL, fromVariantID, toVariantID string) error {
	body, _ := json.Marshal(management.RecommendationDismissRequest{
		FromVariantID: fromVariantID,
		ToVariantID:   toVariantID,
	})
	_, err := httpPost(mgmtURL+"/waired/v1/inference/recommendation/dismiss", body)
	return err
}

// benchPost performs a status-aware POST: it returns the HTTP status code
// and body separately (unlike httpPost, which collapses non-2xx into an
// error) so the caller can branch on 425 / 404.
func benchPost(url string, body []byte) (int, []byte, error) {
	resp, err := benchHTTP.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}
