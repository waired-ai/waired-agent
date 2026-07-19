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
)
