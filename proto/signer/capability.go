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
)
