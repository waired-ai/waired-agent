package main

import (
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// trayFindingFromResult maps a trayhost probe into a doctor finding. Pure, so
// the mapping is unit-tested without a live session. NotApplicable returns the
// zero AuditFinding (empty Subject), which collectDoctorFindings skips — keeping
// doctor output clean on servers, macOS, and Windows.
//
// Severity is Warn, never Fail: the tray is an optional convenience, so a
// missing SNI host must not flip `waired doctor`'s non-zero exit code (see the
// hasFail handling in runDoctorBody).
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
