package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A re-authenticating run and a stored coding-tools instruction
// (waired-agent#987).
//
// `waired init --force-reauth` on an enrolled device rotates credentials
// and re-runs the rest of the install conversation — the engine step, the
// host-speed measurement, the benchmark. The coding-tools step was the one
// piece it never reached: the wizard call is gated on a browser driving
// THIS run, and an instruction stored before the run began is stale by
// construction, so neither that call nor the terminal question below it
// applied it. The executor still attached and left, and the daemon reported
// the row failed/executor_gone with the plugin unwritten.
//
// Owner ruling waired-agent#599 (2026-08-09) is that a re-run on a
// configured host replays the whole conversation idempotently. These pin
// the coding-tools half of that, and the two limits on it: the daemon says
// whether anything is still unwritten, and --skip-integration still wins.

// reauthDaemon is a promptsDaemon carrying a stored instruction, in the
// shape a configured device reports: active (the control plane never
// clears desired state), stale (nobody watched it change), engine already
// installed.
func reauthDaemon(targets []string, pending bool) *promptsDaemon {
	t := targets
	return &promptsDaemon{
		statusSeq: []management.InferenceStatus{readyStatus()},
		setupState: management.SetupStateResponse{
			Active: true, DesiredStale: true,
			EngineInstalled: true, DesiredEngine: "ollama",
			Integrations:        &t,
			IntegrationsPending: pending,
		},
	}
}

// integrationReports returns the §7 coding-tools rows the run reported.
func integrationReports(d *promptsDaemon) []management.SetupExecutorRequest {
	var out []management.SetupExecutorRequest
	for _, r := range d.notedRequests() {
		if r.Step == management.SetupStepIntegration {
			out = append(out, r)
		}
	}
	return out
}

func TestRunInitViaDaemon_ReauthAppliesAnUnwrittenInstruction(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	shrinkLoginTimers(t, 20*time.Millisecond)
	hermeticHome(t)
	pinElevated(t, true)
	stubApplyClaudeRoute(t, nil)

	d := reauthDaemon([]string{signer.IntegrationOpenCode}, true)
	out := runDaemonInit(t, d.server(t).URL, scriptStdin(""), daemonInitScenario{
		reauth: true, authOnlyRefresh: true,
	})

	if !strings.Contains(out, "Setting up your coding tools") {
		t.Errorf("the stored instruction was not applied\n---\n%s", out)
	}
	// The device answered this in the browser; asking again in the
	// terminal would be asking a settled question.
	if strings.Contains(out, "Coding-agent integration") {
		t.Errorf("a re-auth run asked the terminal question\n---\n%s", out)
	}
	// The hint is the repair for a row nobody wrote. This run wrote it.
	if strings.Contains(out, "waired link <agent>") {
		t.Errorf("offered the repair hint for work this run performed\n---\n%s", out)
	}
	reports := integrationReports(d)
	if len(reports) == 0 {
		t.Fatalf("the coding-tools row was left with no author\n---\n%s", out)
	}
	last := reports[len(reports)-1]
	if last.Phase != management.SetupExecutorPhaseDone {
		t.Errorf("final coding-tools report phase = %q, want done (%+v)", last.Phase, last)
	}
	// The wizard-path report names no targets on purpose: the daemon
	// records the instruction it served, not a list the executor repeats
	// back (cmd/waired-agent/setup_desired.go, waired-agent#312).
	if len(last.IntegrationTargets) != 0 {
		t.Errorf("reported targets = %v, want none from this path", last.IntegrationTargets)
	}
}

// An instruction this device has already written is not pending, and a
// re-auth must leave it alone: re-applying would resurrect an integration
// the operator may since have removed with `waired unlink`, which does not
// clear the daemon's record.
func TestRunInitViaDaemon_ReauthLeavesAWrittenInstructionAlone(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	shrinkLoginTimers(t, 20*time.Millisecond)
	hermeticHome(t)
	pinElevated(t, true)
	stubApplyClaudeRoute(t, nil)

	d := reauthDaemon([]string{signer.IntegrationOpenCode}, false)
	out := runDaemonInit(t, d.server(t).URL, scriptStdin(""), daemonInitScenario{
		reauth: true, authOnlyRefresh: true,
	})

	if strings.Contains(out, "Setting up your coding tools") {
		t.Errorf("rewrote an instruction the device had already written\n---\n%s", out)
	}
	if got := integrationReports(d); len(got) != 0 {
		t.Errorf("reported the row again: %+v", got)
	}
	if !strings.Contains(out, "waired link <agent>") {
		t.Errorf("the run said nothing about how to (re)configure\n---\n%s", out)
	}
}

// --skip-integration is the operator saying "leave my coding tools alone",
// and it outranks a stored instruction. Without the distinction the flag
// would be silently overridden on exactly the runs it was passed for.
func TestRunInitViaDaemon_SkipIntegrationOutranksTheStoredInstruction(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	shrinkLoginTimers(t, 20*time.Millisecond)
	hermeticHome(t)
	pinElevated(t, true)
	stubApplyClaudeRoute(t, nil)

	d := reauthDaemon([]string{signer.IntegrationOpenCode}, true)
	out := runDaemonInit(t, d.server(t).URL, scriptStdin(""), daemonInitScenario{
		reauth: true, authOnlyRefresh: true, skipIntegration: true,
	})

	if strings.Contains(out, "Setting up your coding tools") {
		t.Errorf("--skip-integration did not stop the apply\n---\n%s", out)
	}
	if got := integrationReports(d); len(got) != 0 {
		t.Errorf("reported the row despite --skip-integration: %+v", got)
	}
	if !strings.Contains(out, "waired link <agent>") {
		t.Errorf("the run said nothing about how to (re)configure\n---\n%s", out)
	}
}
