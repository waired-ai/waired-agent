package main

import (
	"context"
	"strings"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/appcontrol"
)

// checkAppControl reads which of Waired's programs Windows is refusing.
// Callers that must not touch the host (tests) pass the zero
// appcontrol.Result instead, which produces no finding.
func checkAppControl(ctx context.Context) appcontrol.Result {
	return appcontrol.Check(ctx)
}

// appControlFinding maps that reading into a doctor finding. Pure, so the
// mapping is unit-tested without a Windows event log.
//
// This is a different question from serviceFindingFromResult's. That one asks
// why the service is down and says nothing while it is healthy; on the host in
// waired-agent#1217 the service was healthy for all 91 minutes while
// waired.exe was being refused 234 times.
//
// Warn, not Fail. There is nothing here for the user to repair — the file is
// unsigned and Windows changed its mind about it — and `waired doctor` exiting
// non-zero would say a machine is broken when the fix is to wait. The one
// thing it is NOT is silence, which is what the user got before.
//
// The irony is worth naming for the next reader: `waired doctor` is
// waired.exe, so on the day this finding is about waired.exe it cannot be
// printed at all. That is why the same reading also reaches the tray, which is
// a different file with its own verdict. This row is what a user sees
// afterwards, or when it is one of the other two programs.
func appControlFinding(r appcontrol.Result) integration.AuditFinding {
	// Refused with no entries is not a claim about anything — Explain never
	// builds one, but a finding whose Detail is empty prints as a bare subject
	// with a dash after it, which is worse than saying nothing.
	if r.Status != appcontrol.Refused || len(r.Refusals) == 0 {
		return integration.AuditFinding{}
	}
	detail := r.Cause()
	if h := r.Hint(); h != "" {
		detail += " " + h
	}
	if ev := appControlEvidence(r); ev != "" {
		detail += " [" + ev + "]"
	}
	return integration.AuditFinding{
		Status:  integration.StatusWarn,
		Subject: "Windows Application Control",
		Detail:  detail,
	}
}

// appControlEvidence is the one clause an operator can chase in Event Viewer:
// which processes were turned away, and whether the verdict came from a live
// reputation lookup or from this device's cache. Counts and times are already
// in the cause; repeating 234 timestamps is not evidence, it is a wall.
func appControlEvidence(r appcontrol.Result) string {
	var parts []string
	for _, x := range r.Refusals {
		clause := x.Program
		if len(x.Requesters) > 0 {
			clause += " requested by " + strings.Join(x.Requesters, ", ")
		}
		switch {
		case x.AnsweredFromCache:
			clause += "; answered from this device's cache"
		case x.AskedTheCloud:
			clause += "; a reputation lookup was made"
		}
		parts = append(parts, clause)
	}
	return strings.Join(parts, " | ")
}
