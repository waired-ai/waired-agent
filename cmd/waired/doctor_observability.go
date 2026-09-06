package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

// recentFallbackWindow is the cutoff doctor uses when counting
// kind=fallback events for the "recent fallbacks" finding. Kept in
// sync with the tray's RecentFallbackWindow for consistency.
const recentFallbackWindow = 10 * time.Minute

// localInferenceDisabled is AgentState.LocalInferenceState's "off"
// value. Spelled here rather than imported from internal/runtime/state
// so the CLI keeps reading the daemon's JSON as JSON — an older daemon
// sends an empty string, which matches nothing and leaves the
// pre-#465 behaviour exactly as it was.
const localInferenceDisabled = "disabled"

// probeObservability augments the doctor output with three findings
// derived from /waired/v1/observability/{state,events}. It deliberately
// emits no StatusFail — Phase 9 observability is operational signal,
// not configuration breakage, so the doctor's repair-flow does not
// trigger on these findings.
//
// Behaviour matrix:
//
//	endpoint 404         → single StatusSkip ("daemon predates Phase 9")
//	endpoint unreachable → no findings (the /status probe already
//	                       carries the "daemon down" message; emitting
//	                       a second one would be noise)
//	state OK             → three findings: engine, mesh, recent fallbacks
//
// The second return is the engine's repairability, from the SAME probe that
// produced the engine finding. Returned rather than probed again in
// runDoctorBody so the line the operator reads and the repair that follows it
// cannot describe two different moments (waired-agent#1170).
func probeObservability(ctx context.Context, mgmtURL string) ([]integration.AuditFinding, engineDoctor) {
	state, err := observabilityclient.GetState(ctx, mgmtURL)
	if err != nil {
		if errors.Is(err, observabilityclient.ErrUnsupported) {
			return []integration.AuditFinding{{
				Status:  integration.StatusSkip,
				Subject: "observability",
				Detail:  "this background service predates the observability API. Run `waired update` for fallback diagnostics",
			}}, engineDoctor{}
		}
		// Other errors stay silent: the /status probe in
		// collectDoctorFindings has already (or will shortly) report the
		// underlying daemon-unreachable condition with a better message.
		return nil, engineDoctor{}
	}

	out := make([]integration.AuditFinding, 0, 3)
	out = append(out, engineFinding(state.Agent))
	out = append(out, meshFinding(state.Mesh, probeMeshPeers(ctx, mgmtURL)))
	out = append(out, recentFallbacksFinding(ctx, mgmtURL))
	return out, engineRepair(state.Agent)
}

func engineFinding(a management.AgentState) integration.AuditFinding {
	if a.Paused {
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "inference engine",
			Detail:  "paused. `waired resume` routes through Waired again",
		}
	}
	// A setting is not a fault. Before this, a computer that was told not
	// to run AI models itself — the default on a host below the
	// recommended spec — read exactly like one whose engine had crashed,
	// and `waired doctor` was the command the failure copy pointed at
	// (#465). Ordered above EngineReady because it EXPLAINS the false
	// EngineReady rather than competing with it.
	if a.LocalInferenceState == localInferenceDisabled {
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "inference engine",
			Detail:  "off on this computer. Requests go to your other computers. `waired inference on` runs models here",
		}
	}
	if !a.EngineReady {
		// The reason first, because `waired doctor` is where every
		// failure message sends people and it used to arrive with
		// nothing to act on (waired-agent#1069). The daemon has had the
		// cause the whole time; this struct simply had no field for it.
		// Older daemons send none, and then this reads exactly as before.
		detail := "not ready"
		if a.EngineFailureReason != "" {
			detail += " — " + a.EngineFailureReason
		}
		detail += " — local inference is offline. Turns addressed to Waired go to another of your computers, and fail if none can answer"
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "inference engine",
			Detail:  detail,
		}
	}
	// The version warning and the tuning warning used to be the next two
	// links in this chain, and that was the defect: a chain returns one
	// finding, so a host with both was told about the version and never
	// learned about the tuning. They are published notices now and arrive
	// through noticeFindings as their own rows, two facts rather than a
	// choice between them (waired-agent#1229).
	//
	// The tuning one also arrives with a severity rather than as an
	// unconditional warning. The same field carries a context window
	// traded against decode speed on purpose, which this chain reported
	// as a fault on a computer that was working as designed.
	//
	// What is left here is engine STATE, which is what this row is for:
	// paused, off by choice, not ready, or ready.
	// Not a fraction — see the same note in cmd/waired/status_observability.go
	// (waired-agent#1126). CapacityTotal counts conversations kept warm,
	// CapacityUsed counts requests running.
	cap := ""
	if a.CapacityTotal > 0 {
		cap = fmt.Sprintf(", %d running, %d conversations kept warm", a.CapacityUsed, a.CapacityTotal)
	}
	model := a.ModelID
	if model == "" {
		model = "(unknown)"
	}
	// The engine is named from what the daemon reports, not from a
	// literal: this line said "ollama" on every host, including one
	// serving with vLLM (waired-agent#1076). An older daemon sends no
	// name, and then there is nothing trustworthy to print.
	engine := ""
	if a.EngineName != "" {
		engine = ", engine=" + a.EngineName
		if a.EngineVersion != "" {
			engine += " " + a.EngineVersion
		}
		if a.EngineMode != "" {
			engine += " " + a.EngineMode
		}
	}
	return integration.AuditFinding{
		Status:  integration.StatusOK,
		Subject: "inference engine",
		Detail:  fmt.Sprintf("ready (model=%s%s%s)", model, cap, engine),
	}
}

// meshFinding renders the mesh line from the published counts and,
// when the overlay could be measured, from what actually answered.
//
// probes is nil when no measurement was possible (no daemon, no mesh
// route, no reported-reachable peers) — then the published counts stand
// on their own, as they always did. See doctor_mesh_probe.go for why
// they cannot be trusted alone.
func meshFinding(m management.MeshState, probes []meshPeerProbe) integration.AuditFinding {
	enrolled, reachable, ready := m.PeersEnrolled, m.PeersReachable, m.PeersReady
	if enrolled == 0 {
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "mesh peers",
			Detail:  "no other computers on your network yet",
		}
	}

	// The measurement outranks the claim. A peer the control plane calls
	// reachable and the overlay cannot reach is the exact case
	// waired#1137 found, and reporting the higher number would be
	// reporting the wrong one.
	// StatusWarn, not StatusFail. probeObservability's contract is that
	// these findings never fail the run — they are operational signal,
	// not configuration breakage, and nothing the doctor's `f` repair
	// could fix (pinned by TestProbeObservability_NoFailNeverEmitsStatusFail).
	// A host whose local inference works is degraded by an unreachable
	// peer, not broken by it. What was wrong before was the WORDING, not
	// the severity: `✓ 2/3 reachable` on a host where nothing answered.
	if silent := silentPeers(probes); len(silent) > 0 {
		answered := len(probes) - len(silent)
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "mesh peers",
			Detail: fmt.Sprintf(
				"%d/%d reported reachable, but only %d answered a ping. "+
					"No reply from %s. Inference can't route to a computer that doesn't answer. "+
					"Check NAT traversal and relay connectivity",
				reachable, enrolled, answered, strings.Join(silent, ", ")),
		}
	}

	switch {
	case reachable == 0:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "mesh peers",
			Detail:  fmt.Sprintf("0/%d reachable. Check NAT traversal and relay connectivity", enrolled),
		}
	case ready < reachable:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "mesh peers",
			Detail: fmt.Sprintf("%d/%d enrolled reachable%s, only %d ready for inference",
				reachable, enrolled, measuredSuffix(probes), ready),
		}
	default:
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "mesh peers",
			Detail: fmt.Sprintf("%d/%d reachable%s, %d ready",
				reachable, enrolled, measuredSuffix(probes), ready),
		}
	}
}

// measuredSuffix marks a count the overlay confirmed, so the line never
// reads the same whether it was checked or merely reported. Empty when
// nothing was measured — an unqualified count is the old, second-hand
// meaning, and claiming otherwise would be the defect this fixes.
func measuredSuffix(probes []meshPeerProbe) string {
	if len(probes) == 0 {
		return ""
	}
	return " (measured)"
}

func recentFallbacksFinding(ctx context.Context, mgmtURL string) integration.AuditFinding {
	n := countRecentFallbacks(ctx, mgmtURL, recentFallbackWindow)
	switch {
	case n < 0:
		// /events round-trip failed but /state succeeded. Report a
		// soft skip rather than fabricating "none" — the missing data
		// is worth surfacing as inconclusive.
		return integration.AuditFinding{
			Status:  integration.StatusSkip,
			Subject: "recent fallbacks",
			Detail:  "couldn't read /observability/events",
		}
	case n == 0:
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "recent fallbacks",
			Detail:  "none in last 10 min",
		}
	case n <= 3:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "recent fallbacks",
			Detail:  fmt.Sprintf("%d in the last 10 min. Occasional probe failures are typical behind NAT", n),
		}
	default:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "recent fallbacks",
			Detail:  fmt.Sprintf("%d in the last 10 min. Check the other computers' health (`waired logs`)", n),
		}
	}
}

// countRecentFallbacks asks /events for the recent fallback batch and
// counts entries whose TS falls inside window. Returns -1 on transport
// error so the caller can render a "could not read" finding.
func countRecentFallbacks(ctx context.Context, mgmtURL string, window time.Duration) int {
	resp, err := observabilityclient.GetEvents(
		ctx,
		mgmtURL,
		0, // full ring window
		[]observability.Kind{observability.KindFallback},
		256,
	)
	if err != nil {
		return -1
	}
	cutoff := time.Now().Add(-window)
	n := 0
	for _, ev := range resp.Events {
		if ev.TS.After(cutoff) {
			n++
		}
	}
	return n
}
