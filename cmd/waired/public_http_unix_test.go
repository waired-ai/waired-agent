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

// TestPublicPostJSON_RoutesThroughMgmtWriteRoute asserts that a Public
// Share WRITE issued against the loopback TCP URL actually travels over
// the local IPC socket (waired#838). The daemon's writeGuard 403s POSTs
// that arrive on the TCP port, so a verb that used a plain http.Client
// would fail in production while passing every httptest-based test.
//
// The cmd/waired TestMain (logout_test.go) clears mgmtWriteBase for the
// whole binary so httptest-addressed writes work; this test restores
// production routing explicitly, mirroring main_ipcwrite_unix_test.go.
func TestPublicPostJSON_RoutesThroughMgmtWriteRoute(t *testing.T) {
	prev := mgmtWriteBase
	mgmtWriteBase = ipcclient.BaseURL
	t.Cleanup(func() { mgmtWriteBase = prev })

	var sockPath, sockMethod, sockCT string

	sock := filepath.Join(t.TempDir(), "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	sockSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sockPath, sockMethod, sockCT = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"state":"public"}`))
	}))
	_ = sockSrv.Listener.Close()
	sockSrv.Listener = ln
	sockSrv.Start()
	t.Cleanup(sockSrv.Close)
	t.Setenv(paths.MgmtSocketEnvOverride, sock)

	tcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("write unexpectedly reached the TCP listener")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(tcpSrv.Close)

	var out struct {
		State string `json:"state"`
	}
	if err := publicPostJSON(tcpSrv.URL, "/waired/v1/public/share/enable", map[string]int{"max_clients": 0}, &out); err != nil {
		t.Fatalf("publicPostJSON: %v", err)
	}
	if sockPath != "/waired/v1/public/share/enable" || sockMethod != http.MethodPost {
		t.Fatalf("socket saw %s %q, want POST /waired/v1/public/share/enable", sockMethod, sockPath)
	}
	if sockCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", sockCT)
	}
	if out.State != "public" {
		t.Errorf("decoded State = %q, want public", out.State)
	}
}
