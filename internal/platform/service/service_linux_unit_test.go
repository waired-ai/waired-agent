//go:build linux

package service

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func renderTestUnit() string {
	return renderSystemdUnit(Config{
		Binary:   "/usr/bin/waired-agent",
		StateDir: "/var/lib/waired",
	}, linuxServiceUser)
}

// PRODUCT CONTRACT (#347). A preferred-model switch restarts the daemon by
// exiting with code 17. Without both directives systemd treats that as a
// failure and drops the unit into failed state, so the model switch looks like
// a crash and the agent stays down until someone intervenes.
func TestRenderSystemdUnit_HonoursTheRestartRequestExitCode(t *testing.T) {
	unit := renderTestUnit()
	for _, want := range []string{
		fmt.Sprintf("SuccessExitStatus=%d", RestartRequestedExitCode),
		fmt.Sprintf("RestartForceExitStatus=%d", RestartRequestedExitCode),
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("rendered unit is missing %q:\n%s", want, unit)
		}
	}
}

// The .deb ships its own unit and its comment claims to match this renderer.
// It did not: the packaged unit carried the exit-17 directives and the
// rendered one did not, so a host set up by `waired-agent install` behaved
// differently from a host set up by apt. Pin the directives that decide
// restart behaviour on both sides rather than trusting the comment.
func TestRenderSystemdUnit_MatchesThePackagedUnitOnRestartPolicy(t *testing.T) {
	packaged, err := os.ReadFile("../../../packaging/systemd/waired-agent.service")
	if err != nil {
		t.Skipf("packaged unit not readable from here: %v", err)
	}
	rendered := renderTestUnit()

	for _, key := range []string{
		"Restart",
		"RestartSec",
		"SuccessExitStatus",
		"RestartForceExitStatus",
	} {
		gotPackaged := directive(string(packaged), key)
		gotRendered := directive(rendered, key)
		if gotPackaged == "" {
			t.Errorf("packaged unit has no %s= (did the restart policy move?)", key)
			continue
		}
		if gotRendered != gotPackaged {
			t.Errorf("%s: rendered %q, packaged %q — the two installs would behave differently",
				key, gotRendered, gotPackaged)
		}
	}
}

// directive returns the value of `key=` in a systemd unit, ignoring comments.
func directive(unit, key string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=(.*)$`)
	m := re.FindStringSubmatch(unit)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}
