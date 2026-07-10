package signer_test

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// scenarioTestNetworkMap returns a fixed NetworkMap shape used by the
// scenario-related sign/verify tests. Centralised so each test focuses
// on what it asserts about the ActiveTestScenario field.
func scenarioTestNetworkMap() signer.NetworkMap {
	return signer.NetworkMap{
		Version:   1,
		NetworkID: "wn_scenario_test",
		MapEpoch:  7,
		IssuedAt:  "2026-05-09T00:00:00Z",
		Self: signer.NetworkMapPeer{
			DeviceID:         "dev_self",
			DeviceName:       "self",
			OverlayIP:        "100.96.0.10",
			NodePublicKey:    base64.StdEncoding.EncodeToString(make([]byte, 32)),
			MachinePublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
			AllowedServices:  []string{"inference"},
		},
		Peers: []signer.NetworkMapPeer{{
			DeviceID:         "dev_peer",
			DeviceName:       "peer",
			OverlayIP:        "100.96.0.11",
			NodePublicKey:    base64.StdEncoding.EncodeToString(make([]byte, 32)),
			MachinePublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
			AllowedServices:  []string{"inference"},
		}},
	}
}

func TestCanonicalJSONStableAcrossEncodings(t *testing.T) {
	a := map[string]any{
		"network_id":         "wn_001",
		"allowed_services":   []string{"inference"},
		"machine_public_key": "ed25519:abc",
		"version":            1,
		"issued_at":          "2026-04-30T12:00:00Z",
	}
	b := map[string]any{
		"version":            1,
		"issued_at":          "2026-04-30T12:00:00Z",
		"machine_public_key": "ed25519:abc",
		"allowed_services":   []string{"inference"},
		"network_id":         "wn_001",
	}
	ja, err := signer.CanonicalJSON(a)
	if err != nil {
		t.Fatalf("canonical a: %v", err)
	}
	jb, err := signer.CanonicalJSON(b)
	if err != nil {
		t.Fatalf("canonical b: %v", err)
	}
	if string(ja) != string(jb) {
		t.Fatalf("canonical JSON differs:\n a=%s\n b=%s", ja, jb)
	}
}

func TestDeviceCertificateRoundtrip(t *testing.T) {
	k, err := signer.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	cert := signer.DeviceCertificate{
		Version:          1,
		NetworkID:        "wn_test",
		DeviceID:         "dev_test",
		AccountID:        "acct_test",
		MachinePublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		NodePublicKey:    base64.StdEncoding.EncodeToString(make([]byte, 32)),
		OverlayIP:        "100.96.0.10",
		AllowedServices:  []string{"inference"},
		IssuedAt:         now.Format(time.RFC3339),
		ExpiresAt:        now.Add(signer.IssueDefaultDuration).Format(time.RFC3339),
	}
	signed, err := k.SignDeviceCertificate(cert)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if signed.Signature == "" {
		t.Fatalf("signature should be populated")
	}
	if err := signer.VerifyDeviceCertificate(k.Public, signed); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Tamper.
	tampered := signed
	tampered.OverlayIP = "100.96.0.99"
	if err := signer.VerifyDeviceCertificate(k.Public, tampered); err == nil {
		t.Fatalf("expected verification failure after tampering")
	}

	// Wrong key.
	other, _ := signer.Generate()
	if err := signer.VerifyDeviceCertificate(other.Public, signed); err == nil {
		t.Fatalf("expected verification failure with wrong public key")
	}
}

func TestNetworkMapSignWithRelays(t *testing.T) {
	k, err := signer.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	nm := signer.NetworkMap{
		Version:   1,
		NetworkID: "wn_1",
		MapEpoch:  3,
		IssuedAt:  "2026-05-01T00:00:00Z",
		Self: signer.NetworkMapPeer{
			DeviceID:         "dev_self",
			DeviceName:       "self",
			OverlayIP:        "100.96.0.10",
			NodePublicKey:    base64.StdEncoding.EncodeToString(make([]byte, 32)),
			MachinePublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
			AllowedServices:  []string{"inference"},
			HomeRelay:        "relay_local_1",
		},
		Peers: []signer.NetworkMapPeer{{
			DeviceID:         "dev_peer",
			DeviceName:       "peer",
			OverlayIP:        "100.96.0.11",
			NodePublicKey:    base64.StdEncoding.EncodeToString(make([]byte, 32)),
			MachinePublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
			AllowedServices:  []string{"inference"},
			HomeRelay:        "relay_local_1",
		}},
		Relays: []signer.NetworkMapRelay{{
			RelayID:        "relay_local_1",
			URL:            "wss://127.0.0.1:9478/relay/v1/connect",
			TLSFingerprint: "deadbeef",
		}},
	}
	signed, err := k.SignNetworkMap(nm)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if signed.Signature == "" {
		t.Fatalf("signature should populate")
	}
	if err := signer.VerifyNetworkMap(k.Public, signed); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Tamper with a relay url; verification must fail.
	tampered := signed
	tampered.Relays = append([]signer.NetworkMapRelay(nil), signed.Relays...)
	tampered.Relays[0].URL = "wss://attacker.example/relay/v1/connect"
	if err := signer.VerifyNetworkMap(k.Public, tampered); err == nil {
		t.Fatalf("expected verification failure after relay tampering")
	}
}

// TestNetworkMapSignWithoutScenario_NoFieldInCanonical pins the
// production-CP property: when ActiveTestScenario is nil, the canonical
// JSON bytes contain no `active_test_scenario` key. A regression here
// (e.g. dropping the `omitempty` json tag on the field, or marshalling
// nil pointers as `null`) would diverge canonical bytes between agents
// that compile the new struct vs old, breaking signature verification.
func TestNetworkMapSignWithoutScenario_NoFieldInCanonical(t *testing.T) {
	nm := scenarioTestNetworkMap()
	if nm.ActiveTestScenario != nil {
		t.Fatalf("test setup: ActiveTestScenario should be nil")
	}
	canonical, err := signer.CanonicalJSON(nm)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if bytes.Contains(canonical, []byte("active_test_scenario")) {
		t.Fatalf("canonical JSON unexpectedly contains active_test_scenario:\n%s", canonical)
	}
}

// TestNetworkMapSignWithScenario_RoundTripVerifies covers the
// testharness-CP property: a scenario directive embedded in the signed
// map round-trips and any tamper to it (ScenarioID, PeerDeviceID,
// Direction, ExpectedNonce) is rejected.
func TestNetworkMapSignWithScenario_RoundTripVerifies(t *testing.T) {
	k, err := signer.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	nm := scenarioTestNetworkMap()
	nm.ActiveTestScenario = &signer.ActiveTestScenario{
		ScenarioID:    signer.ScenarioIDFallbackBasic,
		PeerDeviceID:  "dev_peer",
		Direction:     signer.ScenarioDirectionBoth,
		ExpectedNonce: 3,
	}
	signed, err := k.SignNetworkMap(nm)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if signed.Signature == "" {
		t.Fatalf("signature should populate")
	}
	if signed.ActiveTestScenario == nil {
		t.Fatalf("ActiveTestScenario should round-trip")
	}
	if err := signer.VerifyNetworkMap(k.Public, signed); err != nil {
		t.Fatalf("verify: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*signer.NetworkMap)
	}{
		{"ScenarioID", func(m *signer.NetworkMap) { m.ActiveTestScenario.ScenarioID = signer.ScenarioIDAsymmetricDirect }},
		{"PeerDeviceID", func(m *signer.NetworkMap) { m.ActiveTestScenario.PeerDeviceID = "dev_imposter" }},
		{"Direction", func(m *signer.NetworkMap) { m.ActiveTestScenario.Direction = signer.ScenarioDirectionInbound }},
		{"ExpectedNonce", func(m *signer.NetworkMap) { m.ActiveTestScenario.ExpectedNonce = 99 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tampered := signed
			scen := *signed.ActiveTestScenario
			tampered.ActiveTestScenario = &scen
			c.mutate(&tampered)
			if err := signer.VerifyNetworkMap(k.Public, tampered); err == nil {
				t.Fatalf("expected verification failure after tampering %s", c.name)
			}
		})
	}
}

// TestActiveTestScenario_DirectionEnumStability pins the wire-format
// string values of the ScenarioID and Direction enums so a refactor
// that rewords any of them surfaces in tests rather than silently
// breaking deployed CI runners (which pattern-match on the values via
// runner.sh or stored Cloud Logging filters).
func TestActiveTestScenario_DirectionEnumStability(t *testing.T) {
	pairs := []struct {
		got, want string
	}{
		{signer.ScenarioIDFallbackBasic, "fallback-basic"},
		{signer.ScenarioIDUpgradeBasic, "upgrade-basic"},
		{signer.ScenarioIDFlapSuppression, "flap-suppression"},
		{signer.ScenarioIDAsymmetricDirect, "asymmetric-direct"},
		{signer.ScenarioDirectionBoth, "both"},
		{signer.ScenarioDirectionInbound, "inbound"},
		{signer.ScenarioDirectionOutbound, "outbound"},
	}
	for _, p := range pairs {
		if p.got != p.want {
			t.Errorf("enum value mismatch: got %q want %q", p.got, p.want)
		}
	}
}
