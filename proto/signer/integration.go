package signer

// DesiredIntegrations is the per-coding-agent integration selection the
// NAVI setup wizard made for one device (waired#835 §6, waired#932).
// It rides InferenceState on the device's OWN Self map entry; see
// InferenceState.DesiredIntegrations for the injection and gating
// rules.
//
// It is a struct behind a pointer, not a bare []string, because three
// states have to stay distinguishable:
//
//	nil                    no instruction — every host that never ran
//	                       a NAVI setup, and the byte-identical common
//	                       case for the signed map
//	{}                     the wizard asked and every toggle is OFF:
//	                       write nothing
//	{"enabled":["…"]}      write exactly the listed targets
//
// A bare slice collapses the middle case into the first (omitempty
// turns an empty array into an absent field), which would leave the
// wizard unable to tell "the integration step is never coming" from
// "this device was never asked" — the silent-false-success shape of
// waired#904.
//
// The toggles are setup-only and therefore never destructive: at first
// install there is no prior configuration to remove, so OFF means "do
// not write" rather than "remove". The applier is the elevated
// executor; the daemon does not become a privilege bridge (§8.3).
type DesiredIntegrations struct {
	// Enabled lists the coding-agent targets to configure, as
	// Integration* constants. Unknown entries are ignored by the agent
	// so a newer wizard cannot make an older agent fail; an empty list
	// means "configure nothing".
	Enabled []string `json:"enabled,omitempty"`
}

// Integration targets — the entries DesiredIntegrations.Enabled may
// carry. Acceptance is a separate question from existence: see
// IsValidIntegrationTarget and IsRetiredIntegrationTarget below.
//
// The values match the agent's own adapter IDs
// (internal/integration.AgentID). They are re-declared here rather than
// shared because proto is the wire contract and may not import the
// agent's internal packages; both sides compare the literal string, so
// the two lists must be changed together.
//
// Carrying enum IDs — and only enum IDs — is what keeps §17.1 intact:
// a path or a command from the CP is unrepresentable on this channel,
// so the desired-state wire cannot name where anything gets written.
//
// A withdrawn id keeps its constant forever. The string stays reserved:
// re-using it for anything else would make an agent that still carries
// the old adapter apply the wrong thing to a real home directory, and
// the wire has no version to disambiguate it by.
const (
	IntegrationClaudeCode = "claude-code"
	// IntegrationOpenCode was withdrawn in waired-agent#333 and restored
	// in waired-agent#981 (waired#1263). The string was never reused in
	// between, which is what made restoring it under the same value
	// safe: every agent that ever carried an adapter for it wrote the
	// same plugin file.
	IntegrationOpenCode = "opencode"
	IntegrationOpenClaw = "openclaw"
)

// IsValidIntegrationTarget reports whether t is a target the control
// plane may instruct. Used by the CP API validator; the agent applies
// known targets and ignores the rest rather than failing the whole
// instruction.
//
// A retired target is deliberately NOT valid (none is retired today;
// see IsRetiredIntegrationTarget). That is what makes a removal safe
// for devices whose stored instruction still names the target: the
// agent's existing "unknown targets are ignored" rule (see the
// flattener in cmd/waired-agent/setup_desired.go) drops it, so a stored
// ["claude-code","<retired>"] applies claude-code alone and a stored
// ["<retired>"] collapses to "asked, nothing selected" — which still
// reports an integration step, so setup completes instead of waiting
// forever for a row nobody will ever send (the waired#983 class).
func IsValidIntegrationTarget(t string) bool {
	switch t {
	case IntegrationClaudeCode, IntegrationOpenCode, IntegrationOpenClaw:
		return true
	}
	return false
}

// IsRetiredIntegrationTarget reports whether t names an integration that
// Waired used to support and has since removed.
//
// It exists so the control plane can tell "a target this build has never
// heard of" (a malformed or hostile request — reject it) from "a target
// we ourselves shipped and then withdrew" (a stale browser tab, or a row
// written before the removal — drop it and carry on). Rejecting the
// second would fail the whole desired-state write over a value the
// operator never typed.
//
// The retired set is empty today: opencode was the only member
// (waired-agent#333) and was restored in waired-agent#981. The function
// stays — the published proto surface is additive-only, and the
// distinction it encodes is the mechanism any future withdrawal rides.
func IsRetiredIntegrationTarget(t string) bool {
	_ = t
	return false
}
