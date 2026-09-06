package main

import (
	"fmt"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// The Waired rows in Claude Code's /model picker come from the `modelPicker`
// key in the user's own settings.json (waired-agent#1185). Nothing else puts
// them there on a subscription-OAuth host: gateway model discovery is
// credential-gated and waired holds no credential (#332/#407/#488). So when
// the rows are missing, this row is the diagnosis — before it existed an
// empty picker had none at all, and the operator saw correct routing, a
// correct base URL, a correct window and no entries.
//
// Four ways it goes wrong, each needing a different next step:
//
//   - Absent. The most likely cause is not "enable never ran" but that it ran
//     as the wrong user: installer-driven init runs as a real root login with
//     no SUDO_USER, so there is no invoking user to hop to and the rows land
//     in root's settings. The write is best-effort by contract, so the only
//     trace is a warning that scrolled past.
//   - Foreign. Someone else's lineup is in that file. waired never replaces
//     it: Claude Code takes the whole lineup from the highest source that
//     sets the key and never merges two, so writing over it would delete it.
//   - Unreadable. The file is not JSON waired can parse, so the posture is
//     hands off rather than overwrite, and the operator has to look.
//   - Present and ours, in which case saying so is what stops the operator
//     looking here at all.

// claudePickerFacts is what the row is rendered from. A struct rather than
// arguments so the pure renderer below can be table-tested without a
// filesystem, and so "whose settings did we look at" is part of the fact
// rather than implied by the caller's cwd.
type claudePickerFacts struct {
	kind        claudecode.PickerLineupKind
	rows        int
	path        string
	liveBaseURL string
	// viaSudo and sudoUser describe whose home was inspected, so an operator
	// running under sudo can tell a wrong-user lineup from a missing one.
	viaSudo  bool
	sudoUser string
}

// claudePickerStatusRow renders the /model line, or "" when there is nothing
// useful to say (no managed base URL configured, so the picker is not ours to
// explain).
func claudePickerStatusRow(f claudePickerFacts) string {
	if f.liveBaseURL == "" {
		return ""
	}
	who := ""
	if f.viaSudo && f.sudoUser != "" {
		who = fmt.Sprintf(" (user %s)", f.sudoUser)
	}
	switch f.kind {
	case claudecode.PickerLineupUnreadable:
		return fmt.Sprintf("/model rows:        UNREADABLE%s — %s is not settings waired can read\n",
			who, f.path)
	case claudecode.PickerLineupForeign:
		return fmt.Sprintf("/model rows:        left alone%s. %s already lists its own rows\n",
			who, f.path)
	case claudecode.PickerLineupNone:
		return fmt.Sprintf("/model rows:        not written%s. %s\n                    run `waired claude enable` as the user who runs `claude`\n",
			who, f.path)
	default:
		return fmt.Sprintf("/model rows:        %s%s\n                    %s\n",
			plural(f.rows, "row"), who, f.path)
	}
}

// printClaudePickerStatus resolves the facts and prints the row. Reads the
// INVOKING user's home, not this process's: `waired claude status` is usually
// run under sudo, and looking in /root would report a healthy host as
// unconfigured.
func printClaudePickerStatus(liveBaseURL string) {
	home, viaSudo, sudoUser := invokerHome()
	if home == "" {
		return
	}
	path := claudecode.SettingsPath(home)
	kind, rows := claudecode.DetectPickerLineup(path)
	fmt.Fprint(stdout, claudePickerStatusRow(claudePickerFacts{
		kind:        kind,
		rows:        len(rows),
		path:        path,
		liveBaseURL: liveBaseURL,
		viaSudo:     viaSudo,
		sudoUser:    sudoUser,
	}))
}
