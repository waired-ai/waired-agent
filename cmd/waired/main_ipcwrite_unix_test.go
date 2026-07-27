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
