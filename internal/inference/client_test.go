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
type fakeDialer struct{ addr string }

func (f fakeDialer) DialOverlayTCP(ctx context.Context, _ netip.Addr, _ uint16) (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, "tcp", f.addr)
}

func TestClientPing(t *testing.T) {
	srv := httptest.NewServer(NewServer("bob").Handler())
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient(fakeDialer{addr: addr}, 2*time.Second)

	body, latency, err := c.Ping(context.Background(), netip.MustParseAddr("100.96.0.11"), 9474)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !body.OK || body.Device != "bob" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if latency <= 0 {
		t.Fatalf("latency=%s", latency)
	}
}
