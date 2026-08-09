package main

import (
	"encoding/json"
	"io"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Owner-ruled install-flow step 6 (2026-08-08, waired-ai/waired#1067;
// waired-agent#585): the #496 host-speed cutoff used to default local
// inference off silently on the daemon path, with the terminal sitting
// right there. Between the engine install and the model wait, init now
// waits for the one-time measurement and, when it misses the budget,
// asks — default off. The silent daemon default stays for unattended
// paths: it is the same default, applied with nobody to ask.

var (
	// hostSpeedAskWait bounds how long init waits for the measurement.
	// Generous because what it waits for is the ~1 GB probe model's own
	// download plus three samples (#496) — and the multi-GB model wait
	// behind it dwarfs it either way. Vars so tests do not wait.
	hostSpeedAskWait = 20 * time.Minute
	hostSpeedAskPoll = 2 * time.Second
)

// hostSpeedPoll is one status read: the measurement (with the
// TurnedInferenceOff claim already cross-checked against the toggle,
// exactly as fetchHostSpeed does) plus the two states the guards read.
type hostSpeedPoll struct {
	hs           *management.HostSpeedStatus
	desiredState string
	subState     string
	ok           bool
}

func readHostSpeedPoll(mgmt string) hostSpeedPoll {
	body, err := httpGet(mgmt + "/waired/v1/inference/status")
	if err != nil {
		return hostSpeedPoll{}
	}
	var s inferenceStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return hostSpeedPoll{}
	}
	if hostSpeedFigure(s.HostSpeed) == "" {
		// Nothing to report yet. A screened host fills TurnFloorSeconds
		// and leaves TurnSeconds at zero (waired-agent#579), so this asks
		// for "is there a figure" rather than for one of the two fields —
		// checking TurnSeconds alone would park step 6 on the full wait
		// for a host that had already been judged.
		s.HostSpeed = nil
	}
	if s.HostSpeed != nil && s.DesiredState != string(state.InferenceDisabled) {
		s.HostSpeed.TurnedInferenceOff = false
	}
	return hostSpeedPoll{hs: s.HostSpeed, desiredState: s.DesiredState, subState: s.SubsystemState, ok: true}
}

// confirmHostSpeedBudget runs step 6. It never fails an install: every
// early return leaves the daemon exactly as it was, and the #133
// post-download benchmark still runs afterwards as the confirmation
// measurement.
//
// keptOn reports whether local AI is (as far as this step can tell)
// still on when it returns — false exactly when it observed or applied
// an off state. The model picker behind it (waired-agent#586) keys on
// this: asking which model to download right after local AI went off
// would be the flow contradicting itself. Unknown states (an unreachable
// daemon, a measurement that never came) stay true — the picker has its
// own fail-open guards.
//
// stillMine reports whether this terminal still owns the questions — a
// browser setup that started during the wait takes them over (§4.2),
// and a prompt racing the wizard is what it must never print.
func confirmHostSpeedBudget(mgmtURL string, inf daemonInitInference, nonInteractive bool, sc lineReader, out io.Writer, stillMine func() bool) (keptOn bool) {
	if inf.Enabled != nil && *inf.Enabled {
		// #465: the operator already answered with --inference-enabled=true.
		// The daemon's cutoff does not override a set toggle, and neither
		// may this ask.
		return true
	}

	deadline := time.Now().Add(hostSpeedAskWait)
	narrated := false
	misses := 0
	var p hostSpeedPoll
	for {
		p = readHostSpeedPoll(mgmtURL)
		if !p.ok {
			// An unreachable daemon — or an older build without the
			// status route — is not going to produce a measurement.
			// Three consecutive misses and this gives up rather than
			// parking init on the full wait (best-effort, like every
			// other read of this route).
			if misses++; misses >= 3 {
				return true
			}
		} else {
			misses = 0
		}
		if p.ok {
			if p.desiredState == string(state.InferenceDisabled) &&
				(p.hs == nil || !p.hs.TurnedInferenceOff) {
				// A person (or the step-4 decline) turned local AI off.
				// That is an answer, not a measurement — nothing to ask.
				return false
			}
			if p.subState == "no_engine" || p.subState == "stopped" {
				// Nothing is going to measure anything.
				return false
			}
			if p.hs != nil {
				break
			}
			if !narrated {
				writePromptf(out, "%s Measuring how fast this computer runs AI — one-time, a few minutes…\n",
					emo("⏱", "*"))
				narrated = true
			}
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(hostSpeedAskPoll)
	}

	hs := p.hs
	if !hostSpeedMissesBudget(hs) {
		return true // within budget (or an older daemon with no budget figure)
	}
	if !stillMine() {
		return true
	}
	// The judgement leads on both arms below, because both are about to
	// act on it — one by asking, one by announcing what it did.
	// `waired inference status` puts the state first instead; that command
	// was asked for the state. (Owner-approved copy, 2026-08-09,
	// waired-agent#579.)
	if nonInteractive {
		writePrompt(out, hostSpeedBelowSpecLine)
		for _, line := range hostSpeedComparisonLines(hs, "  ") {
			writePrompt(out, line)
		}
		writePromptf(out, "%s Non-interactive: turning local\ninference off. Re-enable with `waired inference on`.\n",
			hostSpeedNotRecommendedLine)
		if !hs.TurnedInferenceOff {
			turnLocalAIOff(mgmtURL, out)
		}
		return false
	}

	writePromptf(out, "\n%s %s\n", emo("🐢", "!"), hostSpeedBelowSpecLine)
	for _, line := range hostSpeedComparisonLines(hs, "     ") {
		writePrompt(out, line)
	}
	writePromptf(out, "   %s\n", hostSpeedNotRecommendedLine)
	// Two-line question so the default and the "No disables it" clarifier
	// read as one prompt, the tinyBenchmarkDisableFlow shape.
	q := "Keep local inference on anyway?\n" +
		"  No turns local inference off — Waired still works as a gateway/relay."
	if ynPrompt(out, sc, q, false) {
		if hs.TurnedInferenceOff {
			// Overturn the cutoff's silent default: the person just chose.
			if _, err := httpPost(mgmtURL+"/waired/v1/inference/enable", nil); err != nil {
				writePromptf(out, "warn: could not turn local AI back on (%v); run `waired inference on`\n", err)
			}
		}
		return true
	}
	if !hs.TurnedInferenceOff {
		turnLocalAIOff(mgmtURL, out)
	}
	writePrompt(out, "Local inference disabled — Waired keeps working as a gateway/relay.")
	return false
}
