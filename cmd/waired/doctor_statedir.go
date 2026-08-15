package main

import (
	"github.com/waired-ai/waired-agent/internal/integration"
)

// stateDirFinding reports the one disagreement neither of doctor's two
// sign-in checks can see on its own: the daemon says this device is
// enrolled, and the state dir it was enrolled into no longer holds an
// identity.
//
// waired-agent#800. After `rm -rf <state-dir>` under a running daemon the
// session keeps serving from the identity it holds in memory, so:
//
//   - signInFinding reads the disk, finds nothing, and stays SILENT — it
//     treats a missing identity as "not enrolled, covered elsewhere"
//   - connectionFinding reads the daemon, is told "enrolled and active",
//     and reports OK
//
// Between them the host looks fine while every model pull fails and
// `waired status` says "Not enrolled". Neither check is wrong about what
// it asked; the fault only exists in the gap between the two answers, so
// it needs a check that holds both.
func stateDirFinding(diskEnrolled, daemonAnswered, daemonEnrolled bool) integration.AuditFinding {
	if !daemonAnswered || !daemonEnrolled || diskEnrolled {
		return integration.AuditFinding{}
	}
	return integration.AuditFinding{
		Status:  integration.StatusFail,
		Subject: "state directory",
		Detail: "the background service is signed in but this device's identity is " +
			"missing from disk — the state directory was removed while the service " +
			"was running; run `waired init` to restore it",
	}
}
