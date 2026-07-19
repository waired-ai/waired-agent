//go:build linux || darwin

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

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

	sock := filepath.Join(t.TempDir(), "mgmt.sock")
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
