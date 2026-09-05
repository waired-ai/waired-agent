package main

import (
	"github.com/waired-ai/waired-agent/internal/integration"
	notices "github.com/waired-ai/waired-agent/internal/notice"
)

// noticeFindings turns what the daemon is publishing into doctor rows
// (waired-agent#1205). This is one of the two surfaces that make a
// suggestion reachable on a computer with no tray.
//
// Warning-severity notices only. Doctor reports on the health of the
// setup, and "you could be running a better model" is not a fault in it;
// the information-severity ones show in `waired status` and the tray,
// which report what is going on rather than what is wrong. Rendering
// them here would have meant inventing a fifth status mark for a case
// none of doctor's four describes.
//
// Silent on a daemon that predates the route, and on any other read
// failure. Deliberately unlike probeObservability's StatusSkip: there
// the skipped row tells an operator to upgrade to get diagnostics back,
// whereas here "no notices" and "this daemon does not publish notices"
// have the same thing to show — nothing — and a row saying so on every
// pre-upgrade host would be noise.
//
// Notices never move the exit code: they are all warnings, and doctor
// counts only failures (countFails).
func noticeFindings(mgmtURL string) []integration.AuditFinding {
	ns, err := fetchNotices(mgmtURL)
	if err != nil || len(ns) == 0 {
		return nil
	}
	out := make([]integration.AuditFinding, 0, len(ns))
	for _, n := range ns {
		if n.Severity != notices.SeverityWarn || n.Title == "" {
			continue
		}
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: n.Title,
			Detail:  n.Text,
		})
	}
	return out
}
