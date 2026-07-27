//go:build linux || darwin

package tray

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// shortTempDir is t.TempDir() with a path short enough to bind a unix socket
// under. macOS puts TMPDIR at /var/folders/<2>/<30>/T — 48 bytes — and
// t.TempDir() then appends the test name, ten random digits and /001, which
// overruns darwin's 104-byte sockaddr_un.sun_path and makes bind() fail with
// EINVAL. That is what turned this suite red the first time it ran on macOS
// (#152). Every site in the tree is fixed, not just the ones that happen to
// overrun today: the rest sit near the boundary and fail as soon as a test
// name grows. Reproducible on Linux with a 52-byte TMPDIR, which leaves the
// same headroom against Linux's 108-byte limit (#216).
//
// Doing this here rather than shortening TMPDIR from the workflow keeps the
// constraint stated where a test author will see it, and leaves the product's
// own answer (internal/platform/paths/paths_unix.go's sunPathBudget plus a
// hashed fallback, pinned by a TMPDIR-independent test) untouched.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveMgmtSocket starts an httptest server on a unix-domain socket and
// points $WAIRED_MGMT_SOCKET at it, so the tray's write client (which
// resolves its endpoint through ipcclient at dial time) reaches it. Since
// waired#838 every mutating tray call goes over this socket rather than
// the loopback TCP port.
func serveMgmtSocket(t *testing.T, h http.Handler) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv(paths.MgmtSocketEnvOverride, sock)
}

func TestClient_Pause_UnsupportedYields404Sentinel(t *testing.T) {
	serveMgmtSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))

	c := NewClient("")
	if err := c.Pause(context.Background()); !errors.Is(err, ErrPauseUnsupported) {
		t.Errorf("expected ErrPauseUnsupported, got %v", err)
	}
	if err := c.Resume(context.Background()); !errors.Is(err, ErrPauseUnsupported) {
		t.Errorf("expected ErrPauseUnsupported, got %v", err)
	}
}

func TestClient_Pause_OK(t *testing.T) {
	got := ""
	serveMgmtSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))

	c := NewClient("")
	if err := c.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "POST /waired/v1/pause" {
		t.Errorf("server saw %q", got)
	}
}

// TestClient_EmptyBodyPost_SetsJSONContentType guards the waired#836 fix:
// the browser-hardened management API 415s a write without Content-Type:
// application/json, so the tray's empty-body POST helpers must set it.
// c.Pause exercises post(); c.StopEngine exercises postWithUnsupported().
func TestClient_EmptyBodyPost_SetsJSONContentType(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(c *Client) error
	}{
		{"pause", func(c *Client) error { return c.Pause(context.Background()) }},
		{"stop-engine", func(c *Client) error { return c.StopEngine(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ct string
			serveMgmtSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ct = r.Header.Get("Content-Type")
				w.WriteHeader(http.StatusOK)
			}))
			if err := tc.call(NewClient("")); err != nil {
				t.Fatal(err)
			}
			if ct != "application/json" {
				t.Errorf("empty-body POST Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// TestClient_WriteDialError confirms a missing socket surfaces the wrapped,
// operator-facing message rather than a raw "dial unix ...: no such file".
func TestClient_WriteDialError(t *testing.T) {
	// Short path here too: the contract under test is "the socket file is
	// absent", and a path over sun_path would make it fail for the wrong
	// reason on darwin.
	t.Setenv(paths.MgmtSocketEnvOverride, filepath.Join(shortTempDir(t), "absent.sock"))
	err := NewClient("").Pause(context.Background())
	if err == nil {
		t.Fatal("expected an error dialing a missing socket")
	}
	if got := err.Error(); !strings.Contains(got, "management socket") {
		t.Fatalf("error %q should mention the management socket", got)
	}
}
