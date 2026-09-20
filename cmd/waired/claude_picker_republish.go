package main

// The second write of this user's /model rows (waired-agent#1454).
//
// Claude Code reads settings at startup and arms the watch that would notice a
// later write only a few seconds after that. The SessionStart hook writes at
// about 1.2 s, before the watch exists, so its rows first appear in the NEXT
// session: the launch that follows a peer switching model shows the old model
// name, and only the one after it is right. Measured on 2.1.267 on a real host
// and on 2.1.278 here, and recorded in docs/knowledges.
//
// So when the hook's write actually changed something, it starts this: a child
// it does not wait for, which writes the rows again once the watch is armed.
// The hook itself still returns in about a second, which is why neither the
// hook command in managed settings nor refreshHookTimeout has to change — a
// host that already carries the SessionStart entry gets this on a binary
// update alone, with no elevated `waired claude enable`.
//
// Everything here is best effort. When the watch arms later than measured, or
// the child cannot outlive its parent, the rows appear at the next launch —
// the behaviour before this existed.

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/platform/detach"
)

// pickerRepublishOffsets are when the child writes the rows again, measured
// from its own start — which is about 1.2 s into the session, so the writes
// land at roughly 9 s and 21 s.
//
// PIN: a record of measured behaviour, not a contract. Claude Code arms its
// settings watch 3-6 s after startup on 2.1.261 (2026-09-06) and 4-5 s on
// 2.1.278 (2026-09-20); writes before that are lost to the session, writes
// after it are picked up. 8 s clears the measured upper bound with room, and
// 20 s is the shape of the external write at 15 s that also landed, for a host
// slower than either that was measured. Two attempts rather than one because
// the cost of a miss is a stale row for a whole session and the cost of a
// spare attempt is one rewrite of bytes that are already there.
var pickerRepublishOffsets = []time.Duration{8 * time.Second, 20 * time.Second}

// pickerWriteOutcome is what a write of the rows actually did. Two bits
// rather than one because only one of them is worth a second write: the rows
// are read from settings, which Claude Code watches, while the retired cache
// is read once at startup and removing it mid-session reaches nobody.
type pickerWriteOutcome struct {
	// LineupChanged: the modelPicker key was rewritten.
	LineupChanged bool
	// CacheRemoved: the pre-#1185 discovery cache was taken away.
	CacheRemoved bool
}

// Changed reports whether the write touched anything, which is what decides
// the CLI's wording.
func (o pickerWriteOutcome) Changed() bool { return o.LineupChanged || o.CacheRemoved }

// pickerRepublishWanted is the whole scheduling rule, pure so every arm is
// table testable without a process (CLAUDE.md §Test discipline).
//
// fromManaged is load bearing rather than incidental: the elevated
// `waired claude enable` path also writes these rows, and there no session is
// watching the file — the operator is at a shell, and this process may be
// root. A child left behind by that path would be a stray process with
// nothing to tell.
func pickerRepublishWanted(fromManaged bool, out pickerWriteOutcome) bool {
	return fromManaged && out.LineupChanged
}

// pickerRepublishSleeps turns offsets-from-start into the waits between them,
// so the schedule can be asserted without living through it.
func pickerRepublishSleeps(offsets []time.Duration) []time.Duration {
	sleeps := make([]time.Duration, 0, len(offsets))
	var elapsed time.Duration
	for _, at := range offsets {
		d := at - elapsed
		if d < 0 {
			d = 0
		}
		sleeps = append(sleeps, d)
		elapsed += d
	}
	return sleeps
}

// runPickerRepublish is the child's whole body: wait, write what is on disk
// back, repeat. sleep is a parameter so the schedule is testable.
//
// It prints nothing on any path. The child has no terminal of its own, but
// the silence is the contract that matters: this code is one refactor away
// from being called in the hook's own process, where stdout is read as the
// user's session context.
func runPickerRepublish(home string, offsets []time.Duration, sleep func(time.Duration)) error {
	path := claudecode.SettingsPath(home)
	for _, d := range pickerRepublishSleeps(offsets) {
		sleep(d)
		// A failure here is not worth reporting and not worth stopping for:
		// the next attempt may find the file readable again, and when none
		// of them lands the rows are still right for the next session.
		_, _ = claudecode.RepublishPickerLineup(path)
	}
	return nil
}

// pickerRepublishArgv is what the child is started with. Its own function so
// the spawn test and the command wiring cannot drift apart.
func pickerRepublishArgv() []string { return []string{"claude", "_picker", "republish"} }

// pickerRepublishExeFn resolves this binary. A var so the spawn test can point
// it at the test binary.
var pickerRepublishExeFn = os.Executable

// spawnPickerRepublish re-execs this binary detached and returns without
// waiting for it.
//
// The standard streams go to the null device explicitly. Inheriting them is
// the failure this has to avoid: the child would hold the write end of the
// hook's stdout pipe, and Claude Code waits for that pipe to close before the
// session starts — a child that sleeps 20 s would stall every launch.
//
// The environment is passed through whole, unlike the sudo hop's
// scrubbedChildEnv. The child has to resolve the same HOME and the same
// CLAUDE_CONFIG_DIR the hook did, or it republishes a file Claude Code is not
// reading.
func spawnPickerRepublish() error {
	self, err := pickerRepublishExeFn()
	if err != nil {
		return fmt.Errorf("waired claude _picker: find this program: %w", err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("waired claude _picker: open %s: %w", os.DevNull, err)
	}
	defer null.Close()
	cmd := exec.Command(self, pickerRepublishArgv()...)
	cmd.Env = os.Environ()
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	return detach.Start(cmd)
}
