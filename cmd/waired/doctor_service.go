package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
)

// checkService probes the OS service manager once. Callers that must not touch
// the host (tests) pass the zero servicediag.Result instead, which produces no
// finding.
func checkService(ctx context.Context, stateDir string) servicediag.Result {
	return servicediag.Check(ctx, stateDir)
}

// serviceFindingFromResult maps a service post-mortem into a doctor finding.
// Pure, so the mapping is unit-tested without a service manager.
//
// Two deliberate silences:
//
//   - Unknown emits nothing. The live probe below it already reports the
//     daemon as unreachable; a second line saying "and I could not work out
//     why" adds noise to the output of someone who is already stuck.
//   - Healthy emits nothing when there is no history to report. `waired
//     doctor` already prints a ✓ for the management endpoint, and a second ✓
//     for the same fact reads as padding.
//
// A failure is Fail, not Warn: unlike the tray host, this is not an optional
// convenience — nothing about Waired works while the agent is down, and
// `waired doctor`'s exit code should say so.
func serviceFindingFromResult(r servicediag.Result) integration.AuditFinding {
	detail := r.Cause
	if r.Hint != "" {
		detail += " " + r.Hint
	}
	if r.Evidence != "" {
		detail += " [" + r.Evidence + "]"
	}

	switch r.Status {
	case servicediag.Failed:
		return integration.AuditFinding{
			Status: integration.StatusFail, Subject: "waired-agent service", Detail: detail,
		}
	case servicediag.Stopped:
		// Deliberately stopped is not a fault to fail the run over, but the
		// user asking why nothing works still needs to be told.
		return integration.AuditFinding{
			Status: integration.StatusWarn, Subject: "waired-agent service", Detail: detail,
		}
	case servicediag.Healthy:
		if r.Evidence == "" {
			return integration.AuditFinding{}
		}
		// Up now, but the history explains something the user may have had to
		// work around — e.g. a boot-time block they started past by hand.
		return integration.AuditFinding{
			Status: integration.StatusWarn, Subject: "waired-agent service", Detail: detail,
		}
	default: // servicediag.Unknown
		return integration.AuditFinding{}
	}
}
