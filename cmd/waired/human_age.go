package main

import (
	"fmt"
	"time"
)

// Coarse ages and counts for status output. They lived next to the /model
// picker row until waired-agent#1185 took the timestamp out of that row (the
// `modelPicker` lineup carries no fetch time, and the question it answered —
// "is this from this install or an old one" — is now answered by whether the
// lineup is ours at all). Several other status lines still ask them, so they
// moved here rather than staying in a file that no longer uses them.

// humanAge renders how long ago something happened, coarsely — the reader is
// deciding "is this recent", not measuring.
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

// plural renders a count with its unit, singular at one. The bands above all
// reach 1 — a 61-second-old reading is "1 minute", not "1 minutes" — and this
// line is read by people, not parsed.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
