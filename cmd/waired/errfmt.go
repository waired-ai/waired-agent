package main

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/platform/elevation"
)

// errAgentDown is the sentinel callers test with errors.Is to detect
// "the local daemon did not answer the dial" after wrapDaemonDialError.
var errAgentDown = errors.New("waired-agent is not running")

// agentDownError replaces the raw Go dial error ("dial tcp 127.0.0.1:9476:
// connect: connection refused") with an actionable message, while keeping
// the cause in the Unwrap chain so errors.Is(err, syscall.ECONNREFUSED)
// and isConnectionRefused keep working for the pause/resume and sharing
// desired-state fallbacks.
type agentDownError struct{ cause error }

func (e *agentDownError) Error() string {
	return "waired-agent is not running (start the service, or run `waired doctor` to see why)"
}

func (e *agentDownError) Unwrap() error { return e.cause }

func (e *agentDownError) Is(target error) bool { return target == errAgentDown }

// wrapDaemonDialError classifies a transport error from one of the
// local loopback daemons (management API :9476, gateways :9473/:9472).
// Connection-refused (and its stringified variants) becomes
// *agentDownError; anything else — timeouts, HTTP status errors — passes
// through unchanged. Only ever applied to loopback URLs, so it cannot
// misfire on a Control Plane dial.
func wrapDaemonDialError(err error) error {
	if err == nil {
		return nil
	}
	if isConnectionRefused(err) {
		return &agentDownError{cause: err}
	}
	return err
}

// elevationHint phrases the platform-appropriate re-run advice.
// cmdline is the suggested command ("waired status"); empty means the
// generic "re-run" phrasing.
func elevationHint(cmdline string) string {
	return elevationHintFor(runtime.GOOS, cmdline)
}

// elevationHintFor is the testable core of elevationHint. The wording
// lives in internal/platform/elevation so the daemon binary and engine
// runtime (which cannot import this package main) share it verbatim
// (waired#752).
func elevationHintFor(goos, cmdline string) string {
	return elevation.HintFor(goos, cmdline)
}

// elevatedCmdline renders a command the way the operator must invoke it to
// run elevated, for inline use in descriptive copy ("reverse with <x>",
// help listings). On Unix that is `sudo <cmd>`; on Windows there is no
// sudo — the command is shown bare and the surrounding copy carries the
// "run as Administrator" cue (a wrong `sudo` on Windows was waired#752).
// For a standalone re-run hint (a full sentence), use elevationHintFor.
func elevatedCmdline(goos, cmd string) string {
	if goos == "windows" {
		return cmd
	}
	return "sudo " + cmd
}

// permissionHintFor is the elevation hint for err, or "" when running elevated
// would not fix it: err is not a permission error, or this process already is
// elevated. On root or an elevated Windows token a permission error comes from
// something else — a file another program holds open past the replace retry
// on Windows, an immutable file on a Unix — and "run it elevated" is advice
// the person has already taken.
//
// errors.Is, never os.IsPermission: every writer here wraps its error, and
// os.IsPermission does not look through fmt.Errorf's wrapping
// (waired-agent#1409, #1419).
func permissionHintFor(goos string, elevated bool, err error, cmdline string) string {
	if elevated || !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	return elevationHintFor(goos, cmdline)
}

// friendlyError renders the final error text for main()'s "waired:"
// line: permission errors get the elevation hint appended so the user
// learns the fix, everything else prints unchanged.
func friendlyError(err error) string {
	return friendlyErrorFor(runtime.GOOS, isElevatedFn(), err)
}

// friendlyErrorFor is friendlyError with the OS and the elevation fact as
// arguments, so every combination is testable from any host.
func friendlyErrorFor(goos string, elevated bool, err error) string {
	if hint := permissionHintFor(goos, elevated, err, ""); hint != "" {
		return fmt.Sprintf("%v\n  (permission denied: %s)", err, hint)
	}
	return err.Error()
}
