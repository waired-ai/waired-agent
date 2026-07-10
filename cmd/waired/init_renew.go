package main

import (
	"bufio"
	"io"

	"github.com/waired-ai/waired-agent/internal/identity"
)

// initStepLabels selects the "[i/N]" prefixes runInit prints before each
// phase. Fresh init has four phases (sign-in, register, persist,
// inference + integration) plus a "done"; renew skips deploy +
// integration and re-uses the inference config from disk, leaving
// sign-in / re-register / done.
type initStepLabelSet struct {
	signIn      string
	register    string
	persist     string
	inference   string // unused in renew mode
	integration string // unused in renew mode
	done        string
}

func initStepLabels(renewing bool) initStepLabelSet {
	if renewing {
		return initStepLabelSet{
			signIn:   emo("🔐", "*") + " [1/3]",
			register: emo("🔁", "*") + " [2/3]",
			persist:  emo("💾", "*") + " [3/3]",
			done:     emo("✅", "*") + " [done]",
		}
	}
	return initStepLabelSet{
		signIn:      emo("🔐", "*") + " [1/4]",
		register:    emo("📦", "*") + " [2/4]",
		persist:     emo("💾", "*") + " [3/4]",
		inference:   emo("🧠", "*") + " [3a/4]",
		integration: emo("🔌", "*") + " [3b/4]",
		// A calm ✅ marks enrollment done mid-flow; the celebratory 🎉 is
		// reserved for the final success box after the model download +
		// benchmark actually finish (runInit).
		done: emo("✅", "*") + " [4/4]",
	}
}

// confirmRenew is the gcloud-init-style prompt shown when `waired init`
// is run on a host that already has an identity.json. Returns true when
// the operator wants to proceed with re-authentication.
//
// In bypass-mode (test harness) or --non-interactive mode the prompt is
// skipped and renewal proceeds — those invocations are scripted and
// already signal intent.
func confirmRenew(in io.Reader, out io.Writer, existing *identity.Identity, bypass, nonInteractive bool) bool {
	writePrompt(out, "Existing Waired configuration found:")
	writePromptf(out, "  Account: %s\n", displayOrDash(existing.AccountEmail))
	writePromptf(out, "  Device:  %s\n", displayOrDash(displayDeviceName(existing)))
	writePromptf(out, "  Network: %s\n", displayOrDash(existing.NetworkName))
	writePromptf(out, "  Control: %s\n", displayOrDash(existing.ControlURL))
	writePrompt(out, "")
	if bypass || nonInteractive {
		writePrompt(out, "Proceeding with re-authentication (non-interactive).")
		return true
	}
	sc := bufio.NewScanner(in)
	return ynPrompt(out, sc, "Re-authenticate this device with Google?", true)
}

func displayOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
