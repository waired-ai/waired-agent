package main

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func applied(id integration.AgentID) integration.ApplyResult {
	return integration.ApplyResult{Agent: id, Applied: true}
}

func linkResult(rs ...integration.ApplyResult) *setup.IntegrationResult {
	return &setup.IntegrationResult{Agents: rs}
}

// What `waired link` is allowed to claim (waired-agent#791). Every "no"
// row below is a way the daemon would otherwise be told something the
// command did not do.
func TestLinkIntegrationReport(t *testing.T) {
	both := linkResult(applied(integration.AgentClaudeCode), applied(integration.AgentOpenClaw))

	tests := []struct {
		name      string
		target    string
		uninstall bool
		res       *setup.IntegrationResult
		applyErr  error
		want      []string // nil = no report
	}{
		{
			// The case this exists for: the row went red during init, the
			// operator ran the command the warning named, and it worked.
			name:   "link all with every adapter applied",
			target: "all",
			res:    both,
			want:   []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
		},
		{
			// `waired link all` without --force leaves an undetected agent
			// Skipped and returns no error at all, so exit 0 is not
			// evidence that both were written. Product contract: the
			// record is written whole, and a claim about a file nobody
			// opened is what
			// docs/decisions/20260802/1757-setup-integration-persisted-front-loaded.md
			// keeps out of it.
			name:   "one adapter skipped",
			target: "all",
			res: linkResult(applied(integration.AgentClaudeCode),
				integration.ApplyResult{Agent: integration.AgentOpenClaw, Skipped: true}),
		},
		{
			name:   "one adapter errored",
			target: "all",
			res: linkResult(applied(integration.AgentClaudeCode),
				integration.ApplyResult{Agent: integration.AgentOpenClaw, Err: errors.New("permission denied")}),
		},
		{
			// A failed repair says nothing rather than reporting failed.
			// Where it would matter the row is already red and the operator
			// is reading the error; where it would not, it invents a way to
			// redden a host whose coding tools are fine.
			name:     "the apply itself failed",
			target:   "all",
			res:      both,
			applyErr: errors.New("integration: claude-code: permission denied"),
		},
		{
			// A single target would SHRINK the record, because
			// recordIntegrationsWritten replaces it whole.
			name:   "a single target",
			target: "claude-code",
			res:    linkResult(applied(integration.AgentClaudeCode)),
		},
		{
			// Removing is not a §7 outcome, and shrinking the record is
			// unlink's own job.
			name:      "unlink all",
			target:    "all",
			uninstall: true,
			res:       both,
		},
		{
			name:      "unlink one",
			target:    "claude-code",
			uninstall: true,
			res:       linkResult(applied(integration.AgentClaudeCode)),
		},
		{
			name:   "nothing to report on",
			target: "all",
		},
		{
			// The default target spelling reaches runLinkWith as "" from
			// `waired link` with no argument.
			name:   "the empty target is all",
			target: "",
			res:    both,
			want:   []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := linkIntegrationReport(tc.target, tc.uninstall, tc.res, tc.applyErr)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("report = %+v, want none", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("no report for a clean `link all`")
			}
			// The shape is the safety argument: this post must be inert
			// for everything except the one step it is about.
			if !got.StepOnly {
				t.Error("step_only is false; this report would move the lease")
			}
			if got.Attached {
				t.Error("attached is true; this process holds no lease")
			}
			if got.Elevated {
				t.Error("elevated is true; a repair must not rewrite the daemon's elevation flag")
			}
			if got.Driver != "" {
				t.Errorf("driver = %q, want none — `link` drives no setup", got.Driver)
			}
			if got.Engine != "" {
				t.Errorf("engine = %q, want none", got.Engine)
			}
			if got.Step != management.SetupStepIntegration {
				t.Errorf("step = %q, want integration", got.Step)
			}
			if got.Phase != management.SetupExecutorPhaseDone {
				t.Errorf("phase = %q, want done", got.Phase)
			}
			if len(got.IntegrationTargets) != len(tc.want) {
				t.Fatalf("targets = %v, want %v", got.IntegrationTargets, tc.want)
			}
			for i, w := range tc.want {
				if got.IntegrationTargets[i] != w {
					t.Fatalf("targets = %v, want %v", got.IntegrationTargets, tc.want)
				}
			}
		})
	}
}

// The report reaches the daemon in the shape the decision above produced.
func TestReportLinkIntegrationsPostsTheStepOnlyReport(t *testing.T) {
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	req := linkIntegrationReport("all", false,
		linkResult(applied(integration.AgentClaudeCode), applied(integration.AgentOpenClaw)), nil)
	reportLinkIntegrations(srv.URL, req)

	noted := d.noted()
	if len(noted) != 1 {
		t.Fatalf("posted %d reports, want exactly 1", len(noted))
	}
	got := noted[0]
	if !got.StepOnly || got.Attached || got.Elevated {
		t.Errorf("posted %+v, want a detached step-only report", got)
	}
	if got.Step != management.SetupStepIntegration || got.Phase != management.SetupExecutorPhaseDone {
		t.Errorf("posted %+v, want the integration row done", got)
	}
	if len(got.IntegrationTargets) != 2 {
		t.Errorf("targets = %v, want both adapters", got.IntegrationTargets)
	}
}

// A repair is about the files. A daemon that is stopped, too old for this
// route, or unreachable from this session must not change what `waired
// link` prints or what it exits with.
func TestReportLinkIntegrationsIsSilentWhenTheDaemonCannotBeReached(t *testing.T) {
	shrinkSetupTimers(t)
	req := linkIntegrationReport("all", false,
		linkResult(applied(integration.AgentClaudeCode), applied(integration.AgentOpenClaw)), nil)

	t.Run("a daemon that refuses the write", func(t *testing.T) {
		d := &fakeSetupDaemon{postFails: true}
		reportLinkIntegrations(d.server(t).URL, req)
	})
	t.Run("a daemon without the route", func(t *testing.T) {
		d := &fakeSetupDaemon{notFound: true}
		reportLinkIntegrations(d.server(t).URL, req)
	})
	t.Run("nothing listening at all", func(t *testing.T) {
		reportLinkIntegrations("http://127.0.0.1:1", req)
	})
	t.Run("nothing to report", func(t *testing.T) {
		reportLinkIntegrations("http://127.0.0.1:1", nil)
	})
}
