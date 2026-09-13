package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/controlurl"
	"github.com/waired-ai/waired-agent/internal/management"
)

// PRODUCT CONTRACT — waired-agent#1343. The login request names a control
// plane only when somebody chose one. The daemon takes a request's control
// URL over its own setting, so an unelevated `waired init` that could not
// read agent.env and sent its built-in fallback signed a dev host in to
// production.
func TestControlToRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolved string
		src      controlurl.Source
		renewing bool
		want     string
	}{
		{"#1343: nothing this process can see, not enrolled", controlurl.Default, controlurl.SourceBuiltin, false, ""},
		{"enrolled: the control plane it is enrolled to", "https://app.dev.waired.net", controlurl.SourceBuiltin, true, "https://app.dev.waired.net"},
		{"--control or $WAIRED_CONTROL_URL", "https://cp.example", controlurl.SourceOperator, false, "https://cp.example"},
		{"an agent.env this process could read", "https://app.dev.waired.net", controlurl.SourceInstaller, false, "https://app.dev.waired.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controlToRequest(tc.resolved, tc.src, tc.renewing); got != tc.want {
				t.Errorf("controlToRequest(%q, %v, %v) = %q, want %q", tc.resolved, tc.src, tc.renewing, got, tc.want)
			}
		})
	}
}

// TestLoginControlLine: the daemon's control plane is the link's host, so
// it is what the line names; this process's own resolution is only for a
// daemon that does not say.
func TestLoginControlLine(t *testing.T) {
	for _, tc := range []struct {
		name, daemon, resolved string
		unknown                bool
		wantLine               string
		wantUnknown            bool
	}{
		{"daemon answers, this process guessed", "https://app.dev.waired.net", controlurl.Default, true, "https://app.dev.waired.net", false},
		{"daemon answers, this process disagrees", "https://app.dev.waired.net", "https://cp.example", false, "https://app.dev.waired.net", false},
		{"older daemon, this process knows", "", "https://cp.example", false, "https://cp.example", false},
		{"older daemon, this process guessed", "", controlurl.Default, true, controlurl.Default, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, unknown := loginControlLine(tc.daemon, tc.resolved, tc.unknown)
			if line != tc.wantLine || unknown != tc.wantUnknown {
				t.Errorf("loginControlLine = (%q, %v), want (%q, %v)", line, unknown, tc.wantLine, tc.wantUnknown)
			}
		})
	}
}

// fakeLoginDaemon records the login-start body and answers with start, then
// with an error on the first status poll so runInitViaDaemon returns.
type fakeLoginDaemon struct {
	start management.LoginStatus

	mu   sync.Mutex
	body []byte
}

func (d *fakeLoginDaemon) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/login/start":
			b, _ := io.ReadAll(r.Body)
			d.mu.Lock()
			d.body = b
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(d.start)
		case "/waired/v1/login/status":
			_ = json.NewEncoder(w).Encode(management.LoginStatus{
				SessionID: d.start.SessionID, Phase: management.LoginPhaseError, Error: "stop here",
			})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *fakeLoginDaemon) sentControlURL(t *testing.T) (string, bool) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.body == nil {
		t.Fatal("no login start request reached the daemon")
	}
	var raw map[string]any
	if err := json.Unmarshal(d.body, &raw); err != nil {
		t.Fatalf("decode login start body %s: %v", d.body, err)
	}
	v, ok := raw["control_url"].(string)
	return v, ok
}

// TestRunInitViaDaemon_PrintsTheDaemonsControlPlane: an unelevated run that
// could not read agent.env names the control plane the daemon reports —
// the link's own host — instead of "couldn't read", and sends none.
func TestRunInitViaDaemon_PrintsTheDaemonsControlPlane(t *testing.T) {
	stubOpener(t, nil)
	d := &fakeLoginDaemon{start: management.LoginStatus{
		SessionID: "s1", Phase: management.LoginPhaseLoggingIn,
		LoginURL: "https://app.dev.waired.net/login/ls_1", UserCode: "ABCD-1234",
		ControlURL: "https://app.dev.waired.net",
	}}
	srv := d.serve(t)
	out := captureStdout(t, func() {
		_ = runInitViaDaemon(daemonInitOpts{
			MgmtURL: srv.URL, Control: controlurl.Default, ControlUnknown: true,
			NoBrowser: true, NonInteractive: true, SkipIntegration: true,
		})
	})
	if !strings.Contains(out, "Control Plane: https://app.dev.waired.net") || strings.Contains(out, "couldn't read") {
		t.Errorf("control plane line: %q", out)
	}
	if v, ok := d.sentControlURL(t); ok {
		t.Errorf("login start sent control_url %q, want none", v)
	}
}

// TestRunInitBody_UnreadableAgentEnvLeavesTheControlPlaneToTheDaemon drives
// `waired init` itself: with agent.env unreadable and nothing else naming a
// control plane, the login request carries no control_url. A readable
// agent.env and an explicit --control are still sent.
func TestRunInitBody_UnreadableAgentEnvLeavesTheControlPlaneToTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name        string
		control     string
		agentEnv    string
		readable    bool
		wantSent    bool
		wantSentURL string
	}{
		{name: "#1343: agent.env unreadable", readable: false},
		{name: "agent.env readable", agentEnv: "https://app.dev.waired.net", readable: true, wantSent: true, wantSentURL: "https://app.dev.waired.net"},
		{name: "--control", control: "https://cp.example", readable: false, wantSent: true, wantSentURL: "https://cp.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubOpener(t, nil)
			t.Setenv(authKeyEnv, "")
			oldReadable, oldIdentity, oldInstalled, oldReachable := platformDefaultReadableFn, daemonIdentity, serviceInstalledFn, daemonReachable
			t.Cleanup(func() {
				platformDefaultReadableFn, daemonIdentity, serviceInstalledFn, daemonReachable = oldReadable, oldIdentity, oldInstalled, oldReachable
			})
			platformDefaultReadableFn = func() (string, bool) { return tc.agentEnv, tc.readable }
			daemonIdentity = func(string) *management.IdentityView { return nil }
			serviceInstalledFn = func() bool { return true }
			daemonReachable = func(string) bool { return true }

			d := &fakeLoginDaemon{start: management.LoginStatus{Phase: management.LoginPhaseUnenrolled}}
			srv := d.serve(t)
			_ = captureStdout(t, func() {
				_ = runInitBody(&initFlags{
					control: tc.control, mgmtURL: srv.URL, stateDir: t.TempDir(),
					noBrowser: true, nonInteractive: true, skipIntegration: true,
				})
			})
			v, ok := d.sentControlURL(t)
			if ok != tc.wantSent || v != tc.wantSentURL {
				t.Errorf("sent control_url = (%q, present %v), want (%q, present %v)", v, ok, tc.wantSentURL, tc.wantSent)
			}
		})
	}
}
