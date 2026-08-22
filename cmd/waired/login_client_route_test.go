package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// This file covers the wiring #294 was about: the DECISION to route was
// never reached on the path every real install takes. planClaudeRoute is
// table-tested separately (init_route_claude_test.go); these tests drive
// the real runInitViaDaemon / applySetupIntegrations and assert the write
// seam is (or is not) reached, because a test of the decision alone is
// exactly what failed to catch the original defect — the decision was
// correct and simply never called.

// hermeticHome points HOME and the state dir at temp directories so the
// coding-agent integration (which is what sets the consent this routing
// decision reads) writes its skills and ledger under the test's own
// tree instead of the developer's home. Returns the state dir.
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("WAIRED_STATE_DIR", state)
	// A stray SUDO_USER (a developer running the suite under sudo) would
	// send the integration through the per-user hop and exec a real
	// `waired link`. Pin the in-process branch.
	t.Setenv("SUDO_USER", "")
	return state
}

// pinElevated fixes what the routing decision believes about elevation.
// Without it every unit test resolves to "needs elevation" and the branch
// that writes is unreachable.
func pinElevated(t *testing.T, elevated bool) {
	t.Helper()
	prev := isElevatedFn
	isElevatedFn = func() bool { return elevated }
	t.Cleanup(func() { isElevatedFn = prev })
}

// signedInDaemon is the smallest daemon that gets runInitViaDaemon to the
// post-login block: login starts already active, and inference reports
// disabled so the engine wait and the benchmark return immediately.
func signedInDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/login/start":
			_ = json.NewEncoder(w).Encode(management.LoginStatus{
				SessionID: "s1", Phase: management.LoginPhaseActive,
				AccountEmail: "user@example.com",
			})
		case "/waired/v1/status":
			_, _ = w.Write([]byte(`{}`))
		case "/waired/v1/inference/status":
			_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: "disabled"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// PRODUCT CONTRACT (#294): a daemon-path `waired init` that the operator
// consented to must leave Claude Code routed, and --skip-claude-route must
// be the thing that stops it. Before this fix the daemon path wrote no
// managed settings at all, so a CLI install finished unrouted and the flag
// opted out of something that was never going to happen.
func TestRunInitViaDaemon_RoutesClaudeUnlessOptedOut(t *testing.T) {
	for _, tc := range []struct {
		name            string
		skipClaudeRoute bool
		wantApplies     int
	}{
		{"consented, no opt-out -> routes", false, 1},
		{"--skip-claude-route -> leaves Claude on the Anthropic API", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
			shrinkSetupTimers(t)
			stateDir := hermeticHome(t)
			pinElevated(t, true)
			rec := stubApplyClaudeRoute(t, nil)
			srv := signedInDaemon(t)

			// Non-interactive: the integration consent resolves to Yes
			// without reading stdin, which is the installers' --yes path.
			out := captureStdout(t, func() {
				if err := runInitViaDaemon(daemonInitOpts{
					MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
					GatewayBaseURL:  "http://127.0.0.1:9473",
					StateDir:        stateDir,
					NoBrowser:       true,
					NonInteractive:  true,
					SkipClaudeRoute: tc.skipClaudeRoute,
				}); err != nil {
					t.Fatalf("runInitViaDaemon: %v", err)
				}
			})

			if rec.count() != tc.wantApplies {
				t.Fatalf("managed-settings write called %d times, want %d\n---\n%s",
					rec.count(), tc.wantApplies, out)
			}
			if tc.wantApplies > 0 {
				if rec.calls[0].StateDir != stateDir {
					t.Errorf("routed with StateDir %q, want %q", rec.calls[0].StateDir, stateDir)
				}
				// Non-interactive has nobody to answer the statusline's
				// ask-before-wrapping question.
				if rec.calls[0].AllowPrompt {
					t.Error("a non-interactive run must not allow the statusline prompt")
				}
			}

			// The summary must state where Claude Code's requests go —
			// both ways round.
			wantLine := "routed through Waired"
			if tc.skipClaudeRoute {
				wantLine = "still using the Anthropic API"
			}
			if !strings.Contains(out, wantLine) {
				t.Errorf("success box missing %q\n---\n%s", wantLine, out)
			}
		})
	}
}

// --skip-integration is the opt-out for the whole coding-agent
// integration, and routing is part of it. RECORD OF TODAY'S BEHAVIOUR
// made explicit: the standalone path gated routing on the same consent,
// and a host that declined the integration must not have its Claude Code
// reconfigured machine-wide.
func TestRunInitViaDaemon_SkipIntegrationLeavesRoutingAlone(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	stateDir := hermeticHome(t)
	pinElevated(t, true)
	rec := stubApplyClaudeRoute(t, nil)
	srv := signedInDaemon(t)

	captureStdout(t, func() {
		if err := runInitViaDaemon(daemonInitOpts{
			MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
			GatewayBaseURL:  "http://127.0.0.1:9473",
			StateDir:        stateDir,
			NoBrowser:       true,
			NonInteractive:  true,
			SkipIntegration: true,
		}); err != nil {
			t.Fatalf("runInitViaDaemon: %v", err)
		}
	})
	if rec.count() != 0 {
		t.Errorf("--skip-integration still wrote managed settings (%d times)", rec.count())
	}
}

// waired#749: a consented run that cannot write the machine-wide file
// must SAY so. The silent skip is what left the consent copy — which has
// just described a machine-wide change — looking like routing happened.
func TestRunInitViaDaemon_NonElevatedSaysItCannotRoute(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	stateDir := hermeticHome(t)
	pinElevated(t, false)
	rec := stubApplyClaudeRoute(t, nil)
	srv := signedInDaemon(t)

	out := captureStdout(t, func() {
		if err := runInitViaDaemon(daemonInitOpts{
			MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
			GatewayBaseURL: "http://127.0.0.1:9473",
			StateDir:       stateDir,
			NoBrowser:      true,
			NonInteractive: true,
		}); err != nil {
			t.Fatalf("runInitViaDaemon: %v", err)
		}
	})
	if rec.count() != 0 {
		t.Errorf("a non-elevated run must not attempt the write (called %d times)", rec.count())
	}
	if !strings.Contains(out, "needs elevation") {
		t.Errorf("expected the elevation hint, got:\n%s", out)
	}
}

// PRODUCT CONTRACT (waired#935 + #294): the wizard's claude-code toggle
// promises "Changes where Claude Code sends its requests for everyone on
// this computer". It must actually change it — the applier used to
// install per-user skills only, leaving that sentence unbacked. The other
// two targets write in the user's home and must never touch routing.
func TestApplySetupIntegrations_OnlyClaudeCodeRoutes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		targets         []string
		skipClaudeRoute bool
		wantApplies     int
	}{
		{"claude-code on -> routes", []string{signer.IntegrationClaudeCode}, false, 1},
		{"openclaw only -> no routing", []string{signer.IntegrationOpenClaw}, false, 0},
		{
			"claude-code alongside others -> routes once",
			[]string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw}, false, 1,
		},
		{
			// opencode writes a plugin in the user's home (waired-agent#982)
			// and, like openclaw, must never touch routing.
			"opencode only -> no routing",
			[]string{signer.IntegrationOpenCode}, false, 0,
		},
		{
			"all three -> routes once",
			[]string{signer.IntegrationClaudeCode, signer.IntegrationOpenCode, signer.IntegrationOpenClaw}, false, 1,
		},
		{
			// The command-line opt-out wins over the browser toggle: it is
			// the more explicit instruction, and the conservative one.
			"claude-code on but --skip-claude-route -> no routing",
			[]string{signer.IntegrationClaudeCode}, true, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := hermeticHome(t)
			pinElevated(t, true)
			rec := stubApplyClaudeRoute(t, nil)

			err := applySetupIntegrations(context.Background(), tc.targets, setupIntegrationOpts{
				GatewayBaseURL:  "http://127.0.0.1:9473",
				StateDir:        stateDir,
				SkipClaudeRoute: tc.skipClaudeRoute,
			}, io.Discard, io.Discard)
			if err != nil {
				t.Fatalf("applySetupIntegrations: %v", err)
			}
			if rec.count() != tc.wantApplies {
				t.Errorf("managed-settings write called %d times, want %d", rec.count(), tc.wantApplies)
			}
			if tc.wantApplies > 0 {
				// §4.2: the browser is driving, so nothing here may prompt.
				if rec.calls[0].AllowPrompt {
					t.Error("the wizard path must not allow a terminal prompt")
				}
				if rec.calls[0].StateDir != stateDir {
					t.Errorf("routed with StateDir %q, want %q", rec.calls[0].StateDir, stateDir)
				}
			}
		})
	}
}

// A non-elevated executor cannot write the machine-wide file, and that is
// not a failure of the wizard's integration step: the host can finish it
// later with `waired claude enable`. Failing the row would turn a
// recoverable gap into a red step on the setup page.
func TestApplySetupIntegrations_NonElevatedClaudeCodeIsNotAnError(t *testing.T) {
	stateDir := hermeticHome(t)
	pinElevated(t, false)
	rec := stubApplyClaudeRoute(t, nil)

	var out strings.Builder
	err := applySetupIntegrations(context.Background(),
		[]string{signer.IntegrationClaudeCode},
		setupIntegrationOpts{GatewayBaseURL: "http://127.0.0.1:9473", StateDir: stateDir},
		&out, io.Discard)
	if err != nil {
		t.Fatalf("a non-elevated claude-code target must not fail the step: %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("a non-elevated executor must not attempt the write (called %d times)", rec.count())
	}
	if !strings.Contains(out.String(), "needs elevation") {
		t.Errorf("expected the elevation hint, got:\n%s", out.String())
	}
}

// PRODUCT CONTRACT (waired-agent#796): the closing card reports whether Claude
// Code is routed on this machine — not whether THIS run performed the routing.
//
// The verdict used to be a local bool assigned only inside `if !setupActive`,
// so a browser-wizard install — the path every real install takes — closed by
// reporting Claude Code still on the Anthropic API over a machine its own
// wizard had just routed, while `waired claude status` seconds later reported
// it routed. The campaign saw it on all three OSes.
//
// What this pins is the structural property the fix installs: with the
// terminal's routing block skipped (asserted, not assumed), the card still
// answers from managed settings. --skip-claude-route is the cheap way to skip
// that block; the wizard's own path skips it for a different reason and is
// covered by TestApplySetupIntegrations_OnlyClaudeCodeRoutes.
func TestRunInitViaDaemon_CardReportsRoutingThisRunDidNotPerform(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	stateDir := hermeticHome(t)
	pinElevated(t, true)
	rec := stubApplyClaudeRoute(t, nil)
	srv := signedInDaemon(t)

	// What another surface — the browser wizard's applier, or an earlier
	// `waired claude enable` — left on disk. Written directly, because the
	// whole point is that this run's routing block does not run.
	baseURL, _ := claudeBaseURL(stateDir)
	path := claudemanaged.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"env":{"ANTHROPIC_BASE_URL":` + strconv.Quote(baseURL) + `}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	out := captureStdout(t, func() {
		if err := runInitViaDaemon(daemonInitOpts{
			MgmtURL: srv.URL, Control: "https://cp.example", DeviceName: "dev-1",
			GatewayBaseURL:  "http://127.0.0.1:9473",
			StateDir:        stateDir,
			NoBrowser:       true,
			NonInteractive:  true,
			SkipClaudeRoute: true,
		}); err != nil {
			t.Fatalf("runInitViaDaemon: %v", err)
		}
	})

	// Anti-vacuity: if this run had routed, the card would have been right for
	// the old reason and the test would prove nothing.
	if rec.count() != 0 {
		t.Fatalf("this run routed %d times; the scenario is a run that did not\n---\n%s",
			rec.count(), out)
	}
	if !strings.Contains(out, "routed through Waired") {
		t.Errorf("closing card denies routing this machine has\n---\n%s", out)
	}
	if strings.Contains(out, "still using the Anthropic API") {
		t.Errorf("closing card contradicts itself\n---\n%s", out)
	}
}
