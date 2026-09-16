package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestHTTPGetAgentDown pins the daemon-down UX: a refused dial to the
// local management API must come back as the friendly agentDownError
// (no raw "dial tcp" text) while still satisfying isConnectionRefused,
// which the pause/resume desired-state fallback depends on.
func TestHTTPGetAgentDown(t *testing.T) {
	addr, err := newClosedTCPAddr()
	if err != nil {
		t.Fatalf("newClosedTCPAddr: %v", err)
	}
	_, err = httpGet("http://" + addr + "/waired/v1/status")
	if err == nil {
		t.Fatal("httpGet against a closed port succeeded")
	}
	if !errors.Is(err, errAgentDown) {
		t.Fatalf("err = %v, want errors.Is(err, errAgentDown)", err)
	}
	if strings.Contains(err.Error(), "dial tcp") {
		t.Errorf("friendly error still leaks the raw dial error: %v", err)
	}
	if !strings.Contains(err.Error(), "waired-agent is not running") {
		t.Errorf("err = %v, want the agent-down wording", err)
	}
	if !isConnectionRefused(err) {
		t.Errorf("isConnectionRefused(wrapped) = false, breaks the desired-state fallbacks")
	}
}

func TestWrapDaemonDialError(t *testing.T) {
	if wrapDaemonDialError(nil) != nil {
		t.Error("nil must pass through")
	}
	plain := errors.New("status 500: boom")
	if got := wrapDaemonDialError(plain); got != plain {
		t.Errorf("non-dial error must pass through unchanged, got %v", got)
	}
	// Stringified refusal (no Errno in the chain) must still classify,
	// and the wrapper must keep satisfying isConnectionRefused via the
	// errAgentDown sentinel.
	stringified := fmt.Errorf("Get \"http://127.0.0.1:9476\": connection refused")
	wrapped := wrapDaemonDialError(stringified)
	if !errors.Is(wrapped, errAgentDown) {
		t.Errorf("stringified refusal not classified: %v", wrapped)
	}
	if !isConnectionRefused(wrapped) {
		t.Error("isConnectionRefused(wrapped stringified) = false")
	}
}

// TestElevationHintFor locks the platform-appropriate re-run advice
// (waired#752): a `sudo`-phrased hint on Unix, an "elevated (Administrator)
// prompt" phrasing on Windows — with and without a specific command.
func TestElevationHintFor(t *testing.T) {
	cases := []struct {
		goos, cmdline, want string
	}{
		{"linux", "waired status", "run `sudo waired status`"},
		{"darwin", "waired status", "run `sudo waired status`"},
		{"windows", "waired status", "re-run `waired status` from an elevated (Administrator) prompt"},
		{"linux", "", "re-run with sudo"},
		{"windows", "", "re-run from an elevated (Administrator) prompt"},
	}
	for _, c := range cases {
		if got := elevationHintFor(c.goos, c.cmdline); got != c.want {
			t.Errorf("elevationHintFor(%q, %q) = %q, want %q", c.goos, c.cmdline, got, c.want)
		}
	}
}

// TestElevatedCmdline locks the inline elevated-command rendering
// (waired#752): `sudo <cmd>` on Unix, bare `<cmd>` on Windows (no sudo).
func TestElevatedCmdline(t *testing.T) {
	cases := []struct {
		goos, cmd, want string
	}{
		{"linux", "waired claude enable", "sudo waired claude enable"},
		{"darwin", "waired claude enable", "sudo waired claude enable"},
		{"windows", "waired claude enable", "waired claude enable"},
	}
	for _, c := range cases {
		if got := elevatedCmdline(c.goos, c.cmd); got != c.want {
			t.Errorf("elevatedCmdline(%q, %q) = %q, want %q", c.goos, c.cmd, got, c.want)
		}
	}
}

// friendlyErrorFor, not friendlyError: the elevation fact is an argument, so
// a CI container that runs the tests as root cannot flip the answer.
func TestFriendlyError(t *testing.T) {
	perm := fmt.Errorf("identity: read /var/lib/waired/identity.json: %w", fs.ErrPermission)
	got := friendlyErrorFor("linux", false, perm)
	if want := perm.Error() + "\n  (permission denied: re-run with sudo)"; got != want {
		t.Errorf("friendlyErrorFor(perm) = %q, want %q", got, want)
	}
	plain := errors.New("some other failure")
	if got := friendlyErrorFor("linux", false, plain); got != plain.Error() {
		t.Errorf("friendlyErrorFor(plain) = %q, want passthrough", got)
	}
	down := wrapDaemonDialError(fmt.Errorf("connection refused"))
	if got := friendlyErrorFor("linux", false, down); got != down.Error() {
		t.Errorf("friendlyErrorFor(agentDown) = %q, want the friendly line only", got)
	}
	// An elevated process is not told to elevate: on root or an elevated
	// token a permission error is something elevating does not fix
	// (waired-agent#1419).
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if got := friendlyErrorFor(goos, true, perm); got != perm.Error() {
			t.Errorf("%s elevated: friendlyErrorFor(perm) = %q, want the error alone", goos, got)
		}
	}
}

// TestPermissionHintFor pins the one question every elevation hint for a
// failed write now asks: would running elevated fix this? Only when the
// process is not elevated and the error is a permission error, however deeply
// it is wrapped. os.IsPermission answered it for `waired claude enable` and
// never saw through the wrapping, so the hint there never printed
// (waired-agent#1419).
//
// PIN: product contract, waired-agent#1419.
func TestPermissionHintFor(t *testing.T) {
	const cmd = "waired claude enable"
	pathErr := &os.PathError{Op: "createtemp", Path: "/etc/claude-code/managed-settings.json.tmp.*", Err: fs.ErrPermission}
	secretsErr := fmt.Errorf("secrets: create temp in %s: %w", "/etc/claude-code", pathErr)
	errs := []struct {
		name string
		err  error
		perm bool
	}{
		{"bare permission", fs.ErrPermission, true},
		{"a *PathError", pathErr, true},
		{"secrets.WriteFile's wrap", secretsErr, true},
		{"claudemanaged's wrap around it", fmt.Errorf("claudemanaged: write %s: %w", "/etc/claude-code/managed-settings.json", secretsErr), true},
		{"not a permission error", errors.New("claudemanaged: settings file is not readable JSON"), false},
		{"nil", nil, false},
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, elevated := range []bool{false, true} {
			for _, e := range errs {
				want := ""
				if e.perm && !elevated {
					want = elevationHintFor(goos, cmd)
				}
				if got := permissionHintFor(goos, elevated, e.err, cmd); got != want {
					t.Errorf("%s elevated=%v %s: permissionHintFor = %q, want %q", goos, elevated, e.name, got, want)
				}
			}
		}
	}
}
