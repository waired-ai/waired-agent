//go:build linux || darwin

package management

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// shortTempDir is t.TempDir() with a path short enough to bind a unix socket
// under. macOS puts TMPDIR at /var/folders/<2>/<30>/T — 48 bytes — and
// t.TempDir() then appends the test name, ten random digits and /001, which
// overruns darwin's 104-byte sockaddr_un.sun_path and makes bind() fail with
// EINVAL. Every site here is fixed, not just the ones that happen to overrun
// today: the rest sit near the boundary and fail as soon as a test name grows.
// Reproducible on Linux with a 52-byte TMPDIR, which leaves the same headroom
// against Linux's 108-byte limit (#216).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestServeLocalUnixSocket exercises the real kernel path a unit test over
// Handler() cannot: ServeLocal binds a unix-domain socket and serves the
// full mux over it (no loopback/browser middleware), and a client dialing
// the socket gets a 200 from GET /status (waired#838).
func TestServeLocalUnixSocket(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	sockPath := filepath.Join(shortTempDir(t), "mgmt.sock")

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

// TestServeLocalSocketCarriesWritesAndUnlistedReads covers what the socket
// exists FOR, which the GET /status case above does not: a write, and a
// read outside the TCP allow-list, both of which the loopback listener
// refuses once the socket is up. It also pins that the socket handler
// applies none of the TCP middleware — the request carries the IPC client's
// dummy Host and, for the write, no JSON Content-Type, either of which
// browserGuard would reject on TCP.
func TestServeLocalSocketCarriesWritesAndUnlistedReads(t *testing.T) {
	pause := newFakePause(state.PhaseActive)
	srv := New(fakeStatus{}, fakePinger{}).
		WithPause(pause).
		WithIdentity(fakeIdentity{v: IdentityView{Enrolled: true}}).
		WithBrowserHardening().
		WithSocketWritesOnly(true).
		WithSocketReadsOnly(true)
	sockPath := filepath.Join(shortTempDir(t), "mgmt.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeLocal(ctx, sockPath) }()

	cl := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}

	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		//nolint:noctx // short-lived test client, dial handles context
		resp, err = cl.Post("http://waired-mgmt/waired/v1/pause", "", nil)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("POST over unix socket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /pause over socket: got %d, want 200", resp.StatusCode)
	}
	if cur, _ := pause.Phase(); cur != state.PhasePaused {
		t.Fatalf("phase after the socket write = %q, want paused; the write did not reach the controller", cur)
	}

	//nolint:noctx // short-lived test client, dial handles context
	resp, err = cl.Get("http://waired-mgmt/waired/v1/identity")
	if err != nil {
		t.Fatalf("GET /identity over unix socket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /identity over socket: got %d, want 200", resp.StatusCode)
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
