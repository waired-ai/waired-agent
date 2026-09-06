package main

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// stateDiskAnswer is what the disk said when doctor went looking for this
// device's identity. The three not-found shapes are different facts and
// only one of them is a fault (waired-agent#1005): before this split they
// were one bool, so a state dir that could not be read was reported as a
// state dir that had been emptied.
type stateDiskAnswer int

const (
	// diskHasIdentity: identity.json was read and holds an identity.
	diskHasIdentity stateDiskAnswer = iota
	// diskAbsent: the directory was readable and holds no identity. This
	// is the #800 fault when the daemon says the device is enrolled.
	diskAbsent
	// diskUnreadable: the directory doctor was told to read denied the
	// read. Nothing was observed, so nothing can be concluded.
	diskUnreadable
	// diskSystemWide: this caller's own state dir holds no identity, and
	// the system-wide one does but needs elevation to read — the ordinary
	// shape of a non-root run on a service install.
	diskSystemWide
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
//
// waired-agent#1005 narrowed it. "The disk holds no identity" was read off
// a single bool that a permission error also produced, so on every apt /
// service install a non-root run declared the identity gone and told the
// user to re-run `waired init` — the one move that would have overwritten
// a healthy enrollment. Only diskAbsent is the #800 fault; the two
// could-not-look answers report themselves as skips the way every other
// unreadable check does (unreadableFinding, #651), which keeps them out of
// the exit code.
//
// goos routes only the wording of the elevation hint, so the decision
// stays table-testable on all three platforms from any host.
func stateDirFinding(disk stateDiskAnswer, daemonAnswered, daemonEnrolled bool, sysDir, goos string) integration.AuditFinding {
	if !daemonAnswered || !daemonEnrolled || disk == diskHasIdentity {
		return integration.AuditFinding{}
	}
	switch disk {
	case diskUnreadable:
		// Same sentence unreadableFinding gives every other check that
		// could not read the state dir.
		return integration.AuditFinding{
			Status:  integration.StatusSkip,
			Subject: "state directory",
			Detail:  "needs administrator rights to check; " + elevationHintFor(goos, "waired doctor"),
		}
	case diskSystemWide:
		// The `waired status` answer for this host (waired#751), as a
		// doctor row: the identity is where it belongs, this caller just
		// cannot read it.
		return integration.AuditFinding{
			Status:  integration.StatusSkip,
			Subject: "state directory",
			Detail: fmt.Sprintf("this computer is signed in system-wide. Its state (%s) needs administrator rights to check; %s",
				sysDir, elevationHintFor(goos, "waired doctor")),
		}
	default:
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "state directory",
			Detail: "the background service is signed in but this computer's identity is " +
				"missing from disk. The state directory was removed while the service " +
				"was running. Run `waired init` to restore it",
		}
	}
}

// stateDiskAnswerHere is stateDiskAnswerFor against this platform's
// system-wide state dir. It lives here rather than at the call site
// because collectDoctorFindings binds `paths` to a local variable, and
// because it is the same shape resolveSystemFallback gives the status
// command.
func stateDiskAnswerHere(stateDir string) (stateDiskAnswer, string) {
	return stateDiskAnswerFor(stateDir, paths.StateDir(paths.System), runtime.GOOS)
}

// stateDiskAnswerFor classifies what the disk says about this device's
// identity, and names the system-wide state dir when that is where the
// answer came from.
//
// The order matters. A permission error on the dir doctor was told to read
// is its own answer and must not be retried elsewhere: an explicit
// --state-dir that cannot be read is not evidence about the system-wide
// one. Only a readable dir with nothing in it asks the second question,
// which is the same one `waired status` asks (resolveSystemFallbackAt,
// waired#751) — on a service install the per-user dir is legitimately
// empty and the identity lives in the system dir.
//
// sysDir and goos are parameters rather than reads of paths.StateDir and
// runtime.GOOS so the whole decision can be exercised from any host.
func stateDiskAnswerFor(stateDir, sysDir, goos string) (stateDiskAnswer, string) {
	id, err := identity.Load(stateDir)
	switch {
	case err == nil && id != nil:
		return diskHasIdentity, ""
	case errors.Is(err, fs.ErrPermission):
		return diskUnreadable, ""
	case err != nil:
		// Unreadable for some other reason — a malformed identity.json is
		// the likely one. Today's answer, unchanged: the daemon is signed
		// in and this disk cannot produce the identity it was enrolled
		// with, which is what the #800 row says.
		return diskAbsent, ""
	}
	switch _, sysID, notice := resolveSystemFallbackAt(stateDir, sysDir, "waired doctor", goos); {
	case sysID != nil:
		// Enrolled system-wide and readable from here (an elevated run on
		// Windows resolves to an empty %AppData% first). Nothing is
		// missing, so there is no gap to report.
		return diskHasIdentity, sysDir
	case notice != "":
		return diskSystemWide, sysDir
	}
	return diskAbsent, ""
}
