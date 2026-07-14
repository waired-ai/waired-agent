package main

import (
	"errors"
	"fmt"
	"io/fs"
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

func TestFriendlyError(t *testing.T) {
	perm := fmt.Errorf("identity: read /var/lib/waired/identity.json: %w", fs.ErrPermission)
	got := friendlyError(perm)
	if !strings.Contains(got, "permission denied — ") {
		t.Errorf("friendlyError(perm) = %q, want elevation hint appended", got)
	}
	plain := errors.New("some other failure")
	if got := friendlyError(plain); got != plain.Error() {
		t.Errorf("friendlyError(plain) = %q, want passthrough", got)
	}
	down := wrapDaemonDialError(fmt.Errorf("connection refused"))
	if got := friendlyError(down); got != down.Error() {
		t.Errorf("friendlyError(agentDown) = %q, want the friendly line only", got)
	}
}
