//go:build linux || darwin

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
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

// TestMgmtWritesRouteToSocket asserts the waired#838 routing contract: a
// management write issued against the loopback TCP URL actually travels
// over the local IPC socket, while the POST /ping liveness probe stays on
// TCP (the daemon's writeGuard exempts it there).
//
// TestMain clears mgmtWriteBase for the rest of the binary, so this test
// restores production routing explicitly.
func TestMgmtWritesRouteToSocket(t *testing.T) {
	prev := mgmtWriteBase
	mgmtWriteBase = ipcclient.BaseURL
	t.Cleanup(func() { mgmtWriteBase = prev })

	var sockPath, sockMethod, tcpPath string

	sock := filepath.Join(shortTempDir(t), "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	sockSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sockPath, sockMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
	}))
	_ = sockSrv.Listener.Close()
	sockSrv.Listener = ln
	sockSrv.Start()
	t.Cleanup(sockSrv.Close)
	t.Setenv(paths.MgmtSocketEnvOverride, sock)

	tcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tcpPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tcpSrv.Close)

	t.Run("post-write-goes-to-socket", func(t *testing.T) {
		sockPath, sockMethod, tcpPath = "", "", ""
		if _, err := httpPost(tcpSrv.URL+"/waired/v1/pause", nil); err != nil {
			t.Fatalf("httpPost pause: %v", err)
		}
		if sockPath != "/waired/v1/pause" || sockMethod != http.MethodPost {
			t.Fatalf("socket saw %s %q, want POST /waired/v1/pause", sockMethod, sockPath)
		}
		if tcpPath != "" {
			t.Fatalf("pause unexpectedly reached the TCP listener at %q", tcpPath)
		}
	})

	t.Run("delete-goes-to-socket", func(t *testing.T) {
		sockPath, sockMethod, tcpPath = "", "", ""
		if _, err := httpDelete(tcpSrv.URL + "/waired/v1/models/m1"); err != nil {
			t.Fatalf("httpDelete: %v", err)
		}
		if sockPath != "/waired/v1/models/m1" || sockMethod != http.MethodDelete {
			t.Fatalf("socket saw %s %q, want DELETE /waired/v1/models/m1", sockMethod, sockPath)
		}
		if tcpPath != "" {
			t.Fatalf("delete unexpectedly reached the TCP listener at %q", tcpPath)
		}
	})

	t.Run("ping-stays-on-tcp", func(t *testing.T) {
		sockPath, sockMethod, tcpPath = "", "", ""
		if _, err := httpPost(tcpSrv.URL+mgmtPingPath, []byte(`{"peer":"p"}`)); err != nil {
			t.Fatalf("httpPost ping: %v", err)
		}
		if tcpPath != mgmtPingPath {
			t.Fatalf("TCP saw %q, want %s", tcpPath, mgmtPingPath)
		}
		if sockPath != "" {
			t.Fatalf("ping unexpectedly reached the socket at %q", sockPath)
		}
	})
}

// TestMgmtReadsRouteToSocket asserts the waired#836 read routing: a read
// issued against the loopback base actually travels over the local IPC
// socket, because the daemon serves only the compatibility routes on the
// TCP port. A --mgmt the operator pointed elsewhere keeps its reads on
// TCP, and a missing socket falls back to TCP so a daemon that could not
// bind one is still reachable.
//
// TestMain clears mgmtWriteBase for the rest of the binary, so this test
// restores production routing explicitly.
func TestMgmtReadsRouteToSocket(t *testing.T) {
	prevWrite, prevBase := mgmtWriteBase, mgmtReadDefaultBase
	mgmtWriteBase = ipcclient.BaseURL
	t.Cleanup(func() { mgmtWriteBase, mgmtReadDefaultBase = prevWrite, prevBase })

	var sockPath, sockMethod, tcpPath string

	sock := filepath.Join(shortTempDir(t), "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	sockSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sockPath, sockMethod = r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{}`))
	}))
	_ = sockSrv.Listener.Close()
	sockSrv.Listener = ln
	sockSrv.Start()
	t.Cleanup(sockSrv.Close)
	t.Setenv(paths.MgmtSocketEnvOverride, sock)

	tcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tcpPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(tcpSrv.Close)

	t.Run("read-on-the-default-base-goes-to-socket", func(t *testing.T) {
		sockPath, sockMethod, tcpPath = "", "", ""
		mgmtReadDefaultBase = tcpSrv.URL
		if _, err := httpGet(tcpSrv.URL + "/waired/v1/identity"); err != nil {
			t.Fatalf("httpGet identity: %v", err)
		}
		if sockPath != "/waired/v1/identity" || sockMethod != http.MethodGet {
			t.Fatalf("socket saw %s %q, want GET /waired/v1/identity", sockMethod, sockPath)
		}
		if tcpPath != "" {
			t.Fatalf("identity unexpectedly reached the TCP listener at %q", tcpPath)
		}
	})

	t.Run("read-on-another-base-stays-on-tcp", func(t *testing.T) {
		sockPath, tcpPath = "", ""
		// A --mgmt the operator pointed somewhere else (scripts/dev/mock-mgmt
		// on another port) must not be redirected to this machine's socket.
		mgmtReadDefaultBase = "http://127.0.0.1:9476"
		if _, err := httpGet(tcpSrv.URL + "/waired/v1/identity"); err != nil {
			t.Fatalf("httpGet identity: %v", err)
		}
		if tcpPath != "/waired/v1/identity" {
			t.Fatalf("TCP saw %q, want /waired/v1/identity", tcpPath)
		}
		if sockPath != "" {
			t.Fatalf("a non-default base reached the socket at %q", sockPath)
		}
	})

	t.Run("no-socket-falls-back-to-tcp", func(t *testing.T) {
		sockPath, tcpPath = "", ""
		mgmtReadDefaultBase = tcpSrv.URL
		t.Setenv(paths.MgmtSocketEnvOverride, filepath.Join(shortTempDir(t), "absent.sock"))
		if _, err := httpGet(tcpSrv.URL + "/waired/v1/identity"); err != nil {
			t.Fatalf("httpGet identity with no socket: %v", err)
		}
		if tcpPath != "/waired/v1/identity" {
			t.Fatalf("fallback did not reach TCP; saw %q", tcpPath)
		}
	})
}
