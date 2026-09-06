package main

import (
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
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
	// RetiredStopHook is the machine-wide Stop-hook command left behind by a
	// build from before the fallback removal, "" when there is none.
	RetiredStopHook string
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
	c.RetiredStopHook = claudemanaged.StopHookCommandAt(claudemanaged.Path())
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
			f.Detail = "written for a Unix shell. Claude Code runs it here only when Git Bash is " +
				"installed, so the routing segment may stay blank. Run " +
				"`waired claude statusline install` again to rewrite it"
		}
		out = append(out, f)
	default:
		// None or Foreign: waired did not put it there. `waired claude status`
		// already reports both, and doctor does not grade a statusLine the user
		// owns.
	}

	// A Stop hook left behind by a build from before the fallback removal
	// (waired-agent#1184). Nothing writes it any more, and both the write and
	// the uninstall path strip it — but only when something rewrites the file,
	// and nothing does on its own: the one in-process writer that runs
	// afterwards, topUpClaudeWindow, edits a single env key rather than going
	// through WriteWithOptions. So a host that upgraded and never re-ran
	// `waired claude enable` keeps invoking `waired claude _fallback-hook` at
	// the end of every assistant turn, at a command that no longer exists.
	//
	// The visible cost is a line of stderr per turn. On a Windows host where
	// Application Control has turned against waired.exe it is also one more
	// refused launch per turn — which is half of what made waired-agent#1217
	// look like a storm, since the measured host was on a build that still
	// carried this hook and on current code the only per-turn caller left is
	// the status line.
	if c.RetiredStopHook != "" {
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusWarn,
			Subject: "claude-code stop hook",
			Detail: "a retired hook is still in machine-wide managed settings and runs " +
				"`waired claude _fallback-hook` after every turn, at a command that no longer " +
				"exists. Run `waired claude enable` again to remove it",
		})
	}
	return out
}
