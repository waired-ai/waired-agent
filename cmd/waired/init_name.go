package main

import (
	"os"

	"github.com/waired-ai/waired-agent/internal/identity"
)

// reportedDeviceName is what this machine reports as its own name at
// enrollment — the value that travels in `device_facts.hostname`.
//
// `existing` is taken and deliberately ignored. The control plane keeps
// two names now: the hostname a machine reports, rewritten on every
// re-enrollment, and the display name an operator edits, which
// re-enrollment leaves alone (waired-ai/waired#1204). Re-init used to
// default this from identity.json, so every re-authentication pushed the
// agent's stored copy back as the reported hostname — which, before the
// split, was what reverted a rename made in the web console. The
// parameter stays in the signature so a table test can pin that a stored
// name does not change the answer (#767).
//
// hostname is passed in rather than read here so the decision is a pure
// function of its inputs, the shape initStateDirMode established.
func reportedDeviceName(flagValue string, existing *identity.Identity, hostname string) string {
	_ = existing
	if flagValue != "" {
		return flagValue
	}
	return hostname
}

// hostnameOrEmpty is os.Hostname with its error folded into the empty
// string: a machine that cannot name itself reports nothing and lets the
// control plane apply its own fallback, which is what the previous
// inline code did.
func hostnameOrEmpty() string {
	host, _ := os.Hostname()
	return host
}
