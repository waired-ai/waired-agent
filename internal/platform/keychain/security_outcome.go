package keychain

import (
	"errors"
	"os/exec"
	"strings"
)

// security(1) reports what happened twice — once as an exit status, once
// as prose on stderr — and the two do not always agree. Reading only one
// of them is how waired-agent#799 shipped a log line that contradicted
// itself:
//
//	WARN securestore: failed to clear stale keychain entry after write failure
//	     err="keychain delete waired/gateway-token: exit status 195
//	          (stderr=\"password has been deleted.\n\")"
//
// stderr there says the item WAS deleted and carries no error text at
// all; only the status says the write never landed. Classifying on
// stderr alone — which is all this package did — cannot see that failure.
// The reverse case exists too: a refused write prints BOTH "Write
// permissions error." and "User interaction is not allowed.", and only
// the status says which one is the verdict.
//
// The status is the authority, so it is read first. security(1) returns
// the OSStatus truncated to eight bits, which is why these are the
// numbers a caller sees rather than the constants Apple documents.
const (
	// exitInteractionNotAllowed is errSecInteractionNotAllowed
	// (-25308): securityd has no session agent to put a prompt in
	// front of. This is what every keychain write from a non-Aqua
	// context gets — an SSH login, a LaunchDaemon, or the `waired
	// link` hop of `sudo waired init`.
	exitInteractionNotAllowed = 36
	// exitItemNotFound is errSecItemNotFound (-25300).
	exitItemNotFound = 44
	// exitDuplicateItem is errSecDuplicateItem (-25299).
	exitDuplicateItem = 45
	// exitWritePermissions is wrPermErr (-61). Observed on
	// delete-generic-password against a keychain the caller may read
	// but not modify.
	exitWritePermissions = 195
)

// outcome is what one security(1) invocation actually did. It exists so
// the decision is a pure function of the facts the process produced,
// testable on any OS — the classification used to live inside the
// darwin-tagged backend, where the Linux unit job never reached it
// (CLAUDE.md, "Test discipline").
type outcome int

const (
	// outcomeOK: the command succeeded.
	outcomeOK outcome = iota
	// outcomeNotFound: there is no such item. For Delete this is
	// success in every sense the caller cares about.
	outcomeNotFound
	// outcomeNoSession: the keychain is unreachable from this
	// process, not broken. Expected, not a fault of the host.
	outcomeNoSession
	// outcomeDuplicate: an item with this identity already exists.
	outcomeDuplicate
	// outcomeDenied: the keychain refused to be modified by this
	// caller. Unlike outcomeNoSession, a retry from an Aqua session
	// would not help.
	outcomeDenied
	// outcomeOther: something this build has no reading for. Report
	// it verbatim rather than guessing.
	outcomeOther
)

// classifySecurity reads the outcome from the two things security(1)
// produced. exitCode is -1 when the process never ran (see exitCodeOf),
// in which case only stderr — usually empty — is left to go on.
func classifySecurity(exitCode int, stderr []byte) outcome {
	if exitCode == 0 {
		return outcomeOK
	}
	switch exitCode {
	case exitItemNotFound:
		return outcomeNotFound
	case exitInteractionNotAllowed:
		return outcomeNoSession
	case exitDuplicateItem:
		return outcomeDuplicate
	case exitWritePermissions:
		return outcomeDenied
	}
	// Backstop for a status this build cannot read (including the -1
	// of a process that never started, and any future path where the
	// CLI reports through prose only). The phrases are the CLI's own,
	// which is why they are matched rather than the numeric codes:
	// security(1) does not always print the latter.
	s := string(stderr)
	switch {
	case strings.Contains(s, "could not be found in the keychain"), strings.Contains(s, "-25300"):
		return outcomeNotFound
	case strings.Contains(s, "User interaction is not allowed"), strings.Contains(s, "-25308"):
		return outcomeNoSession
	case strings.Contains(s, "already exists in the keychain"), strings.Contains(s, "-25299"):
		return outcomeDuplicate
	}
	return outcomeOther
}

// String is the short, plain-language half of what security(1) reported.
// It exists so a caller can log WHAT happened without pasting the CLI's
// own output, which in the delete case says the opposite of the status.
func (o outcome) String() string {
	switch o {
	case outcomeOK:
		return "succeeded"
	case outcomeNotFound:
		return "no such item"
	case outcomeNoSession:
		return "no session to unlock the keychain in"
	case outcomeDuplicate:
		return "an item with this identity already exists"
	case outcomeDenied:
		return "not permitted to modify the keychain"
	}
	return "unrecognised failure"
}

// exitCodeOf returns the process exit status behind err, or -1 when
// there is none — err is nil, or the command could not be started at
// all (a missing /usr/bin/security, a context cancellation), which is a
// different thing from a command that ran and refused.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
