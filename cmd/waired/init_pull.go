package main

import (
	"encoding/json"
	"io"
	"slices"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// waitForBundledModel blocks until the agent's active (bundled) model has
// finished downloading and the engine is serving it, rendering a percentage
// progress line as the pull proceeds. It is the foreground half of issue #490:
// on a fresh bundled install the agent pulls the multi-GB model in the
// background right after init starts it, so without this step `waired init`
// would return while the download proceeds invisibly. Polling
// /waired/v1/inference/status — which the agent already populates with
// byte-level Models.Downloads — keeps the agent the single owner of the
// engine/pull; init only watches and renders.
//
// It returns true once the model is ready, so the post-init benchmark (and the
// issue #133 auto-fallback) can then run immediately. It returns false —
// without holding init hostage — when readiness can't be confirmed (daemon
// unreachable, terminal pull failure, inference disabled/parked, the engine
// never coming up within benchNoEngineGrace, or benchPollDeadline elapsing).
// The agent keeps pulling in the background regardless, so callers treat false
// as a soft skip and continue.
func waitForBundledModel(mgmtURL string, out io.Writer, tty bool) bool {
	if !waitDaemonReachable(mgmtURL, 15*time.Second) {
		// The caller already prints a "start the agent, then …" hint; stay
		// quiet here so we don't double up.
		return false
	}
	deadline := time.Now().Add(benchPollDeadline)
	var noEngineDeadline time.Time // armed on no_engine, disarmed once engine is up
	line := downloadLineState{lastPct: -1}
	var rate rateWindow
	dlHinted := false // one-time "this is a multi-GB transfer" hint before the bar

	// lastStep dedups the per-phase step lines: each distinct transitional
	// subsystem_state prints one concise note as it is entered, so the user
	// watches the engine move through its phases (start → prepare → download
	// → load → ready) instead of staring at a single silent line. The live
	// download bar uses the stepDownloading sentinel so a following step line
	// terminates the in-place bar exactly once.
	lastStep := ""
	announce := func(step, msg string) {
		if step == lastStep {
			return
		}
		lastStep = step
		endProgressLine(out, tty, &line)
		writePrompt(out, msg)
	}

	for {
		st, ok := fetchInferenceStatus(mgmtURL)
		switch {
		case !ok:
			// /status unreachable this tick — keep waiting, re-read next tick.
		case modelReady(st):
			endProgressLine(out, tty, &line)
			writePromptf(out, "%s  %s ready\n", emo("✅", "[ok]"), activeModelName(st))
			return true
		case st.SubsystemState == "pull_failed" || modelFailed(st):
			endProgressLine(out, tty, &line)
			writePromptf(out, "Model download failed; the agent will keep retrying. "+
				"Run `waired models pull %s` to retry, or `waired runtimes benchmark` later.\n", activeModelName(st))
			return false
		case st.SubsystemState == "disabled" || st.SubsystemState == "stopped":
			// Inference won't become ready while disabled / parked — don't block.
			return false
		case st.SubsystemState == "no_engine":
			// Engine still being brought up on a fresh bundled install
			// (issue #489): wait it out within the grace, then conclude it
			// won't come up.
			if noEngineDeadline.IsZero() {
				noEngineDeadline = time.Now().Add(benchNoEngineGrace)
			}
			if time.Now().After(noEngineDeadline) {
				endProgressLine(out, tty, &line)
				writePrompt(out, "The inference engine still isn't up; the agent will keep bringing it up in the background.")
				writePrompt(out, "Check progress with `waired status`; if it persists, see `waired doctor` or `journalctl -u waired-agent -e`.")
				return false
			}
			announce("no_engine", "Waiting for the inference engine to start… "+
				dim("(first run installs the engine — this can take a few minutes)"))
		default:
			// Engine is up; a download may be in flight. Disarm the no_engine
			// grace so a later blip re-arms a fresh window instead of expiring.
			noEngineDeadline = time.Time{}
			if dl, found := activeDownload(st); found && dl.TotalBytes > 0 {
				speed := rate.observe(time.Now(), dl.CompletedBytes)
				pct := int(dl.CompletedBytes * 100 / dl.TotalBytes)
				if !dlHinted {
					dlHinted = true
					announce("download_hint", dim("Downloading the model — a multi-GB transfer that can take a few minutes."))
				}
				lastStep = stepDownloading // the bar owns the line; let a later step end it
				drawDownloadLine(out, tty, &line, activeModelName(st), pct, dl.CompletedBytes, dl.TotalBytes, speed)
			} else {
				announce(st.SubsystemState, prepMessage(st))
			}
		}

		if time.Now().After(deadline) {
			endProgressLine(out, tty, &line)
			writePrompt(out, "Model still downloading; it will finish in the background. "+
				"Run `waired status` to watch progress, or `waired runtimes benchmark` later to check performance.")
			return false
		}
		time.Sleep(pullPollInterval)
	}
}

// pullPollInterval is the gap between /inference/status polls while init
// watches the model download. Deliberately tighter than benchPollInterval:
// the bar redraws — and the rate re-samples — once per poll, and at 3 s the
// line sat visually unchanged long enough to read as frozen (the byte
// counts only tick every 0.1 GB). A var so tests can shrink it.
var pullPollInterval = 1 * time.Second

// rateWindowSpan is how far back rateWindow smooths the download rate.
const rateWindowSpan = 5 * time.Second

// rateWindow smooths the polled download rate over a short rolling window
// of (time, bytes) samples, so 1 s polls don't make the displayed number
// jitter — or vanish whenever a single poll happens to see no byte
// movement (the old two-poll delta did exactly that, stripping the rate
// off the bar). observe returns -1 until samples span time (rate unknown
// yet) and the windowed average afterwards — 0 during a genuine stall,
// which drawDownloadLine renders as "(0 B/s)" so a stalled transfer looks
// different from a frozen UI. A byte regression (the agent restarted the
// pull) resets the window.
type rateWindow struct {
	samples []rateSample
}

type rateSample struct {
	at    time.Time
	bytes int64
}

func (w *rateWindow) observe(now time.Time, completed int64) int64 {
	if n := len(w.samples); n > 0 && completed < w.samples[n-1].bytes {
		w.samples = w.samples[:0]
	}
	w.samples = append(w.samples, rateSample{at: now, bytes: completed})
	// Drop samples that fell out of the window, but keep one older sample
	// as the anchor — pruning to a single sample would flip a long stall
	// back to "unknown" instead of decaying the rate to 0.
	cutoff := now.Add(-rateWindowSpan)
	for len(w.samples) > 1 && w.samples[1].at.Before(cutoff) {
		w.samples = w.samples[1:]
	}
	first, last := w.samples[0], w.samples[len(w.samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	if secs <= 0 {
		return -1
	}
	return int64(float64(last.bytes-first.bytes) / secs)
}

// stepDownloading is the lastStep sentinel used while the live download bar
// owns the output line; it is not a real subsystem_state, so the next step
// note always differs from it and terminates the bar.
const stepDownloading = "__downloading__"

// prepMessage maps a non-terminal subsystem_state (engine up, model not yet
// downloading bytes) to a concise one-line step note, so the pre-download
// phases announce themselves instead of waiting silently.
func prepMessage(st management.InferenceStatus) string {
	switch st.SubsystemState {
	case "initializing":
		return "Starting the inference engine…"
	case "starting":
		return "Engine starting…"
	case "loading":
		return "Loading " + activeModelName(st) + "…"
	case "awaiting_model":
		return "Preparing to download " + activeModelName(st) + "…"
	case "degraded":
		return "Using a fallback inference engine…"
	default:
		return "Preparing the model…"
	}
}

// endProgressLine terminates an in-place TTY progress line with a newline so a
// following message starts on its own line. No-op when nothing was drawn or
// off a TTY (there the progress lines already end in newlines).
func endProgressLine(out io.Writer, tty bool, st *downloadLineState) {
	if tty && st.lastPct >= 0 {
		writePrompt(out)
	}
}

// fetchInferenceStatus GETs /inference/status and decodes it; ok is false on
// any transport / decode error so callers can keep polling.
func fetchInferenceStatus(mgmtURL string) (st management.InferenceStatus, ok bool) {
	body, err := httpGet(mgmtURL + "/waired/v1/inference/status")
	if err != nil {
		return management.InferenceStatus{}, false
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return management.InferenceStatus{}, false
	}
	return st, true
}

// modelReady reports whether the active model is downloaded and serving — the
// subsystem reports "ready", or (defensively, e.g. a degraded fallback engine)
// the active model id is in the ready set.
func modelReady(st management.InferenceStatus) bool {
	if st.SubsystemState == "ready" {
		return true
	}
	return st.Active != nil && slices.Contains(st.Models.Ready, st.Active.ModelID)
}

// modelFailed reports whether the active model's most recent download failed.
func modelFailed(st management.InferenceStatus) bool {
	return st.Active != nil && slices.Contains(st.Models.Failed, st.Active.ModelID)
}

// activeModelName is the active model id, or a generic label before one is set.
func activeModelName(st management.InferenceStatus) string {
	if st.Active != nil && st.Active.ModelID != "" {
		return st.Active.ModelID
	}
	return "the model"
}

// activeDownload returns the in-flight download for the active model, falling
// back to the first in-flight download (the bundled pull is the only one at
// install time). ok is false when no sized download is in progress yet.
func activeDownload(st management.InferenceStatus) (management.ModelDownload, bool) {
	if st.Active != nil {
		for _, d := range st.Models.Downloads {
			if d.Model == st.Active.ModelID {
				return d, true
			}
		}
	}
	if len(st.Models.Downloads) > 0 {
		return st.Models.Downloads[0], true
	}
	return management.ModelDownload{}, false
}
