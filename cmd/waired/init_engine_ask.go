package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Owner-ruled install-flow step 4 (2026-08-08, waired-ai/waired#1067;
// waired-agent#584): before the terminal path installs an engine, ask.
// A host with a recommended pick defaults to Yes; a host below the
// recommended spec is warned and defaults to No. Declining is a choice,
// not a fault: local AI is turned off and init finishes as a
// gateway/relay install (#551, #569).

// engineAskAnswer is what the precedence table decided to do before any
// terminal I/O happens.
type engineAskAnswer int

const (
	// engineAskInstall: proceed without asking.
	engineAskInstall engineAskAnswer = iota
	// engineAskSkip: decline without asking (non-interactive default on
	// an unfit host) — turn local AI off and skip the install.
	engineAskSkip
	// engineAskPrompt: put the question to the operator.
	engineAskPrompt
)

// engineInstallAsk is the precedence half of step 4, kept free of I/O
// so all its rows are table-testable: explicit flag > non-interactive
// default > interactive ask (waired-agent#584).
func engineInstallAsk(forcedOn, nonInteractive, fit bool) engineAskAnswer {
	switch {
	case forcedOn:
		// --inference-enabled=true answers the question, exactly as its
		// help text promises. (=false never reaches here: the disable was
		// already applied and the daemon stops wanting an engine.)
		return engineAskInstall
	case !nonInteractive:
		return engineAskPrompt
	case fit:
		return engineAskInstall
	default:
		return engineAskSkip
	}
}

// installRecommendation reduces GET /inference/catalog to the step-4
// facts: whether this host has a recommended pick, its display label,
// and the warning reason when it does not.
//
// An unreachable or absent catalog (older daemon build) reads as fit
// with no label: the question is still asked, with the safe default,
// rather than inventing a warning the data cannot back.
func installRecommendation(mgmt string) (fit bool, label, reason string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(mgmt + "/waired/v1/inference/catalog")
	if err != nil {
		return true, "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, "", ""
	}
	var cat catalogDetailResp
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return true, "", ""
	}
	anyFits := false
	for _, f := range cat.Families {
		if f.RecommendedPick {
			if f.DisplayName != "" {
				return true, f.DisplayName, ""
			}
			return true, f.ModelID, ""
		}
		if f.Fits {
			anyFits = true
		}
	}
	if anyFits {
		return false, "", "models can run here, but Waired would not choose any of them for this hardware"
	}
	return false, "", "no bundled model fits in this computer's memory"
}

// confirmDaemonPathEngineInstall runs step 4 and reports whether the
// engine install may proceed. false means the operator (or the
// non-interactive default on an unfit host) declined: local AI is
// turned off so the rest of init — the model wait, the closing box —
// reads the host as a deliberate gateway/relay install (#569), and the
// exit code stays 0.
func confirmDaemonPathEngineInstall(mgmtURL string, inf daemonInitInference, nonInteractive bool, sc lineReader, out io.Writer) bool {
	forced := inf.Enabled != nil && *inf.Enabled
	fit, label, reason := installRecommendation(mgmtURL)
	switch engineInstallAsk(forced, nonInteractive, fit) {
	case engineAskInstall:
		return true
	case engineAskSkip:
		writePromptf(out, "Non-interactive: skipping local AI (%s).\n", reason)
		writePrompt(out, "Turn it on with `waired inference on`.")
		turnLocalAIOff(mgmtURL, out)
		return false
	}

	if fit {
		if label != "" {
			writePromptf(out, "\n%s This computer can run AI models locally (recommended: %s).\n",
				emo("🤖", "*"), label)
		} else {
			writePromptf(out, "\n%s This computer can run AI models locally.\n", emo("🤖", "*"))
		}
	} else {
		writePromptf(out, "\n%s This computer is below the recommended spec for local AI: %s.\n",
			emo("⚠", "!"), reason)
		writePrompt(out, "  The smallest model would run slowly and may exhaust memory.")
	}
	if ynPrompt(out, sc, "Run AI models on this computer?", fit) {
		return true
	}
	writePrompt(out, "Skipping local AI — Waired keeps working as a gateway/relay.")
	writePrompt(out, "Turn it on anytime with `waired inference on`.")
	turnLocalAIOff(mgmtURL, out)
	return false
}

// turnLocalAIOff records the step-4 decline with the daemon. A failure
// is a warning, never fatal: the operator's answer was "no install",
// and that much is already honoured by the caller.
func turnLocalAIOff(mgmtURL string, out io.Writer) {
	if err := disableLocalInference(mgmtURL); err != nil {
		writePromptf(out, "warn: could not turn local AI off (%v); turn it off with `waired inference off`\n", err)
	}
}
