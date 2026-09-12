package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// resolveSystemFallback answers a status query from the platform SYSTEM
// state dir when the caller's resolved (per-user) dir held no identity.
// It is the honest replacement for the old os.Stat guess: instead of
// inferring readability from a stat of identity.json, it actually loads the
// System dir and reports what happened. cmdline is the command to suggest
// re-running elevated (e.g. "waired status").
func resolveSystemFallback(resolvedDir, cmdline string, e daemonEnrolment) (string, *identity.Identity, string) {
	return resolveSystemFallbackAt(resolvedDir, paths.StateDir(paths.System), cmdline, runtime.GOOS, e)
}

// resolveSystemFallbackAt is the testable core of resolveSystemFallback. Its
// three outcomes map to the caller's three exit-0 branches:
//
//   - (sysDir, id, "")   the System dir is enrolled AND readable → the caller
//     renders status from sysDir. This is the elevated / admin case: on a
//     Windows service install the whole %ProgramData%\waired tree is locked to
//     SYSTEM+Administrators, and on Windows even an elevated `waired status`
//     resolves to the admin's empty %AppData% first, so this fallback is the
//     only way it sees the system-wide enrollment.
//   - ("", nil, notice)  the System dir can't be read without elevation →
//     the caller prints notice, whose wording depends on what the background
//     service said about the enrollment (e). Standard / basic-token users land
//     here (Windows DACL, or a 0700 root-owned dir on Unix).
//   - ("", nil, "")      no System-dir fallback applies (genuinely not
//     enrolled, the service says this computer is signed out, or resolvedDir
//     already IS the System dir) → the caller prints its plain "Not enrolled"
//     message.
func resolveSystemFallbackAt(resolvedDir, sysDir, cmdline, goos string, e daemonEnrolment) (string, *identity.Identity, string) {
	// A WAIRED_STATE_DIR override collapses every mode to the same dir, and
	// Unix root already defaults to the System dir — either way there is no
	// distinct System dir to fall back to. (The caller's explicit permission
	// short-circuit on resolvedDir covers an unreadable override.)
	if filepath.Clean(sysDir) == filepath.Clean(resolvedDir) {
		return "", nil, ""
	}
	id, err := identity.Load(sysDir)
	switch {
	case err == nil && id != nil:
		return sysDir, id, ""
	case errors.Is(err, fs.ErrPermission):
		// A System dir locked down (Windows service DACL →
		// SYSTEM+Administrators, or a 0700 root-owned dir on Unix). Go maps
		// Windows ERROR_ACCESS_DENIED to fs.ErrPermission, so this branch
		// matches on all three OSes. Report it; don't fail. WHAT it reports
		// comes from the background service, because the permission error
		// itself says nothing about whether anything is enrolled in there.
		notice, ok := systemStateNotice(sysDir, cmdline, goos, e)
		if !ok {
			return "", nil, ""
		}
		return "", nil, notice
	default:
		// identity.Load returns (nil, nil) when identity.json is absent —
		// genuinely not enrolled. Any other error fails open to the plain
		// "Not enrolled" message rather than inventing a new failure mode.
		return "", nil, ""
	}
}

// daemonEnrolment is what the local background service says about this
// computer, for the branches that cannot read the state dir themselves.
//
// The distinction exists because a permission error is not evidence of an
// enrollment. The system state dir is 0700 root (or SYSTEM+Administrators
// DACL'd) whether or not anything is enrolled inside it, so an unelevated
// caller gets fs.ErrPermission either way, and reading that as "enrolled"
// told a signed-out computer it was signed in (waired-agent#1272, measured
// on macOS right after `sudo waired logout`). It is the same misreading
// waired-agent#1005 fixed for doctor and waired-agent#1269 for the app:
// absent, unreadable and enrolled are three states, not two.
type daemonEnrolment int

const (
	enrolmentUnknown   daemonEnrolment = iota // the service did not answer
	enrolmentSignedIn                         // the service says it is enrolled
	enrolmentSignedOut                        // the service says it is not
)

// askDaemonEnrolment resolves the tri-state over the local management
// endpoint. That endpoint is the one channel an unelevated caller genuinely
// has — the unix socket is bound 0666 so a different-uid desktop user can
// reach it, and the Windows named pipe grants Interactive Users
// (internal/platform/localipc) — and `waired doctor` already reads the same
// view through it.
func askDaemonEnrolment(mgmt string) daemonEnrolment {
	view := daemonIdentity(mgmt)
	switch {
	case view == nil:
		return enrolmentUnknown
	case view.Enrolled:
		return enrolmentSignedIn
	default:
		return enrolmentSignedOut
	}
}

// systemStateNotice is the OS-aware wording for "this process cannot read the
// system state dir", told apart by what the background service says about the
// enrollment. Kept pure so it can be table-tested across goos values without a
// real locked directory or a real daemon.
//
// The second return says whether the caller still has something to print: a
// service that reports "not signed in" leaves nothing for this notice to say,
// and the caller's own plain message is the right answer.
func systemStateNotice(sysDir, cmdline, goos string, e daemonEnrolment) (string, bool) {
	switch e {
	case enrolmentSignedIn:
		return systemEnrolledElevationNotice(sysDir, cmdline, goos), true
	case enrolmentSignedOut:
		return "", false
	default:
		return fmt.Sprintf(
			"This computer's Waired state (%s) needs administrator rights to read, and the background "+
				"service isn't answering, so this can't tell whether the computer is signed in.\n%s.",
			sysDir, capitalize(elevationHintFor(goos, cmdline))), true
	}
}

// systemEnrolledElevationNotice is the OS-aware wording for "the computer is
// signed in system-wide and you need elevation to read its state". Kept pure
// so it can be table-tested across goos values without a real locked
// directory.
//
// Only reached once something has actually observed the enrollment — see
// daemonEnrolment. It used to be the answer to a permission error alone,
// which is the assertion waired-agent#1272 is about.
func systemEnrolledElevationNotice(sysDir, cmdline, goos string) string {
	return fmt.Sprintf(
		"This computer is signed in system-wide, but its state (%s) needs administrator rights to read.\n%s.",
		sysDir, capitalize(elevationHintFor(goos, cmdline)))
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// unreadableSystemStateNotice turns a permission-denied state read into
// the same informational answer an empty per-user dir gets (waired#751):
// a status query says what would make it readable and exits 0.
//
// It fires only for the System dir. An explicit --state-dir or
// $WAIRED_STATE_DIR that cannot be read is a genuine error — nothing
// about it is "enrolled system-wide", and claiming so would send the
// operator to elevate a prompt that would still fail.
//
// The case is reachable because "elevated" does not imply "can read the
// service's ACL'd tree": a filtered/basic token (runas /trustlevel) still
// reports TokenIsElevated, and since waired-agent#313 that is what picks
// the System dir.
func unreadableSystemStateNotice(stateDir, cmdline string, e daemonEnrolment) (string, bool) {
	return unreadableSystemStateNoticeAt(stateDir, paths.StateDir(paths.System), cmdline, runtime.GOOS, e)
}

func unreadableSystemStateNoticeAt(stateDir, sysDir, cmdline, goos string, e daemonEnrolment) (string, bool) {
	if filepath.Clean(stateDir) != filepath.Clean(sysDir) {
		return "", false
	}
	return systemStateNotice(sysDir, cmdline, goos, e)
}
