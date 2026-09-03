package main

import (
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// claudeDoctor is the Claude Code half of one doctor run: the commands waired
// has on this machine, read once. Passed into collectDoctorFindings rather than
// probed there — like trayDoctor — so the files are read once per run and so
// tests can pass the zero value and stay independent of whatever Claude Code
// state the runner happens to have.
type claudeDoctor struct {
	// StatusLineKind / StatusLineCmd describe the invoking user's statusLine.
	StatusLineKind claudecode.StatusLineKind
	StatusLineCmd  string
}

// checkClaude reads managed settings and the invoking user's ~/.claude once.
// Best-effort throughout: doctor must still run for a user who has never
// enabled the integration, and every read here degrades to the zero value.
func checkClaude(homeDir string) claudeDoctor {
	var c claudeDoctor
	if homeDir != "" {
		if kind, cmd, err := claudecode.DetectStatusLine(homeDir); err == nil {
			c.StatusLineKind, c.StatusLineCmd = kind, cmd
		}
	}
	return c
}

// claudeCommandFindings reports whether the Claude Code entries waired wrote can
// actually be run by the shell Claude Code starts them with on goos.
//
// This check exists because `waired claude status` judged both by presence, and
// on Windows both were written as POSIX one-liners — installed, reported
// installed, and doing nothing on a host without Git Bash (waired-agent#787).
// Nothing rewrites those entries on its own, so a host that never re-runs
// `waired claude enable` keeps them indefinitely; saying so here is what makes
// that a one-command fix instead of a silence.
//
// Pure over (goos, claudeDoctor), and table-tested across all three OSes: the
// defect being reported is itself an OS-specific branch that a Linux-only CI
// would otherwise never execute.
//
// Severity is Warn, never Fail. `waired doctor`'s exit code is driven by
// StatusFail alone (see the hasFail handling in runDoctorBody), and what breaks
// here is the fallback notice and the status-line segment — Claude Code still
// routes through waired and still answers. Flipping the exit code would make
// every rc9 Windows host fail a check that costs it no inference. Same reading
// as trayFindingFromResult's "optional convenience is Warn".
//
// An entry waired never installed produces no finding at all (empty Subject,
// which collectDoctorFindings skips), so a machine that does not use Claude Code
// gains no rows.
func claudeCommandFindings(goos string, c claudeDoctor) []integration.AuditFinding {
	var out []integration.AuditFinding

	switch c.StatusLineKind {
	case claudecode.StatusLineOurs, claudecode.StatusLineWrapped:
		f := integration.AuditFinding{Status: integration.StatusOK,
			Subject: "claude-code statusline",
			Detail:  "installed, and this computer's shell can run it"}
		if !claudecode.StatusLineRunsOn(goos, c.StatusLineKind, c.StatusLineCmd) {
			f.Status = integration.StatusWarn
			f.Detail = "written for a Unix shell — Claude Code runs it here only when Git Bash is " +
				"installed, so the routing segment may stay blank; re-run " +
				"`waired claude statusline install` to rewrite it"
		}
		out = append(out, f)
	default:
		// None or Foreign: waired did not put it there. `waired claude status`
		// already reports both, and doctor does not grade a statusLine the user
		// owns.
	}
	return out
}
