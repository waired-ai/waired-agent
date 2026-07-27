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

// Integration targets — accepted entries of DesiredIntegrations.Enabled.
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
const (
	IntegrationClaudeCode = "claude-code"
	IntegrationOpenCode   = "opencode"
	IntegrationOpenClaw   = "openclaw"
)

// IsValidIntegrationTarget reports whether t is a known integration
// target. Used by the CP API validator; the agent applies known targets
// and ignores the rest rather than failing the whole instruction.
func IsValidIntegrationTarget(t string) bool {
	switch t {
	case IntegrationClaudeCode, IntegrationOpenCode, IntegrationOpenClaw:
		return true
	}
	return false
}
