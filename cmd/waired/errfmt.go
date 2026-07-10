package main

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"
)

// errAgentDown is the sentinel callers test with errors.Is to detect
// "the local daemon did not answer the dial" after wrapDaemonDialError.
var errAgentDown = errors.New("waired-agent is not running")

// agentDownError replaces the raw Go dial error ("dial tcp 127.0.0.1:9476:
// connect: connection refused") with an actionable message, while keeping
// the cause in the Unwrap chain so errors.Is(err, syscall.ECONNREFUSED)
// and isConnectionRefused keep working for the pause/resume and
// inference-share desired-state fallbacks.
type agentDownError struct{ cause error }

func (e *agentDownError) Error() string {
	return "waired-agent is not running (start the service, or run `waired doctor` to diagnose)"
}

func (e *agentDownError) Unwrap() error { return e.cause }

func (e *agentDownError) Is(target error) bool { return target == errAgentDown }

// wrapDaemonDialError classifies a transport error from one of the
// local loopback daemons (management API :9476, gateways :9473/:9479).
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

// elevationHintFor is the testable core of elevationHint.
func elevationHintFor(goos, cmdline string) string {
	if goos == "windows" {
		if cmdline == "" {
			return "re-run from an elevated (Administrator) prompt"
		}
		return fmt.Sprintf("re-run `%s` from an elevated (Administrator) prompt", cmdline)
	}
	if cmdline == "" {
		return "re-run with sudo"
	}
	return fmt.Sprintf("run `sudo %s`", cmdline)
}

// friendlyError renders the final error text for main()'s "waired:"
// line: permission errors get the elevation hint appended so the user
// learns the fix, everything else prints unchanged.
func friendlyError(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Sprintf("%v\n  (permission denied — %s)", err, elevationHint(""))
	}
	return err.Error()
}
