//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// tcpReadRoutes mirrors the daemon's own allow-list
// (internal/management/socket.go). Copied rather than imported because it
// is unexported there, and because a copy is what makes this a test of the
// CLI against the daemon's stated contract rather than against itself: if
// the daemon narrows the list, this fixture stays as the contract the CLI
// was written for until someone updates both.
var tcpReadRoutes = map[string]struct{}{
	"/waired/v1/status":             {},
	"/waired/v1/inference/status":   {},
	"/waired/v1/inference/runtimes": {},
	"/waired/v1/inference/catalog":  {},
	"/waired/v1/setup/state":        {},
}

// TestMgmtReadsSurviveTheTCPReadGuard is the regression test for #785.
//
// It is a record of the daemon's behaviour since waired#836, not a new
// contract: while the local IPC socket is bound, the loopback TCP port
// serves GETs only for the routes in tcpReadRoutes and answers 403 for
// every other read. Six CLI call sites built their own HTTP client and
// read three non-allow-listed routes over TCP, so they all failed in
// production — `waired peers list` and `waired worker set --pin=<name>`
// exited 1, `waired doctor` silently dropped two findings, `waired init`
// lost its daemon authority, and the Claude Code statusline rendered
// "agent down" on every turn of a healthy machine.
//
// The TCP server here is the negative control: it answers exactly as the
// daemon's readGuard does. A future read that goes straight to TCP fails
// this test by the symptom it actually produces, which a source scan for
// http.DefaultClient could not do — half the defective sites built
// &http.Client{...} instead.
//
// TestMain clears mgmtWriteBase for the rest of the binary, so this test
// restores production routing explicitly.
func TestMgmtReadsSurviveTheTCPReadGuard(t *testing.T) {
	prevWrite, prevBase := mgmtWriteBase, mgmtReadDefaultBase
	mgmtWriteBase = ipcclient.BaseURL
	t.Cleanup(func() { mgmtWriteBase, mgmtReadDefaultBase = prevWrite, prevBase })

	var sockPath, tcpPath string

	// The socket serves the full mux, exactly as socketHandler() does.
	sock := filepath.Join(shortTempDir(t), "mgmt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	sockSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sockPath = r.URL.Path
		writeMgmtReadFixture(w, r.URL.Path)
	}))
	_ = sockSrv.Listener.Close()
	sockSrv.Listener = ln
	sockSrv.Start()
	t.Cleanup(sockSrv.Close)
	t.Setenv(paths.MgmtSocketEnvOverride, sock)

	// The loopback port applies readGuard.
	tcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tcpPath = r.URL.Path
		if _, ok := tcpReadRoutes[r.URL.Path]; !ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error_code":"forbidden","message":"reads of this endpoint must use the local management socket, not the loopback TCP port"}`))
			return
		}
		writeMgmtReadFixture(w, r.URL.Path)
	}))
	t.Cleanup(tcpSrv.Close)

	reset := func() { sockPath, tcpPath = "", "" }

	t.Run("mesh-snapshot", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = tcpSrv.URL
		snap, err := fetchMeshSnapshot(meshAddrFromURL(tcpSrv.URL), 2*time.Second)
		if err != nil {
			t.Fatalf("fetchMeshSnapshot: %v", err)
		}
		if snap == nil || len(snap.Peers) != 1 {
			t.Fatalf("snapshot = %+v, want one peer", snap)
		}
		if sockPath != meshSnapshotPath {
			t.Fatalf("socket saw %q, want %s", sockPath, meshSnapshotPath)
		}
		if tcpPath != "" {
			t.Fatalf("mesh read reached the TCP listener at %q", tcpPath)
		}
	})

	t.Run("mesh-snapshot-with-caller-context", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = tcpSrv.URL
		if _, err := fetchMeshSnapshotCtx(context.Background(), tcpSrv.URL); err != nil {
			t.Fatalf("fetchMeshSnapshotCtx: %v", err)
		}
		if sockPath != meshSnapshotPath || tcpPath != "" {
			t.Fatalf("socket=%q tcp=%q, want the read on the socket only", sockPath, tcpPath)
		}
	})

	t.Run("daemon-identity", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = tcpSrv.URL
		// nil here is the defect: daemonIdentity maps every failure to
		// "no answer", so the 403 made it unable to ever reach the daemon.
		if v := daemonIdentity(tcpSrv.URL); v == nil || !v.Enrolled {
			t.Fatalf("daemonIdentity = %+v, want the daemon's enrolled answer", v)
		}
		if sockPath != identityPath || tcpPath != "" {
			t.Fatalf("socket=%q tcp=%q, want the read on the socket only", sockPath, tcpPath)
		}
	})

	t.Run("doctor-connection-finding", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = tcpSrv.URL
		got := connectionFinding(context.Background(), tcpSrv.URL)
		if got.Subject != "network connection" {
			t.Fatalf("finding = %+v, want the network-connection row", got)
		}
		if sockPath != identityPath || tcpPath != "" {
			t.Fatalf("socket=%q tcp=%q, want the read on the socket only", sockPath, tcpPath)
		}
	})

	t.Run("claude-statusline-route", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = tcpSrv.URL
		body, err := fastGet(claudeRouteURL(tcpSrv.URL), statuslineBudget)
		if err != nil {
			t.Fatalf("fastGet claude route: %v", err)
		}
		if len(body) == 0 {
			t.Fatal("fastGet returned an empty body")
		}
		if sockPath != claudeRoutePath || tcpPath != "" {
			t.Fatalf("socket=%q tcp=%q, want the read on the socket only", sockPath, tcpPath)
		}
	})

	// The allow-listed reads are the control: they were never broken, and
	// routing them through the same helper must not have moved them.
	t.Run("allow-listed-read-still-works", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = tcpSrv.URL
		if _, err := fastGet(mgmtURL(tcpSrv.URL, inferenceStatusPath), statuslineBudget); err != nil {
			t.Fatalf("fastGet inference status: %v", err)
		}
		if sockPath != inferenceStatusPath {
			t.Fatalf("socket saw %q, want %s", sockPath, inferenceStatusPath)
		}
	})

	// The security property: a --mgmt the operator pointed at some other
	// daemon must never be redirected to THIS machine's socket. Here that
	// means the read genuinely hits TCP and genuinely gets the guard's 403.
	t.Run("operator-supplied-base-stays-on-tcp", func(t *testing.T) {
		reset()
		mgmtReadDefaultBase = "http://127.0.0.1:9476"
		if _, err := fetchMeshSnapshot(meshAddrFromURL(tcpSrv.URL), 2*time.Second); err == nil {
			t.Fatal("a non-default base was served; want the TCP guard's 403")
		}
		if tcpPath != meshSnapshotPath {
			t.Fatalf("TCP saw %q, want %s", tcpPath, meshSnapshotPath)
		}
		if sockPath != "" {
			t.Fatalf("a non-default base reached the socket at %q", sockPath)
		}
	})
}

// writeMgmtReadFixture answers each route with the smallest body its
// caller actually parses, so a routing regression fails on the transport
// rather than on a decode error.
func writeMgmtReadFixture(w http.ResponseWriter, path string) {
	w.Header().Set("Content-Type", "application/json")
	switch path {
	case meshSnapshotPath:
		_, _ = w.Write([]byte(`{"peers":[{"device_id":"dev_1","device_name":"peer-a"}]}`))
	case identityPath:
		_ = json.NewEncoder(w).Encode(map[string]any{"enrolled": true, "active": true, "device_id": "dev_self"})
	case claudeRoutePath:
		_, _ = w.Write([]byte(`{"policy":{"main":"auto","sub":"same"}}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}
