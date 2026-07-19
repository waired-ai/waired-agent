//go:build linux || darwin

package ipcclient

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

// --- waired#81: per-instance endpoint resolution ------------------------

// bindSocket creates a real listening unix socket at path, so
// resolveEndpoint's os.Stat probe sees something a client could reach.
func bindSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func TestResolveEndpoint_SocketOverrideBeatsStateDir(t *testing.T) {
	forced := filepath.Join(t.TempDir(), "forced.sock")
	t.Setenv(paths.MgmtSocketEnvOverride, forced)
	t.Setenv(paths.EnvOverride, t.TempDir())
	if got := resolveEndpoint(); got != forced {
		t.Fatalf("resolveEndpoint = %q, want %q", got, forced)
	}
}

// TestResolveEndpoint_InstanceStateDir is the client half of waired#81: when
// $WAIRED_STATE_DIR names a non-default dir AND that instance's socket is
// live, address it rather than the machine-wide runtime socket.
func TestResolveEndpoint_InstanceStateDir(t *testing.T) {
	t.Setenv(paths.MgmtSocketEnvOverride, "")
	stateDir := t.TempDir()
	t.Setenv(paths.EnvOverride, stateDir)

	want := paths.InstanceMgmtEndpoint(stateDir)
	if want == "" {
		t.Fatal("a temp dir should count as a non-default state dir")
	}
	bindSocket(t, want)

	if got := resolveEndpoint(); got != want {
		t.Fatalf("resolveEndpoint = %q, want the instance socket %q", got, want)
	}
}

// TestResolveEndpoint_UnboundInstanceStillNamesTheInstance: when a
// non-default $WAIRED_STATE_DIR has no socket in it AND no System socket is
// running either, resolution must still land on the instance path, so
// WrapDialError names the dir the operator actually chose rather than a
// /run/waired path they never asked for.
func TestResolveEndpoint_UnboundInstanceStillNamesTheInstance(t *testing.T) {
	t.Setenv(paths.MgmtSocketEnvOverride, "")
	stateDir := t.TempDir() // non-default, but no socket inside
	t.Setenv(paths.EnvOverride, stateDir)

	if _, err := os.Stat(paths.MgmtEndpoint(paths.System)); err == nil {
		t.Skip("a System management socket is live on this host; the System probe legitimately wins")
	}
	if got, want := resolveEndpoint(), paths.InstanceMgmtEndpoint(stateDir); got != want {
		t.Fatalf("resolveEndpoint = %q, want the instance path %q", got, want)
	}
}

// TestResolveEndpoint_DefaultStateDirKeepsRuntimeChain is the regression guard
// for the waired#81 trap: the install tests and the routing sentinel all
// export $WAIRED_STATE_DIR pointing AT the OS default, while the daemon is
// started by systemd/launchd and never sees the variable. Such a client must
// keep resolving the runtime endpoint the daemon actually bound.
func TestResolveEndpoint_DefaultStateDirKeepsRuntimeChain(t *testing.T) {
	t.Setenv(paths.MgmtSocketEnvOverride, "")
	// Read the OS default BEFORE overriding, then point the override at it.
	def := paths.StateDir(paths.System)
	t.Setenv(paths.EnvOverride, def)

	got := resolveEndpoint()
	if got != paths.MgmtEndpoint(paths.System) && got != paths.MgmtEndpoint(paths.Interactive) {
		t.Fatalf("resolveEndpoint = %q, want a System or Interactive runtime endpoint", got)
	}
}
