package signer

// Agent capability strings. An agent declares its capabilities to the
// CP on every network-map poll (the capabilities field lives in the CP
// API request type, not here — it is unsigned client→server input);
// the CP persists them and gates capability-dependent map fields on
// them (public share spec §8.4).
//
// Defined in proto because both sides of the wire compare the literal
// string: the CP's poll intake / distribution gate and the agent's
// poller + NAVI hints must reference the same constant.
const (
	// CapabilityPublicShareV1 declares that this agent understands the
	// Public Share v1 map fields (NetworkMapPeer.Grant,
	// InferenceState.PublicShare/PublicCapacity) and the cross-network
	// relay frame field (EncryptedPacket.DstNetworkID). The CP never
	// emits those fields to a poller that has not declared it, keeping
	// the signed map byte-identical for older agents.
	CapabilityPublicShareV1 = "public-share-v1"

	// CapabilityOnboardingV1 declares that this agent understands the
	// NAVI-onboarding desired-state map fields
	// (InferenceState.DesiredEngine / DesiredModelID /
	// DesiredBenchmarkGen, waired#835 §6/§14). The CP never emits
	// those fields to a poller that has not declared it, keeping the
	// signed map byte-identical for older agents.
	CapabilityOnboardingV1 = "onboarding-v1"

	// CapabilityOnboardingV2 declares that this agent understands the
	// second wave of NAVI-onboarding wire (waired#932): the signed-map
	// field InferenceState.DesiredIntegrations, and the setup-progress
	// additions SetupProgress.Driver, SetupStep.RateBps and the
	// SetupBenchmark trial fields.
	//
	// Only DesiredIntegrations needs the gate for correctness — it is
	// the one that rides the signed map, where an agent that does not
	// know the field drops it on canonical re-marshal and fails
	// verification. The progress additions travel the separate
	// telemetry push, which is never signed or distributed, so they are
	// safe to send unconditionally.
	//
	// They are nonetheless covered by the same constant because the
	// reader needs them to be: absent a declaration, a wizard cannot
	// tell "this agent does not publish a driver" from "no surface has
	// claimed this run yet", and would have to treat a legitimate empty
	// value as a fault. One capability answers both questions.
	CapabilityOnboardingV2 = "onboarding-v2"

	// CapabilityOnboardingV3 declares that this agent understands the
	// model-generation retry lever (waired-agent#136): the signed-map
	// field InferenceState.DesiredModelGen, and the setup-progress echo
	// SetupProgress.ModelGen that answers it.
	//
	// Only DesiredModelGen needs the gate, for the same reason
	// DesiredIntegrations did: it rides the signed map, so an agent that
	// does not know the field drops it on canonical re-marshal and fails
	// verification. The echo travels the unsigned telemetry push.
	//
	// The echo is nonetheless covered by this constant because the
	// READER needs it to be. A wizard that cannot tell "this agent does
	// not publish a generation" from "it has not picked up the retry
	// yet" would leave its retry button spinning forever on every older
	// agent. One capability answers both questions.
	CapabilityOnboardingV3 = "onboarding-v3"

	// CapabilityOnboardingV4 declares that this agent understands the
	// operator's explicit local-AI answer (waired-agent#597): the
	// signed-map field InferenceState.DesiredInference, applied as the
	// same persisted soft-disable / re-enable a person's own
	// `waired inference off|on` writes.
	//
	// The field needs the gate for the reason V2 and V3 each state: it
	// rides the signed map, so an agent that does not know it drops it
	// on canonical re-marshal and fails verification. And the READER
	// needs the constant regardless — a wizard that cannot tell "this
	// agent will never act on off" from "it has not acted yet" would
	// wait forever on every older agent.
	CapabilityOnboardingV4 = "onboarding-v4"

	// CapabilityContextWindowV1 declares that this agent understands
	// InferenceState.ContextWindow — the window a device says its engine
	// is loaded with, which the requesting router uses to decide whether
	// a peer may serve a given traffic window at all.
	//
	// It needs the gate for the reason DesiredIntegrations and
	// DesiredModelGen did: the field rides the signed map, so an agent
	// that does not know it drops it on canonical re-marshal and fails
	// verification. This one differs from those two in WHERE it appears —
	// they are CP-injected onto the poller's own Self entry, this one is
	// agent-reported and travels on every PEER entry — so the CP has to
	// strip it across the whole map for an undeclared poller, not just
	// from Self.
	//
	// A reader that predates the field sees 0, which every consumer must
	// already treat as "declares nothing" and fail open on. So the gate
	// protects signature verification, not correctness of the routing
	// decision: an old agent simply routes the way it does today.
	CapabilityContextWindowV1 = "context-window-v1"

	// CapabilityRAMAvailableV1 declares that this agent understands
	// HardwareSummary.RAMAvailableGB — the install-time available-memory
	// measurement behind the OS deduction
	// max(OSMemoryAllowanceGB, RAMTotalGB − RAMAvailableGB)
	// (waired-agent#568).
	//
	// It needs the gate for the reason ContextWindow did: the field is
	// agent-reported and rides the signed map on every PEER entry, so
	// an agent that does not know it drops it on canonical re-marshal
	// and fails verification, and the CP has to strip it across the
	// whole map for an undeclared poller, not just from Self.
	//
	// A reader that predates the field sees 0, which every consumer
	// already treats as "measurement unavailable" and answers with the
	// OSMemoryAllowanceGB constant — an old agent simply computes the
	// deduction the way it does today.
	CapabilityRAMAvailableV1 = "ram-available-v1"

	// CapabilityRAMAvailableV2 declares that this agent additionally
	// understands HardwareSummary.RAMAvailableMeasuredAt — WHEN the
	// measurement above was taken (waired-agent#699).
	//
	// A second constant rather than a wider reading of the first: an
	// agent declaring ram-available-v1 knows RAMAvailableGB and nothing
	// else, so it drops the timestamp on canonical re-marshal and fails
	// verification. Reusing v1 would break precisely the generation it
	// was added for. Same reasoning that gave the onboarding family its
	// own numbered constants.
	//
	// Declaring v2 is expected to come with v1 — a timestamp with no
	// value to date is noise, not half an answer — and the CP does not
	// have to trust an agent to get that right: it strips both when v1
	// is missing, whatever v2 says.
	CapabilityRAMAvailableV2 = "ram-available-v2"
)
