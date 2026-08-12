//go:build linux || darwin

package ipcclient

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// TestReadClientPrefersTheSocket: when the endpoint is there, a read goes
// over it and never touches the TCP base.
func TestReadClientPrefersTheSocket(t *testing.T) {
	var socketHits, tcpHits int
	serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socketHits++
		w.WriteHeader(http.StatusOK)
	}))
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tcpHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer tcp.Close()

	resp, err := NewReadClient(tcp.URL, 2*time.Second).Get(BaseURL + "/waired/v1/identity")
	if err != nil {
		t.Fatalf("GET over socket: %v", err)
	}
	_ = resp.Body.Close()
	if socketHits != 1 || tcpHits != 0 {
		t.Fatalf("socket hits = %d, tcp hits = %d; want 1 and 0", socketHits, tcpHits)
	}
}

// TestReadClientFallsBackToTCP is the other half of the daemon's fail-open:
// with no socket bound the daemon serves every read over TCP, and the
// client has to find it there. A mock daemon with no socket at all
// (scripts/dev/mock-mgmt) relies on the same path.
func TestReadClientFallsBackToTCP(t *testing.T) {
	t.Setenv(paths.MgmtSocketEnvOverride, filepath.Join(shortTempDir(t), "absent.sock"))

	var gotPath, gotHost string
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost = r.URL.Path, r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer tcp.Close()

	resp, err := NewReadClient(tcp.URL, 2*time.Second).Get(BaseURL + "/waired/v1/identity")
	if err != nil {
		t.Fatalf("GET with fallback: %v", err)
	}
	_ = resp.Body.Close()
	if gotPath != "/waired/v1/identity" {
		t.Fatalf("fallback server saw path %q, want /waired/v1/identity", gotPath)
	}
	// The dummy IPC authority must not survive into the TCP request: the
	// daemon's browserGuard allow-lists loopback Hosts and would 403 it.
	if gotHost == "waired-mgmt" {
		t.Fatalf("fallback request carried the IPC authority %q; browserGuard would reject it", gotHost)
	}
}

// TestReadClientWithoutFallbackReportsTheDialError: an empty tcpBase turns
// the fallback off, and the caller still gets a dial error WrapDialError
// can classify.
func TestReadClientWithoutFallbackReportsTheDialError(t *testing.T) {
	missing := filepath.Join(shortTempDir(t), "absent.sock")
	t.Setenv(paths.MgmtSocketEnvOverride, missing)

	_, err := NewReadClient("", 2*time.Second).Get(BaseURL + "/waired/v1/identity")
	if err == nil {
		t.Fatal("expected a dial error with no fallback configured")
	}
	if !endpointUnavailable(err) {
		t.Fatalf("endpointUnavailable(%v) = false; the fallback would never fire", err)
	}
}
