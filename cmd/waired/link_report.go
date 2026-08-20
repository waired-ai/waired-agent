package main

import (
	"encoding/json"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// What `waired link` tells the local daemon about a repair
// (waired-agent#791).
//
// The coding-tools row can go red — a failed apply during `waired init`
// now reports `failed` instead of leaving the step absent — and the
// warning that accompanies it names `waired link --force all` as the fix.
// Until this, that command was the one recovery in the product that wrote
// no record of itself: it applies the files and says nothing to anybody,
// so the row stayed red until the next `waired init` or a service restart.
// The ruling it is catching up with is
// docs/decisions/20260802/1757-setup-integration-persisted-front-loaded.md
// — "the recovery for a failure is re-running the command, and that itself
// writes the record".

// linkReportTimeout bounds the report. Short on purpose: the daemon's work
// is one map update and one small file write, so nothing real is truncated
// by it, while the ordinary write timeout would show up as an unexplained
// pause at the end of a repair on a host whose daemon is down.
const linkReportTimeout = 3 * time.Second

// linkIntegrationReport decides what, if anything, to tell the daemon
// about a finished `waired link`. nil means say nothing.
//
// Three rules, and each has a failure it prevents:
//
//   - Only `all`. recordIntegrationsWritten replaces the record whole, so
//     a report naming one adapter SHRINKS a two-adapter record, and Covers
//     then fails against an instruction that still names both — a green row
//     turning red on the next restart. Worse, when an instruction exists
//     the daemon records the instruction rather than the report, so a
//     single-target run would persist a claim about a file it never opened.
//   - Only when every adapter was Applied. A `link all` without --force
//     leaves an undetected agent Skipped with no error at all, so "the
//     command exited 0" is not "both were written". That is also why the
//     documented recovery is `--force all`.
//   - Never a failure, and never an uninstall. A failure here adds nothing
//     in the case that matters (the row is already red, and the operator is
//     looking at the error) while inventing a way to turn a green row red
//     on a host whose coding tools are fine — the persisted-red the same
//     decision refuses. An unlink has no "undone" phase on this wire, and
//     shrinking the record is unlink's own job, not this row's.
func linkIntegrationReport(target string, uninstall bool, res *setup.IntegrationResult, applyErr error) *management.SetupExecutorRequest {
	if uninstall || applyErr != nil || res == nil {
		return nil
	}
	if target != "all" && target != "" {
		return nil
	}
	var targets []string
	for _, ar := range res.Agents {
		if ar.Err != nil || !ar.Applied || ar.Skipped {
			return nil
		}
		id := string(ar.Agent)
		if !signer.IsValidIntegrationTarget(id) {
			// A retired adapter still on this host is not something the
			// control plane accepts in a claim (waired-agent#333).
			continue
		}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return nil
	}
	return &management.SetupExecutorRequest{
		StepOnly:           true,
		Phase:              management.SetupExecutorPhaseDone,
		Step:               management.SetupStepIntegration,
		IntegrationTargets: targets,
	}
}

// reportLinkIntegrations posts the report and forgets about it.
//
// Silent by construction: `waired link` is a repair command and its job is
// the files, so a daemon that is stopped, too old to know this route, or
// unreachable from this session changes nothing about whether the repair
// worked. Nothing is printed and the exit code is untouched.
//
// Reachable from an ordinary user on all three platforms — the IPC socket
// is world-writable by design (internal/platform/localipc) and the Windows
// pipe grants interactive users — with one gap worth naming: a Windows
// NETWORK logon (SSH, WinRM) is outside the pipe's IU term, so a repair run
// that way reports nothing and the row waits for a local one.
func reportLinkIntegrations(mgmtURL string, req *management.SetupExecutorRequest) {
	if req == nil {
		return
	}
	body, err := json.Marshal(req)
	if err != nil {
		return
	}
	_, _ = httpPostWithin(mgmtURL+"/waired/v1/setup/executor", body, linkReportTimeout)
}
