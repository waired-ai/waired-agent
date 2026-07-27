package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Which enrollment journey `waired init` takes.
//
// Enrollment is daemon-owned: the running waired-agent performs it and the
// CLI drives its login API (the Tailscale model). `waired init` used to
// decide that from a single 1-second probe and, on any failure, silently
// run a second, local enrollment implementation instead. The two are not
// equivalent — agent capabilities (onboarding-v1/v2, public-share-v1) are
// declared only on the daemon's network-map poll, so a host whose service
// failed to start enrolled "successfully" and then dead-ended the browser
// setup permanently (#175). The fallback converted a loud, local, fixable
// failure into a silent, remote, unfixable one.
//
// So the probe no longer decides *which implementation runs*; it decides
// whether the agent is there at all. Local enrollment runs only when it is
// explicitly selected, and an agent that is not answering is an error.
// Tailscale has no fallback either: `tailscale up` fails when tailscaled
// is not answering, and even its container image starts tailscaled first
// and authenticates it with an auth key.

type enrollRoute int

const (
	// routeDaemon: a waired-agent is answering the Local Management API.
	// Drive its login endpoints; it owns enrollment, the state dir and the
	// live tunnel.
	routeDaemon enrollRoute = iota
	// routeLocal: local enrollment, explicitly selected. --bypass-mode and
	// --google-sa-login complete the login session programmatically and
	// re-auth refreshes tokens only; the daemon login implements neither
	// yet. Both move daemon-side in the follow-ups to #175, after which
	// this route (and the local enrollment implementation behind it) goes
	// away entirely.
	routeLocal
	// routeAgentDown: a service is registered but the management API never
	// answered inside the wait window. This is exactly the state the silent
	// fallback used to hide, so it fails loudly instead.
	routeAgentDown
	// routeAgentAbsent: no registered service and nothing answering — a
	// raw-binary dev host, or one where the install never completed.
	routeAgentAbsent
)

// enrollFacts is everything the route decision depends on, so the decision
// itself is a pure function that can be table-tested over every
// combination (CLAUDE.md §Test discipline).
type enrollFacts struct {
	// The three explicit selectors for local enrollment.
	bypassMode    bool
	googleSALogin bool
	renewing      bool
	// serviceInstalled reports whether an OS service is registered
	// (systemd unit / LaunchDaemon plist / SCM entry) — i.e. whether an
	// agent is *supposed* to be running here.
	serviceInstalled bool
}

// chooseEnrollRoute picks the journey. probe is invoked only when an
// explicit selector has not already settled the answer, so a bypass / SA /
// re-auth run never pays the wait window; it receives serviceInstalled
// because the probe waits longer for a service that is registered (and is
// therefore probably still starting).
func chooseEnrollRoute(f enrollFacts, probe func(serviceInstalled bool) bool) enrollRoute {
	if f.bypassMode || f.googleSALogin || f.renewing {
		return routeLocal
	}
	if probe(f.serviceInstalled) {
		return routeDaemon
	}
	if f.serviceInstalled {
		return routeAgentDown
	}
	return routeAgentAbsent
}

// daemonProbeWindow is how long waitForDaemonStartup waits for a
// *registered* service to answer, and daemonProbeInterval is how often it
// re-probes. The installers register and start the service immediately
// before invoking `waired init` (install.sh linux_service_up /
// darwin_register_agent, install.ps1 Ensure-AgentRunning), so a
// single-shot probe loses that start-up race on a slow host. Vars so tests
// can collapse the window.
var (
	daemonProbeWindow   = 20 * time.Second
	daemonProbeInterval = 500 * time.Millisecond
)

// waitForDaemonStartup reports whether a waired-agent is answering the
// Local Management API, giving a registered-but-still-starting service
// time to bind. With no registered service there is nothing to wait for:
// a raw-binary daemon either answers now or is absent, and making a dev
// host sit through the window would be pure delay.
func waitForDaemonStartup(mgmtURL string, serviceInstalled bool, out io.Writer) bool {
	if daemonReachable(mgmtURL) {
		return true
	}
	if !serviceInstalled {
		return false
	}
	// Said as soon as we know we are going to wait, rather than after a
	// silent delay — a quiet pause during first-run setup reads as a hang.
	_, _ = fmt.Fprintln(out, dim("Waiting for Waired's background service to start…"))
	return waitDaemonReachable(mgmtURL, daemonProbeWindow)
}

// waitDaemonReachable polls the Management API until it answers or the
// timeout elapses; returns true once reachable. Also used to give the
// just-started daemon a moment to bind before the model-pull and benchmark
// probes.
func waitDaemonReachable(mgmtURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if daemonReachable(mgmtURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(daemonProbeInterval)
	}
}

// daemonRequiredError renders the failure for the two routes that have no
// agent to talk to, and returns nil for the routes that do. goos and
// startHint are parameters rather than reads of runtime.GOOS /
// service.StartHint() so every platform's wording is testable from one
// host (CLAUDE.md §Cross-OS parity). The copy stays in plain English: a
// person reading it has no reason to know what a daemon is.
func daemonRequiredError(route enrollRoute, goos, startHint string) error {
	var b strings.Builder
	switch route {
	case routeAgentDown:
		b.WriteString("Waired's background service is installed but isn't responding, so sign-in can't continue.\n")
		b.WriteString("  Check what's wrong:  waired doctor\n")
		if startHint != "" {
			fmt.Fprintf(&b, "  Start it:            %s\n", startHint)
		}
	case routeAgentAbsent:
		b.WriteString("Waired isn't running in the background, so sign-in can't continue.\n")
		b.WriteString("  Start it:            waired-agent\n")
	default:
		return nil
	}
	fmt.Fprintf(&b, "  Then run again:      %s", elevatedCmdline(goos, "waired init"))
	return errors.New(b.String())
}
