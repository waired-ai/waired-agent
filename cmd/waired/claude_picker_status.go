package main

import (
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// The /model picker cache is the only thing that puts the Waired entries in
// Claude Code's picker on a subscription-OAuth host: discovery is
// credential-gated and waired holds no credential, so the client reads
// whatever file is on disk (#332/#407). Until now `waired claude status` said
// nothing about it, so an empty picker had no diagnosis at all — the operator
// saw correct routing, a correct base URL, a correct window, and no entries.
//
// Three ways it goes wrong, and each needs a different next step:
//
//   - Absent. The most likely cause is not "enable never ran" but that it ran
//     as the wrong user: installer-driven init runs as a real root login with
//     no SUDO_USER, so there is no invoking user to hop to and the file lands
//     under root's home. The write is best-effort by contract, so the only
//     trace is a warning that scrolled past.
//   - Present but its baseUrl does not byte-match the live ANTHROPIC_BASE_URL.
//     The client compares those as strings and silently ignores the whole file
//     on any difference — a changed port, or a trailing slash.
//   - Present and current, in which case saying so is what stops the operator
//     looking here at all.

// claudePickerFacts is what the row is rendered from. A struct rather than
// arguments so the pure renderer below can be table-tested without a
// filesystem, and so "which home did we look in" is part of the fact rather
// than implied by the caller's cwd.
type claudePickerFacts struct {
	state       claudecode.GatewayCacheState
	readErr     error
	liveBaseURL string
	// viaSudo and sudoUser describe whose home was inspected, so an operator
	// running under sudo can tell a wrong-user cache from a missing one.
	viaSudo  bool
	sudoUser string
	now      time.Time
}

// claudePickerStatusRow renders the picker-cache line, or "" when there is
// nothing useful to say (no managed base URL configured, so the picker is not
// ours to explain).
func claudePickerStatusRow(f claudePickerFacts) string {
	if f.liveBaseURL == "" {
		return ""
	}
	who := ""
	if f.viaSudo && f.sudoUser != "" {
		who = fmt.Sprintf(" (user %s)", f.sudoUser)
	}
	switch {
	case f.readErr != nil:
		return fmt.Sprintf("/model picker:      UNREADABLE%s — %v\n", who, f.readErr)
	case !f.state.Present:
		return fmt.Sprintf("/model picker:      not written%s — %s\n                    run `waired claude enable` as the user who runs `claude`\n",
			who, f.state.Path)
	case f.state.BaseURL != f.liveBaseURL:
		// The client's own comparison, reproduced: it is exact-string, and a
		// mismatch is invisible from every other surface.
		return fmt.Sprintf("/model picker:      IGNORED BY CLAUDE CODE%s — cached baseUrl %q != %q\n                    %s\n",
			who, f.state.BaseURL, f.liveBaseURL, f.state.Path)
	default:
		return fmt.Sprintf("/model picker:      %d entries, written %s%s\n                    %s\n",
			len(f.state.Models), humanAge(f.now, f.state.FetchedAt), who, f.state.Path)
	}
}

// humanAge renders how long ago the cache was written, coarsely — the reader
// is deciding "is this from this install or an old one", not measuring.
func humanAge(now, then time.Time) string {
	if then.IsZero() || then.After(now) {
		return "at an unknown time"
	}
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

// plural renders a count with its unit, singular at one. The bands above
// all reach 1 — a 61-second-old reading is "1 minute", not "1 minutes" —
// and this line is read by people, not parsed.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
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
	st, err := claudecode.ReadGatewayCache(claudecode.ClaudeConfigDir(), home)
	fmt.Fprint(stdout, claudePickerStatusRow(claudePickerFacts{
		state:       st,
		readErr:     err,
		liveBaseURL: liveBaseURL,
		viaSudo:     viaSudo,
		sudoUser:    sudoUser,
		now:         time.Now(),
	}))
}
