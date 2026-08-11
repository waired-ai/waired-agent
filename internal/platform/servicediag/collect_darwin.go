//go:build darwin

package servicediag

import (
	"context"
	"os"
	"strings"
	"time"

	"os/exec"

	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// Check reads launchd's record of the agent job, plus the tail of its error
// log.
//
// `launchctl print system/<label>` is the post-mortem: it reports the job's
// state and the previous run's exit status. It needs root for the system
// domain, and a doctor run is often unprivileged — so a failure to read it is
// not an error, just an absence of evidence, and the error log alone can still
// carry the answer.
//
// `log show` is deliberately not used: scanning the unified log costs seconds
// to tens of seconds, which is too slow for a diagnostic that runs inside
// `waired doctor`'s normal output. The daemon's own stderr is already captured
// to /Library/Logs by the LaunchDaemon plist.
func Check(ctx context.Context, stateDir string) Result {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	events := launchctlPrint(ctx)
	running := property(events, "state") == "running"
	if !running {
		events = append(events, errorLogTail()...)
	}
	if len(events) == 0 {
		return Result{}
	}
	return Explain("darwin", running, events, stateDir)
}

// launchctlPrint parses the handful of `key = value` lines we care about out
// of launchctl's deeply indented report.
func launchctlPrint(ctx context.Context) []Event {
	out, err := exec.CommandContext(ctx, "/bin/launchctl", "print",
		"system/"+darwinLabel).Output()
	if err != nil {
		return nil
	}
	wanted := map[string]bool{
		"state":          true,
		"last exit code": true,
		"runs":           true,
		"pid":            true,
	}
	var events []Event
	for _, line := range strings.Split(string(out), "\n") {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if wanted[strings.ToLower(strings.TrimSpace(k))] {
			events = append(events, Event{Source: "launchd", Message: strings.TrimSpace(line)})
		}
	}
	return events
}

// darwinLabel is the LaunchDaemon job label. Kept in step with
// internal/platform/service by the agreement test in servicediag_test.go.
const darwinLabel = "com.waired.agent"

// errorLogTail quotes the last few lines the daemon wrote to stderr. The
// LaunchDaemon plist points StandardErrorPath here.
//
// The path comes from logrotate.AgentLogPath rather than a literal: it is
// the one definition of "where an operator reads the agent's log" that
// `waired logs`, the tray hint and this check all share (#636), so a
// change to the layout cannot leave one surface pointing somewhere the
// file no longer is.
func errorLogTail() []Event {
	path := logrotate.AgentLogPath("darwin", "")
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	var events []Event
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			events = append(events, Event{Source: "waired-agent.err.log", Message: line})
		}
	}
	return events
}

// keep the service import meaningful on darwin: the label above must name the
// same job the service package manages.
var _ = service.ServiceName
