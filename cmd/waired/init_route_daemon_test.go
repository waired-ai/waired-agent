package main

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// PRODUCT CONTRACT (#175). `waired init` used to choose between the
// daemon-driven login and a second, local enrollment implementation from a
// single 1-second probe, with no user-visible signal — so a host whose
// service failed to start enrolled "successfully" into a permanently
// dead-ended setup. These pin the replacement: local enrollment runs ONLY
// when explicitly selected, and an agent that is not answering is an error,
// never a silent downgrade.
func TestChooseEnrollRoute(t *testing.T) {
	tests := []struct {
		name       string
		facts      enrollFacts
		daemonUp   bool
		want       enrollRoute
		wantProbed bool
	}{
		{
			name:       "daemon answers, service registered",
			facts:      enrollFacts{serviceInstalled: true},
			daemonUp:   true,
			want:       routeDaemon,
			wantProbed: true,
		},
		{
			name:       "daemon answers, no registered service (raw-binary dev run)",
			facts:      enrollFacts{},
			daemonUp:   true,
			want:       routeDaemon,
			wantProbed: true,
		},
		{
			name:       "service registered but never answers: loud failure, not local enroll",
			facts:      enrollFacts{serviceInstalled: true},
			daemonUp:   false,
			want:       routeAgentDown,
			wantProbed: true,
		},
		{
			name:       "nothing registered and nothing answering",
			facts:      enrollFacts{},
			daemonUp:   false,
			want:       routeAgentAbsent,
			wantProbed: true,
		},
		{
			name:       "--bypass-mode selects local enrollment without probing",
			facts:      enrollFacts{bypassMode: true, serviceInstalled: true},
			daemonUp:   true,
			want:       routeLocal,
			wantProbed: false,
		},
		{
			name:       "--google-sa-login selects local enrollment without probing",
			facts:      enrollFacts{googleSALogin: true, serviceInstalled: true},
			daemonUp:   true,
			want:       routeLocal,
			wantProbed: false,
		},
		{
			name:       "re-auth selects local enrollment without probing",
			facts:      enrollFacts{renewing: true, serviceInstalled: true},
			daemonUp:   false,
			want:       routeLocal,
			wantProbed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probed := false
			var sawInstalled bool
			got := chooseEnrollRoute(tt.facts, func(serviceInstalled bool) bool {
				probed = true
				sawInstalled = serviceInstalled
				return tt.daemonUp
			})
			if got != tt.want {
				t.Errorf("chooseEnrollRoute() = %v, want %v", got, tt.want)
			}
			if probed != tt.wantProbed {
				t.Errorf("probe called = %v, want %v", probed, tt.wantProbed)
			}
			// The probe waits longer for a registered service, so it must
			// receive that fact verbatim rather than re-deriving it.
			if probed && sawInstalled != tt.facts.serviceInstalled {
				t.Errorf("probe got serviceInstalled=%v, want %v", sawInstalled, tt.facts.serviceInstalled)
			}
		})
	}
}

// PRODUCT CONTRACT (#175): the installers start the service immediately
// before running `waired init`, so a daemon that is still binding must not
// be misread as an absent one. The old probe was a single 1-second GET and
// lost that race outright.
func TestWaitForDaemonStartupOutwaitsAStartingService(t *testing.T) {
	restore := shrinkDaemonProbe(t)
	defer restore()

	var calls atomic.Int32
	swapDaemonReachable(t, func(string) bool { return calls.Add(1) >= 3 })

	var out bytes.Buffer
	if !waitForDaemonStartup("http://127.0.0.1:0", true, &out) {
		t.Fatal("waitForDaemonStartup should keep waiting for a registered service that is still starting")
	}
	if got := out.String(); !strings.Contains(got, "Waiting for Waired") {
		t.Errorf("expected a wait notice while the service starts, got %q", got)
	}
}

// PRODUCT CONTRACT (#175): with no registered service there is nothing to
// wait for — a raw-binary daemon either answers now or it is absent. The
// wait must not be paid on a dev host with no service at all.
func TestWaitForDaemonStartupDoesNotWaitWithoutAService(t *testing.T) {
	restore := shrinkDaemonProbe(t)
	defer restore()

	var calls atomic.Int32
	swapDaemonReachable(t, func(string) bool { calls.Add(1); return false })

	var out bytes.Buffer
	if waitForDaemonStartup("http://127.0.0.1:0", false, &out) {
		t.Fatal("waitForDaemonStartup should report the agent absent")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("probed %d times, want exactly 1 (no wait window without a service)", got)
	}
	if out.Len() != 0 {
		t.Errorf("expected no wait notice, got %q", out.String())
	}
}

// Record of today's behaviour: a daemon that answers on the first probe
// costs nothing and prints nothing, which is the healthy install.
func TestWaitForDaemonStartupIsSilentWhenTheDaemonAnswers(t *testing.T) {
	restore := shrinkDaemonProbe(t)
	defer restore()

	swapDaemonReachable(t, func(string) bool { return true })

	var out bytes.Buffer
	if !waitForDaemonStartup("http://127.0.0.1:0", true, &out) {
		t.Fatal("waitForDaemonStartup should report the daemon reachable")
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when the daemon answers immediately, got %q", out.String())
	}
}

// PRODUCT CONTRACT (#175): the failure has to say which of the two states
// it is (a service that will not answer vs no agent at all) and carry the
// platform-correct commands — the same shape Tailscale's
// fixTailscaledConnectError uses. Table over all three GOOS values per
// CLAUDE.md §Cross-OS parity.
func TestDaemonRequiredError(t *testing.T) {
	tests := []struct {
		name      string
		route     enrollRoute
		goos      string
		startHint string
		wantAll   []string
	}{
		{
			name:      "linux service down",
			route:     routeAgentDown,
			goos:      "linux",
			startHint: "sudo systemctl start waired-agent",
			wantAll:   []string{"isn't responding", "waired doctor", "sudo systemctl start waired-agent", "sudo waired init"},
		},
		{
			name:      "darwin service down",
			route:     routeAgentDown,
			goos:      "darwin",
			startHint: "sudo launchctl kickstart -k system/com.waired.agent",
			wantAll:   []string{"isn't responding", "waired doctor", "launchctl kickstart", "sudo waired init"},
		},
		{
			// No `sudo` on Windows — a wrong one was waired#752.
			name:      "windows service down",
			route:     routeAgentDown,
			goos:      "windows",
			startHint: "Start-Service waired-agent",
			wantAll:   []string{"isn't responding", "waired doctor", "Start-Service waired-agent", "waired init"},
		},
		{
			name:    "linux agent absent",
			route:   routeAgentAbsent,
			goos:    "linux",
			wantAll: []string{"isn't running in the background", "waired-agent", "sudo waired init"},
		},
		{
			name:    "darwin agent absent",
			route:   routeAgentAbsent,
			goos:    "darwin",
			wantAll: []string{"isn't running in the background", "waired-agent", "sudo waired init"},
		},
		{
			name:    "windows agent absent",
			route:   routeAgentAbsent,
			goos:    "windows",
			wantAll: []string{"isn't running in the background", "waired-agent", "waired init"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := daemonRequiredError(tt.route, tt.goos, tt.startHint)
			if err == nil {
				t.Fatal("daemonRequiredError returned nil for a failing route")
			}
			got := err.Error()
			for _, want := range tt.wantAll {
				if !strings.Contains(got, want) {
					t.Errorf("message missing %q:\n%s", want, got)
				}
			}
			if tt.goos == "windows" && strings.Contains(got, "sudo") {
				t.Errorf("windows message must not carry sudo (waired#752):\n%s", got)
			}
			// User-facing copy stays in plain English: "daemon" is an
			// internal word and must not leak into it.
			if strings.Contains(strings.ToLower(got), "daemon") {
				t.Errorf("message leaks the internal word %q:\n%s", "daemon", got)
			}
		})
	}
}

// The successful routes carry no error — the caller switches on the route
// and only asks for an error on the two failing ones.
func TestDaemonRequiredErrorNilForSuccessfulRoutes(t *testing.T) {
	for _, route := range []enrollRoute{routeDaemon, routeLocal} {
		if err := daemonRequiredError(route, "linux", "hint"); err != nil {
			t.Errorf("route %v: expected no error, got %v", route, err)
		}
	}
}

// shrinkDaemonProbe collapses the wait window so the tests above run in
// milliseconds instead of the production 20 seconds.
func shrinkDaemonProbe(t *testing.T) func() {
	t.Helper()
	oldWindow, oldInterval := daemonProbeWindow, daemonProbeInterval
	daemonProbeWindow, daemonProbeInterval = 2*time.Second, time.Millisecond
	return func() { daemonProbeWindow, daemonProbeInterval = oldWindow, oldInterval }
}

// swapDaemonReachable installs a stub probe for the duration of the test.
func swapDaemonReachable(t *testing.T, fn func(string) bool) {
	t.Helper()
	old := daemonReachable
	daemonReachable = fn
	t.Cleanup(func() { daemonReachable = old })
}
