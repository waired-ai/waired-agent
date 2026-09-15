package wgnet

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A peer present under a Public Share grant: the relay envelope carries
// its device id, which belongs to another account. PRODUCT CONTRACT:
// public share spec §8.5 — logs name a public peer by its pseudonym and
// never keep its real device id (waired-agent#1368).
const (
	grantPeerDeviceID = "dev_guest_real_id"
	grantPeerLogName  = "guest-0001"
)

func TestForeignKeyReportNamesARelayGrantPeerByLogName(t *testing.T) {
	var logs bytes.Buffer
	b := NewMultiplexBind(MultiplexBindConfig{
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
		SelfNodePub: selfNodePubB64,
	})
	b.SetPeerLogNames(map[string]string{grantPeerDeviceID: grantPeerLogName})

	msg := wgHandshake(t, wgMsgTypeInitiation, wgInitiationSize, otherNodePubB64)
	b.noteForeignKeyHandshake(msg, func() string { return b.relaySenderName(grantPeerDeviceID) })

	out := logs.String()
	if !strings.Contains(out, "handshakes authenticated against a public key this device does not hold") {
		t.Fatalf("no foreign-key report; got:\n%s", out)
	}
	if !strings.Contains(out, grantPeerLogName+" via relay") {
		t.Errorf("report does not name the sender by its log name:\n%s", out)
	}
	if strings.Contains(out, grantPeerDeviceID) {
		t.Errorf("report carries the grant peer's device id:\n%s", out)
	}
}

// Own-network senders, and ids the current map does not carry, keep
// their device id — the same trade the disco service documents on
// inboundSource. Record of today's behaviour.
func TestRelaySenderName(t *testing.T) {
	b := NewMultiplexBind(MultiplexBindConfig{})
	b.SetPeerLogNames(map[string]string{grantPeerDeviceID: grantPeerLogName})
	cases := map[string]string{
		grantPeerDeviceID: grantPeerLogName + " via relay",
		"dev_own_peer":    "dev_own_peer via relay",
		"":                "unidentified sender via relay",
	}
	for in, want := range cases {
		if got := b.relaySenderName(in); got != want {
			t.Errorf("relaySenderName(%q) = %q, want %q", in, got, want)
		}
	}
	b.SetPeerLogNames(nil)
	if got := b.relaySenderName(grantPeerDeviceID); got != grantPeerDeviceID+" via relay" {
		t.Errorf("after the table is cleared = %q", got)
	}
}

// wireguard-go prints a relay endpoint's DstToString in some debug lines
// (invalid handshakes, cookie replies), and that string carries the
// peer's device id. The log bridge rewrites it for grant peers only.
func TestRedactRelayEndpoints(t *testing.T) {
	b := NewMultiplexBind(MultiplexBindConfig{})
	grantEP := (&relayEndpoint{url: "wss://r1.example/relay/v1/connect", dstDeviceID: grantPeerDeviceID, dstNodeKey: "nk1"}).DstToString()
	ownEP := (&relayEndpoint{url: "wss://r1.example/relay/v1/connect", dstDeviceID: "dev_own_peer", dstNodeKey: "nk2"}).DstToString()
	line := "Received invalid initiation message from " + grantEP + " and " + ownEP

	if got := b.redactRelayEndpoints(line); got != line {
		t.Errorf("with no grant peers the line changed: %q", got)
	}

	b.SetPeerLogNames(map[string]string{grantPeerDeviceID: grantPeerLogName})
	got := b.redactRelayEndpoints(line)
	if strings.Contains(got, grantPeerDeviceID) {
		t.Errorf("grant peer's device id survived: %q", got)
	}
	if !strings.Contains(got, "#dst="+grantPeerLogName+"&nk=nk1") {
		t.Errorf("grant endpoint not renamed: %q", got)
	}
	if !strings.Contains(got, ownEP) {
		t.Errorf("own-network endpoint was rewritten: %q", got)
	}
	if tail := "#dst=" + grantPeerDeviceID; b.redactRelayEndpoints("x "+tail) != "x #dst="+grantPeerLogName {
		t.Errorf("an id at the end of the line is not renamed: %q", b.redactRelayEndpoints("x "+tail))
	}
}

// The bridge hands every formatted line to redact.
func TestWireguardLoggerAppliesRedact(t *testing.T) {
	var logs bytes.Buffer
	l := wireguardLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		func(s string) string { return strings.ReplaceAll(s, grantPeerDeviceID, grantPeerLogName) })
	l.Verbosef("Receiving cookie response from %s", "relay:u#dst="+grantPeerDeviceID+"&nk=k")
	l.Errorf("failed for %s", grantPeerDeviceID)
	if out := logs.String(); strings.Contains(out, grantPeerDeviceID) || strings.Count(out, grantPeerLogName) != 2 {
		t.Errorf("bridge did not apply redact to both levels:\n%s", out)
	}
}
