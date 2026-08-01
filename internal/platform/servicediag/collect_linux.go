//go:build linux

package servicediag

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// Check reads systemd's post-mortem for the agent unit.
//
// `systemctl show` is the authoritative source and needs no privileges:
// ActiveState/SubState say where the unit is now, Result says how the last run
// ended, and NRestarts counts how often Restart=always has had to intervene —
// a nonzero count on a unit that is currently up is the signature of a
// crash-loop that has not settled.
//
// The journal is read only to quote a line back at the user. It is
// best-effort: on a unit whose logs are root-only, `journalctl` prints nothing
// useful for an unprivileged run, and the systemctl properties alone still
// produce a verdict.
func Check(ctx context.Context) Result {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return Result{} // no systemd: no unit, nothing to explain
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	events := showProperties(ctx)
	if len(events) == 0 {
		return Result{}
	}
	running := property(events, "ActiveState") == "active"
	if !running {
		events = append(events, journalErrors(ctx)...)
	}
	return Explain("linux", running, events)
}

func showProperties(ctx context.Context) []Event {
	out, err := exec.CommandContext(ctx, "systemctl", "show", service.ServiceName,
		"--property=ActiveState,SubState,Result,ExecMainStatus,NRestarts").Output()
	if err != nil {
		return nil
	}
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			events = append(events, Event{Source: "systemd", Message: line})
		}
	}
	return events
}

// journalErrors pulls this boot's error-level lines for the unit, newest last
// so the quoted line is the most recent failure.
func journalErrors(ctx context.Context) []Event {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "journalctl",
		"-u", service.ServiceName, "-b", "-p", "err", "-n", "5",
		"--no-pager", "-o", "cat").Output()
	if err != nil {
		return nil
	}
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			events = append(events, Event{Source: "journal", Message: line})
		}
	}
	return events
}
