//go:build linux || darwin

package ipcclient

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// serveSocket starts an httptest server on a unix socket and points
// $WAIRED_MGMT_SOCKET at it, so Endpoint/dial resolve to it.
func serveSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "mgmt.sock")
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
	return sock
}

func TestEndpointHonoursEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom.sock")
	t.Setenv(paths.MgmtSocketEnvOverride, want)
	if got := Endpoint(); got != want {
		t.Fatalf("Endpoint() = %q, want %q", got, want)
	}
}

// TestRoundTripOverSocket proves the transport actually carries an HTTP
// request over the unix socket, addressed to the dummy BaseURL authority.
func TestRoundTripOverSocket(t *testing.T) {
	var gotPath, gotMethod string
	serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
	}))

	resp, err := NewHTTPClient(2*time.Second).Post(BaseURL+"/waired/v1/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("POST over socket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/waired/v1/pause" || gotMethod != http.MethodPost {
		t.Fatalf("server saw %s %q, want POST /waired/v1/pause", gotMethod, gotPath)
	}
}

// TestWrapDialErrorNamesMissingSocket checks the operator-facing wording:
// a missing socket must be reported as "not found" and name the endpoint,
// not surface a bare "dial unix ...: no such file or directory".
func TestWrapDialErrorNamesMissingSocket(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.sock")
	t.Setenv(paths.MgmtSocketEnvOverride, missing)

	_, err := NewHTTPClient(2 * time.Second).Get(BaseURL + "/waired/v1/status")
	if err == nil {
		t.Fatal("expected a dial error against a missing socket")
	}
	wrapped := WrapDialError(err).Error()
	if !strings.Contains(wrapped, "not found") {
		t.Errorf("want a not-found classification, got %q", wrapped)
	}
	if !strings.Contains(wrapped, missing) {
		t.Errorf("wrapped error should name the endpoint %q, got %q", missing, wrapped)
	}
	if !strings.Contains(wrapped, "waired-agent running") {
		t.Errorf("want an actionable hint, got %q", wrapped)
	}
}

func TestWrapDialErrorNilIsNil(t *testing.T) {
	if err := WrapDialError(nil); err != nil {
		t.Fatalf("WrapDialError(nil) = %v, want nil", err)
	}
}
