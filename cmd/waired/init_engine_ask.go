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

// installEngineFit reduces GET /inference/catalog to the one step-4
// fact: whether Waired would choose a model for this host at all, and
// the warning reason when it would not.
//
// It deliberately does NOT report WHICH model. The recommendation is
// computed by the daemon from the engine and its version, and at step 4
// no engine is installed yet — so every variant with a minimum engine
// version is excluded and the answer can differ from the one the picker
// gives minutes later, on the same host, from the same function. Naming
// a model here presented that provisional answer as a final one:
// waired-agent#649 saw step 4 say "Qwen3.6 27B" and the picker, seconds
// after, mark "qwen3.6-35b-a3b — recommended for this computer". The
// picker is the one surface that asks after the facts are in, so it is
// the only one that names a model now.
//
// An unreachable or absent catalog (older daemon build) reads as fit:
// the question is still asked, with the safe default, rather than
// inventing a warning the data cannot back.
func installEngineFit(mgmt string) (fit bool, reason string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(mgmt + "/waired/v1/inference/catalog")
	if err != nil {
		return true, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, ""
	}
	var cat catalogDetailResp
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return true, ""
	}
	anyFits := false
	for _, f := range cat.Families {
		if f.RecommendedPick {
			return true, ""
		}
		if f.Fits {
			anyFits = true
		}
	}
	if anyFits {
		return false, "models can run here, but none of them is recommended for this hardware"
	}
	return false, "no bundled model fits in this computer's memory"
}

// confirmDaemonPathEngineInstall runs step 4 and reports whether the
// engine install may proceed. false means the operator (or the
// non-interactive default on an unfit host) declined: local AI is
// turned off so the rest of init — the model wait, the closing box —
// reads the host as a deliberate gateway/relay install (#569), and the
// exit code stays 0.
func confirmDaemonPathEngineInstall(mgmtURL string, inf daemonInitInference, nonInteractive bool, sc lineReader, out io.Writer) bool {
	forced := inf.Enabled != nil && *inf.Enabled
	fit, reason := installEngineFit(mgmtURL)
	switch engineInstallAsk(forced, nonInteractive, fit) {
	case engineAskInstall:
		return true
	case engineAskSkip:
		writePromptf(out, "Non-interactive: skipping local inference (%s).\n", reason)
		writePrompt(out, "Turn it on with `waired inference on`.")
		turnLocalAIOff(mgmtURL, out)
		return false
	}

	if fit {
		writePromptf(out, "\n%s This computer can run models locally. You choose which model in a moment.\n",
			emo("🤖", "*"))
	} else {
		writePromptf(out, "\n%s This computer is below the recommended spec for local inference: %s.\n",
			emo("⚠", "!"), reason)
		writePrompt(out, "  The smallest model would run slowly and may exhaust memory.")
	}
	switch ynAsk(out, sc, "Run models on this computer?", fit) {
	case ynYes:
		return true
	case ynNoAnswer:
		// Stdin ended before an answer arrived, so nobody is at the
		// keyboard — and on a fit host the default this would otherwise
		// take is Yes, which installs an engine and starts a multi-GB
		// download that no one asked for (waired-agent#1048, split out of
		// #1033).
		//
		// It is the shape waired-agent#754 named one flow earlier, in the
		// same words ynAsk's own doc uses: an exhausted stdin is not the
		// Enter a person presses. --non-interactive still means "take the
		// documented defaults" and is unchanged on every host; a closed
		// pipe never meant that, and this is the one question in the
		// wizard where reading it that way costs an install.
		//
		// Deliberately NOT a TTY check. `waired init` cannot force
		// non-interactive mode off a terminal — scripted installs pipe
		// their answers in, and scripts/dev/lib/installtest-enroll.sh
		// drives the model picker exactly that way — so what is acted on
		// here is the narrower fact that this particular question got no
		// answer.
		writePrompt(out)
		writePrompt(out, "No answer on stdin — nobody is here to say whether this computer should run models.")
	}
	// Both arms land the host in the same place and say so in the same
	// words; only the line above them differs, because only the reason
	// does.
	writePrompt(out, "Skipping local inference — Waired keeps working as a gateway/relay.")
	writePrompt(out, "Turn it on anytime with `waired inference on`.")
	turnLocalAIOff(mgmtURL, out)
	return false
}

// turnLocalAIOff records the step-4 decline with the daemon. A failure
// is a warning, never fatal: the operator's answer was "no install",
// and that much is already honoured by the caller.
func turnLocalAIOff(mgmtURL string, out io.Writer) {
	if err := disableLocalInference(mgmtURL); err != nil {
		writePromptf(out, "warn: could not turn local inference off (%v); turn it off with `waired inference off`\n", err)
	}
}
