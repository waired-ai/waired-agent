package main

import (
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// trayDoctor is the tray half of one doctor run: the finding to print, and
// what — if anything — the fix flow can do about it. Both come from a single
// probe because Check makes a D-Bus round trip and runDoctorBody needs the
// answer twice (#295).
type trayDoctor struct {
	Finding integration.AuditFinding
	Repair  trayhost.RepairAction
}

// checkTray probes the live session once. Callers that must not touch the host
// (tests) pass the zero trayDoctor instead, which prints no finding and offers
// no repair.
func checkTray() trayDoctor {
	r := trayhost.Check()
	return trayDoctor{Finding: trayFindingFromResult(r), Repair: trayhost.Plan(r)}
}

// trayFindingFromResult maps a trayhost probe into a doctor finding. Pure, so
// the mapping is unit-tested without a live session. NotApplicable returns the
// zero AuditFinding (empty Subject), which collectDoctorFindings skips — keeping
// doctor output clean on servers, macOS, and Windows.
//
// Severity is Warn, never Fail: the tray is an optional convenience, so a
// missing SNI host must not flip `waired doctor`'s non-zero exit code (see the
// hasFail handling in runDoctorBody). It being fixable does not change that:
// the fix prompt is offered for it, but the exit code stays 0.
func trayFindingFromResult(r trayhost.Result) integration.AuditFinding {
	switch r.Status {
	case trayhost.HostPresent:
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "system tray host",
			Detail:  "an SNI host is present; the waired-tray icon will render",
		}
	case trayhost.NoHost, trayhost.Unsupported:
		return integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "system tray host",
			Detail:  r.Hint,
		}
	default: // trayhost.NotApplicable
		return integration.AuditFinding{}
	}
}
