package disco

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// TestObserverTelemetry_PerFamilyCountersAndFirstV6 drives observeOne
// against a v6 relay host, confirms the v6 attempt counter increments,
// then injects a matching stun_response from a v6 src and confirms the
// v6 response counter + firstObservedV6At stamp. The v4 counters must
// remain zero for the v6-only run — proves the family classification
// is not over-counting both families per probe round.
//
// This is the primary unit guard for the
// docs/records/20260516/1430-ipv6-flake-stratification.md telemetry
// described in cmd/waired-agent/stats.go's stats payload.
func TestObserverTelemetry_PerFamilyCountersAndFirstV6(t *testing.T) {
	secret := []byte("relay-shared-secret")
	bind := newFakeBind()
	s, _, _ := newService(t, secret, bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// v6 relay URL — extractHostFromRelayURL returns "2001:db8::1",
	// which makeUDPDst classifies as v6 via the ':' heuristic.
	s.UpdateRelays([]string{"wss://[2001:db8::1]:443/relay/v1/connect"})

	// Wait for the first v6 stun_request.
	var sent sentDiscoPacket
	select {
	case sent = <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no stun_request observed within 2s")
	}
	if !strings.HasPrefix(sent.Dst, "udp6:") {
		t.Fatalf("expected v6 dst, got %q", sent.Dst)
	}

	// Pre-response counter snapshot: attempts_v6 ≥ 1 (one send fired),
	// no responses yet, no first-v6 stamp.
	a4, a6, r4, r6 := s.STUNCounters()
	if a6 < 1 {
		t.Fatalf("expected stunAttemptsV6 ≥ 1 after first send, got %d", a6)
	}
	if r4 != 0 || r6 != 0 {
		t.Fatalf("expected zero responses before injecting, got v4=%d v6=%d", r4, r6)
	}
	if !s.FirstObservedV6At().IsZero() {
		t.Fatal("firstObservedV6At should be zero before any v6 sample")
	}
	_ = a4

	// Decode the request to grab the nonce, then build a matching v6 response.
	reqFrame, err := wireframe.Decode(sent.Payload)
	if err != nil {
		t.Fatalf("decode req: %v", err)
	}
	observedAddr := netip.MustParseAddrPort("[2001:db8::42]:51820")
	resp := &wireframe.Frame{
		Type:         wireframe.TypeSTUNResponse,
		HasNonce:     true,
		Nonce:        reqFrame.Nonce,
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
		HasObserved:  true,
		ObservedAddr: observedAddr,
	}
	respBytes := mustHMACSign(t, resp, secret)
	v6Src := netip.MustParseAddrPort("[2001:db8::1]:3478")
	bind.inbound <- wireframe.Inbound{Payload: respBytes, Src: v6Src}

	// Wait for the response counter to bump + the observer to stamp
	// firstObservedV6At. Tied to the observeOnce loop's
	// STUNObserveActive (100 ms in newService), so 2 s is generous.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, _, r6 := s.STUNCounters()
		first := s.FirstObservedV6At()
		if r6 >= 1 && !first.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			a4, a6, r4, r6 := s.STUNCounters()
			t.Fatalf("counters never converged: a4=%d a6=%d r4=%d r6=%d firstV6=%v",
				a4, a6, r4, r6, s.FirstObservedV6At())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Final invariants: no v4 attempt should ever have fired (only v6
	// relay was registered), and the v6 attempt counter should be
	// monotonically ≥ the response counter.
	a4, a6, r4, r6 = s.STUNCounters()
	if a4 != 0 {
		t.Errorf("stunAttemptsV4 expected 0 for v6-only relay, got %d", a4)
	}
	if r4 != 0 {
		t.Errorf("stunResponsesV4 expected 0 for v6-only relay, got %d", r4)
	}
	if a6 < r6 {
		t.Errorf("stunAttemptsV6 (%d) must be ≥ stunResponsesV6 (%d)", a6, r6)
	}
}

// TestObserverTelemetry_V4Attempt confirms attempts_v4 increments
// (and not v6) when the relay host is a hostname / v4 literal — the
// negative of the v6 test above.
func TestObserverTelemetry_V4Attempt(t *testing.T) {
	secret := []byte("relay-shared-secret")
	bind := newFakeBind()
	s, _, _ := newService(t, secret, bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.UpdateRelays([]string{"wss://relay.example.com:443/relay/v1/connect"})

	var sent sentDiscoPacket
	select {
	case sent = <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no stun_request observed within 2s")
	}
	if !strings.HasPrefix(sent.Dst, "udp4:") {
		t.Fatalf("expected v4 dst, got %q", sent.Dst)
	}
	a4, a6, _, _ := s.STUNCounters()
	if a4 < 1 {
		t.Fatalf("expected stunAttemptsV4 ≥ 1, got %d", a4)
	}
	if a6 != 0 {
		t.Errorf("stunAttemptsV6 expected 0 for v4-only relay, got %d", a6)
	}
}

// TestIsV6Source checks the family classifier used by
// handleSTUNResponse to attribute response counters. v4-in-v6 mapped
// addresses MUST be classified as v4 — the agent's StdNetBind may
// surface v4 packets received on its v6 socket as ::ffff:a.b.c.d.
func TestIsV6Source(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{"plain_v4", "192.0.2.1", false},
		{"v6_gua", "2001:db8::1", true},
		{"v6_loopback", "::1", true},
		{"v6_link_local", "fe80::1", true},
		{"v4_in_v6_mapped", "::ffff:192.0.2.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := netip.MustParseAddr(tc.addr)
			if got := isV6Source(a); got != tc.want {
				t.Errorf("isV6Source(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
