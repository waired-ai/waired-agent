//go:build linux || darwin

package management

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestServeLocalUnixSocket exercises the real kernel path a unit test over
// Handler() cannot: ServeLocal binds a unix-domain socket and serves the
// full mux over it (no loopback/browser middleware), and a client dialing
// the socket gets a 200 from GET /status (waired#838).
func TestServeLocalUnixSocket(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	sockPath := filepath.Join(t.TempDir(), "mgmt.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeLocal(ctx, sockPath) }()

	cl := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}

	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		//nolint:noctx // short-lived test client, dial handles context
		resp, err = cl.Get("http://waired-mgmt/waired/v1/status")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status over socket: got %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case e := <-errc:
		if e != nil {
			t.Fatalf("ServeLocal returned %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeLocal did not return after ctx cancel")
	}
}

// TestServeLocalEmptyEndpointIsNoop confirms an empty endpoint disables the
// socket cleanly (returns nil, does not bind).
func TestServeLocalEmptyEndpointIsNoop(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	if err := srv.ServeLocal(context.Background(), ""); err != nil {
		t.Fatalf("ServeLocal(\"\") = %v, want nil", err)
	}
	if srv.socketUp.Load() {
		t.Fatal("socketUp should stay false when no endpoint is bound")
	}
}
