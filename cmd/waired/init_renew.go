package main

import (
	"io"

	"github.com/waired-ai/waired-agent/internal/identity"
)

// confirmRenew is the gcloud-init-style prompt shown when `waired init`
// is run on a host that already has an identity.json. Returns true when
// the operator wants to proceed with re-authentication.
//
// In bypass-mode (test harness) or --non-interactive mode the prompt is
// skipped and renewal proceeds — those invocations are scripted and
// already signal intent.
func confirmRenew(in lineReader, out io.Writer, existing *identity.Identity, bypass, nonInteractive bool) bool {
	writePrompt(out, "Found an existing Waired configuration:")
	writePromptf(out, "  Account: %s\n", displayOrDash(existing.AccountEmail))
	writePromptf(out, "  Device:  %s\n", displayOrDash(displayDeviceName(existing)))
	writePromptf(out, "  Network: %s\n", displayOrDash(existing.NetworkName))
	writePromptf(out, "  Control: %s\n", displayOrDash(existing.ControlURL))
	writePrompt(out, "")
	if bypass || nonInteractive {
		writePrompt(out, "Non-interactive: signing in again.")
		return true
	}
	return ynPrompt(out, in, "Sign in to this computer again with Google?", true)
}

func displayOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
