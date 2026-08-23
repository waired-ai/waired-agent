package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
)

// daemonAnswering stands up a fake management API that publishes the
// integration expectation the real daemon serves, and points the CLI's read
// path at it. Returns its base URL.
func daemonAnswering(t *testing.T, expected string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/integration/openclaw" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"expected_base_url":"` + expected + `"}`))
	}))
	t.Cleanup(srv.Close)
	// Clearing the write base is how the rest of this package's tests keep
	// mgmtReadRoute on plain TCP so it can address an httptest server.
	restore := mgmtWriteBase
	mgmtWriteBase = ""
	t.Cleanup(func() { mgmtWriteBase = restore })
	return srv.URL
}

func writePinnedPort(t *testing.T, port int) string {
	t.Helper()
	dir := t.TempDir()
	body := []byte(`{"inference":{"local_gateway_port":` + strconv.Itoa(port) + `}}`)
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLocalGatewayBaseURL_PrefersTheDaemon is the regression guard for the
// half of waired-ai/waired-agent#999 that reading agent.json does not fix.
//
// Measured on a Linux service install: with inference.local_gateway_port
// pinned to 19473 the daemon listened on 19473, the desktop user could not
// read /var/lib/waired/agent.json at all, and `waired link` wrote 9473 —
// a plugin pointing at a port nothing was listening on, which Audit then
// compared against the same wrong constant and called healthy. The daemon
// answered 19473 correctly over its socket the whole time.
//
// So the state dir here deliberately says something ELSE: if the resolver
// reads the file in preference to the daemon, this fails.
func TestLocalGatewayBaseURL_PrefersTheDaemon(t *testing.T) {
	mgmt := daemonAnswering(t, "http://127.0.0.1:19473/v1")
	stateDir := writePinnedPort(t, 24680) // a wrong answer, on purpose

	got, port := localGatewayBaseURL(context.Background(), mgmt, stateDir)
	if want := "http://127.0.0.1:19473"; got != want {
		t.Errorf("localGatewayBaseURL = %q, want %q — the daemon is the only party that knows which port it bound", got, want)
	}
	if port != 19473 {
		t.Errorf("port = %d, want 19473", port)
	}
}

// TestLocalGatewayBaseURL_FallsBackToConfig keeps the second step honest:
// with no daemon answering, a pinned agent.json still reaches the plugins.
// This is the per-user install shape, where the CLI and the daemon share a
// state dir.
func TestLocalGatewayBaseURL_FallsBackToConfig(t *testing.T) {
	restore := mgmtWriteBase
	mgmtWriteBase = ""
	t.Cleanup(func() { mgmtWriteBase = restore })

	stateDir := writePinnedPort(t, 19473)
	got, port := localGatewayBaseURL(context.Background(), "http://127.0.0.1:1", stateDir)
	if want := "http://127.0.0.1:19473"; got != want {
		t.Errorf("localGatewayBaseURL = %q, want %q", got, want)
	}
	if port != 19473 {
		t.Errorf("port = %d, want 19473", port)
	}
}

// TestLocalGatewayBaseURL_FallsBackToTheDefault is the third step, and it
// keeps the two guards above from passing by always echoing their input.
func TestLocalGatewayBaseURL_FallsBackToTheDefault(t *testing.T) {
	restore := mgmtWriteBase
	mgmtWriteBase = ""
	t.Cleanup(func() { mgmtWriteBase = restore })

	got, port := localGatewayBaseURL(context.Background(), "http://127.0.0.1:1", t.TempDir())
	if got != defaultGatewayURL {
		t.Errorf("localGatewayBaseURL = %q, want the default %q", got, defaultGatewayURL)
	}
	if port != 9473 {
		t.Errorf("port = %d, want 9473", port)
	}
}

// TestResolveGatewayBaseURL_ExplicitFlagWins pins the precedence: someone
// who typed --gateway-base-url means it, even against a daemon that says
// otherwise. Without this, pointing the CLI at a tunnel or a second daemon
// would be silently overridden.
func TestResolveGatewayBaseURL_ExplicitFlagWins(t *testing.T) {
	mgmt := daemonAnswering(t, "http://127.0.0.1:19473/v1")

	cmd := &cobra.Command{Use: "x"}
	var flagVal string
	cmd.Flags().StringVar(&flagVal, "gateway-base-url", defaultGatewayURL, "")

	if got := resolveGatewayBaseURL(cmd, mgmt, t.TempDir(), flagVal); got != "http://127.0.0.1:19473" {
		t.Errorf("unset flag: got %q, want the daemon's answer", got)
	}
	if err := cmd.Flags().Set("gateway-base-url", "http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	if got := resolveGatewayBaseURL(cmd, mgmt, t.TempDir(), flagVal); got != "http://127.0.0.1:1234" {
		t.Errorf("explicit flag: got %q, want it honoured verbatim", got)
	}
}
