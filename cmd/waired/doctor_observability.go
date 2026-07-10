package main

import (
	"context"
	"errors"
	"fmt"
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
func probeObservability(ctx context.Context, mgmtURL string) []integration.AuditFinding {
	state, err := observabilityclient.GetState(ctx, mgmtURL)
	if err != nil {
		if errors.Is(err, observabilityclient.ErrUnsupported) {
			return []integration.AuditFinding{{
				Status:  integration.StatusSkip,
				Subject: "observability",
				Detail:  "daemon predates Phase 9 — upgrade waired-agent for fallback diagnostics",
			}}
		}
		// Other errors stay silent: the /status probe in
		// collectDoctorFindings has already (or will shortly) report the
		// underlying daemon-unreachable condition with a better message.
		return nil
	}

	out := make([]integration.AuditFinding, 0, 3)
	out = append(out, engineFinding(state.Agent))
	out = append(out, meshFinding(state.Mesh))
	out = append(out, recentFallbacksFinding(ctx, mgmtURL))
	return out
}

func engineFinding(a management.AgentState) integration.AuditFinding {
	if a.Paused {
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "inference engine",
			Detail:  "paused — `waired resume` to restore overlay routing",
		}
	}
	if !a.EngineReady {
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "inference engine",
			Detail:  "not ready — local inference is offline; mesh peers and api.anthropic.com fallback will be used",
		}
	}
	// A ready engine with a version warning (bundled live != pin, or a
	// reuse engine below the supported floor) is still a warn: waired
	// is not in control of what answers requests.
	if a.EngineVersionWarning != "" {
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "inference engine",
			Detail:  a.EngineVersionWarning,
		}
	}
	// A ready engine whose serve tuning degraded (floored context
	// window, f16 KV fallback, spill, untunable reuse engine) still
	// serves — but slower or with less context than sized (#621).
	if a.EngineTuningWarning != "" {
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "inference engine",
			Detail:  a.EngineTuningWarning,
		}
	}
	cap := ""
	if a.CapacityTotal > 0 {
		cap = fmt.Sprintf(", %d/%d slots in use", a.CapacityUsed, a.CapacityTotal)
	}
	model := a.ModelID
	if model == "" {
		model = "(unknown)"
	}
	engine := ""
	if a.EngineMode != "" {
		engine = fmt.Sprintf(", engine=ollama %s %s", a.EngineVersion, a.EngineMode)
	}
	return integration.AuditFinding{
		Status:  integration.StatusOK,
		Subject: "inference engine",
		Detail:  fmt.Sprintf("ready (model=%s%s%s)", model, cap, engine),
	}
}

func meshFinding(m management.MeshState) integration.AuditFinding {
	enrolled, reachable, ready := m.PeersEnrolled, m.PeersReachable, m.PeersReady
	if enrolled == 0 {
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "mesh peers",
			Detail:  "no peers enrolled — solo deployment",
		}
	}
	switch {
	case reachable == 0:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "mesh peers",
			Detail:  fmt.Sprintf("0/%d reachable — check NAT traversal and relay connectivity", enrolled),
		}
	case ready < reachable:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "mesh peers",
			Detail:  fmt.Sprintf("%d/%d enrolled reachable, only %d ready for inference", reachable, enrolled, ready),
		}
	default:
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "mesh peers",
			Detail:  fmt.Sprintf("%d/%d reachable, %d ready", reachable, enrolled, ready),
		}
	}
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
			Detail:  "could not read /observability/events",
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
			Detail:  fmt.Sprintf("%d in last 10 min — occasional probe failures (typical for NAT mesh)", n),
		}
	default:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "recent fallbacks",
			Detail:  fmt.Sprintf("%d in last 10 min — investigate peer health (`waired-agent` journal)", n),
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
