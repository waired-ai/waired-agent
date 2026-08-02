package inference

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// fakeDialer ignores the requested overlay IP/port and instead dials the
// supplied "real" loopback address. This lets us exercise the HTTP wiring of
// Client without standing up wireguard-go.
//
// delay stalls the dial by a known amount so the round trip Ping measures is
// longer than the clock can round away. Client.NewClient sets
// DisableKeepAlives, so every request dials afresh and the stall lands inside
// the measured window.
type fakeDialer struct {
	addr  string
	delay time.Duration
}

func (f fakeDialer) DialOverlayTCP(ctx context.Context, _ netip.Addr, _ uint16) (net.Conn, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, "tcp", f.addr)
}

// pingDialDelay must stay comfortably above Windows' monotonic clock
// granularity — the shared-page timer advances in ~0.5 ms ticks at best and
// ~15.6 ms at worst, depending on whether anything on the machine has raised
// the timer resolution.
const pingDialDelay = 50 * time.Millisecond

func TestClientPing(t *testing.T) {
	srv := httptest.NewServer(NewServer("bob").Handler())
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient(fakeDialer{addr: addr, delay: pingDialDelay}, 2*time.Second)

	body, latency, err := c.Ping(context.Background(), netip.MustParseAddr("100.96.0.11"), 9474)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !body.OK || body.Device != "bob" {
		t.Fatalf("unexpected body: %+v", body)
	}
	// PRODUCT CONTRACT: Ping reports the round trip it measured, not a zero
	// placeholder — callers rank peers on it. Asserting against an injected
	// delay is what makes that checkable: the previous `latency <= 0` looked
	// like the same contract but was really a bet that a loopback round trip
	// outlasts one clock tick, which on Windows it need not (#400).
	if latency < pingDialDelay {
		t.Fatalf("latency=%s, want >= %s (the delay injected into the dial)", latency, pingDialDelay)
	}
}
