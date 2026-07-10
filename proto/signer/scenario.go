package signer

// Scenario id values carried in ActiveTestScenario.ScenarioID. The
// testharness build of waired-agent compiles a dispatcher that acts on
// these; the production build compiles only NoopDispatcher and ignores
// any directive that arrives over the wire.
const (
	ScenarioIDFallbackBasic    = "fallback-basic"
	ScenarioIDUpgradeBasic     = "upgrade-basic"
	ScenarioIDFlapSuppression  = "flap-suppression"
	ScenarioIDAsymmetricDirect = "asymmetric-direct"
)

// Direction values for ActiveTestScenario.Direction. "both" blocks
// inbound + outbound between the two peers; "inbound" blocks only the
// receiver's ingress from the named peer; "outbound" blocks only the
// receiver's egress to the named peer. Asymmetric-direct is the only
// scenario whose Direction differs per-receiver — Control Plane
// projects the per-agent value when emitting NetworkMap.
const (
	ScenarioDirectionBoth     = "both"
	ScenarioDirectionInbound  = "inbound"
	ScenarioDirectionOutbound = "outbound"
)

// ActiveTestScenario describes a test directive a bypass-IDP CP has
// installed for a given network. It rides on the signed Network Map so
// that signature verification works the same shape on prod and
// testharness agents — both decode the field, only the testharness
// build has a dispatcher receiver, so the directive is a no-op in
// production.
//
// PeerDeviceID is the receiver's view of the peer the directive
// targets (after Control Plane has done per-agent direction projection
// for asymmetric scenarios). ExpectedNonce echoes the CP-assigned
// monotonic counter so CI can pin "this is the post-PUT sample, not a
// stale one".
type ActiveTestScenario struct {
	ScenarioID    string `json:"scenario_id"`
	PeerDeviceID  string `json:"peer_device_id,omitempty"`
	Direction     string `json:"direction,omitempty"`
	ExpectedNonce int64  `json:"expected_nonce,omitempty"`
}
