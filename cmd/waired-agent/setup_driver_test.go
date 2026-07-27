package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Who is driving setup (waired-agent#198), the per-measurement benchmark
// wire (#199), and the coding-agent instruction (waired#935) — the three
// things the daemon reports that it could not before.

// --- driver (#198) ---

// Desired state is the browser's claim: the wizard has no lease to
// report through, and the write it made is already the evidence.
func TestSetupDriverDefaultsToBrowserWhenDesiredStateExists(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	p := r.snapshot(context.Background())
	if p.Driver != signer.SetupDriverBrowser {
		t.Fatalf("driver = %q, want browser", p.Driver)
	}
}

// A terminal takeover leaves no trace anywhere else: no desired state is
// written, so without this push the wizard sits on "waiting for this
// computer" until the setup window expires.
func TestSetupDriverTerminalPushesWithoutDesiredState(t *testing.T) {
	f := &fakeSetupProvider{}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	c := newFakeClock()
	r.now = c.now

	// Nothing at all yet: no instruction, no lease.
	if p := r.snapshot(context.Background()); p != nil {
		t.Fatalf("snapshot = %+v, want nil for a host with no onboarding activity", p)
	}

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Driver: signer.SetupDriverTerminal,
	})

	p := r.snapshot(context.Background())
	if p == nil {
		t.Fatal("snapshot = nil after a terminal takeover; the wizard would wait forever")
	}
	if p.Driver != signer.SetupDriverTerminal {
		t.Errorf("driver = %q, want terminal", p.Driver)
	}
	// Zero steps is the point: it keeps the control plane's completion
	// rule false and the device page's "setup unfinished" banner away,
	// while still saying who has the machine.
	if len(p.Steps) != 0 {
		t.Errorf("steps = %+v, want none — nothing was asked for", p.Steps)
	}
}

// The claim is bound to the lease. A latch that outlived its executor
// would have the wizard reporting a terminal that is not running, with
// no way back — which is the failure mode the lease TTL exists for.
func TestSetupDriverDiesWithTheLease(t *testing.T) {
	f := &fakeSetupProvider{}
	r, c := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Driver: signer.SetupDriverTerminal,
	})
	if got := r.snapshot(context.Background()).Driver; got != signer.SetupDriverTerminal {
		t.Fatalf("driver = %q, want terminal while the lease is live", got)
	}

	c.advance(setupExecutorTTL + time.Second)

	// Back to the browser, because desired state still exists — not to
	// "nobody", which would be a third state the wizard has no copy for.
	if got := r.snapshot(context.Background()).Driver; got != signer.SetupDriverBrowser {
		t.Fatalf("driver = %q after the lease expired, want browser", got)
	}
}

// A heartbeat carries no driver; it must not silently drop the claim.
func TestSetupDriverSurvivesAnEmptyHeartbeat(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Driver: signer.SetupDriverTerminal,
	})
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{Attached: true, Elevated: true})
	if got := r.snapshot(context.Background()).Driver; got != signer.SetupDriverTerminal {
		t.Fatalf("driver = %q after a bare heartbeat, want terminal", got)
	}
}

// --- benchmark trials (#199) ---

func TestSetupBenchmarkRunningCarriesTheTrials(t *testing.T) {
	f := &fakeSetupProvider{}
	f.bench = management.BenchmarkStatusResponse{
		State: management.BenchmarkStateRunning,
		Phase: "measuring", Trial: 2, Trials: 3,
		SampleTokps: 58.5, MedianTokps: 57.1, SpreadPct: 4.2,
		Method: signer.BenchmarkMethodOllamaEval,
	}
	r, _ := leasedReconciler(t, f, "", "")
	r.Apply(context.Background(), desiredFrame("", "", 1))

	p := r.snapshot(context.Background())
	if step := stepByID(t, p, setupStepBenchmark); step.Status != signer.SetupStatusRunning {
		t.Fatalf("benchmark step = %+v, want running", step)
	}
	b := p.Benchmark
	if b == nil {
		t.Fatal("no benchmark payload while running")
	}
	if b.Trial != 2 || b.Trials != 3 {
		t.Errorf("trial/trials = %d/%d, want 2/3", b.Trial, b.Trials)
	}
	if b.MedianTokps != 57.1 || b.SampleTokps != 58.5 {
		t.Errorf("median/sample = %v/%v, want the running figures", b.MedianTokps, b.SampleTokps)
	}
	if b.Method != signer.BenchmarkMethodOllamaEval {
		t.Errorf("method = %q, want ollama_eval", b.Method)
	}
	// The contract that keeps shipped wizards honest: measured_tokps is
	// the FINAL answer, rendered as "Speed: about N". A running median
	// there would present a provisional figure as a settled one.
	if b.MeasuredTokps != 0 {
		t.Errorf("measured_tokps = %v while running, want absent", b.MeasuredTokps)
	}
}

// Warm-up can take ~180 s on a cold multi-GB model and is not a
// measurement. The wire says so with Trials set and Trial still 0 — no
// phase field needed, which is why #199 required no proto change.
func TestSetupBenchmarkWarmupIsTrialZero(t *testing.T) {
	f := &fakeSetupProvider{}
	f.bench = management.BenchmarkStatusResponse{
		State: management.BenchmarkStateRunning, Phase: "warmup", Trials: 3,
	}
	r, _ := leasedReconciler(t, f, "", "")
	r.Apply(context.Background(), desiredFrame("", "", 1))

	b := r.snapshot(context.Background()).Benchmark
	if b == nil || b.Trials != 3 || b.Trial != 0 {
		t.Fatalf("benchmark = %+v, want trials=3 with no trial yet", b)
	}
}

func TestSetupBenchmarkDoneKeepsTheFinalFigure(t *testing.T) {
	f := &fakeSetupProvider{}
	f.bench = management.BenchmarkStatusResponse{
		State: management.BenchmarkStateDone, Gen: 1, MeasuredTokps: 57.1,
		Trials: 3, SpreadPct: 4.2, Method: signer.BenchmarkMethodOpenAISlope,
	}
	r, _ := leasedReconciler(t, f, "", "")
	r.Apply(context.Background(), desiredFrame("", "", 1))

	b := r.snapshot(context.Background()).Benchmark
	if b == nil || b.MeasuredTokps != 57.1 {
		t.Fatalf("benchmark = %+v, want the final measurement", b)
	}
	if b.Method != signer.BenchmarkMethodOpenAISlope || b.SpreadPct != 4.2 {
		t.Errorf("method/spread = %q/%v, want them retained on the finished run", b.Method, b.SpreadPct)
	}
	if b.Trial != 0 {
		t.Errorf("trial = %d on a finished run, want absent", b.Trial)
	}
}

// --- desired integrations (waired#935) ---

func integrationFrame(targets *[]string) *signer.InferenceState {
	st := desiredFrame("ollama", "", 0)
	if targets != nil {
		st.DesiredIntegrations = &signer.DesiredIntegrations{Enabled: *targets}
	}
	return st
}

// The three states, which is the whole reason this field is a pointer.
func TestSetupIntegrationStepThreeStates(t *testing.T) {
	none := []string{}
	two := []string{signer.IntegrationOpenCode, signer.IntegrationClaudeCode}

	t.Run("no instruction reports no row", func(t *testing.T) {
		f := &fakeSetupProvider{}
		r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
		r.Apply(context.Background(), integrationFrame(nil))
		if hasStepID(r.snapshot(context.Background()), setupStepIntegration) {
			t.Fatal("an integration row appeared with nothing asked for")
		}
	})

	t.Run("asked with everything off reports skipped", func(t *testing.T) {
		f := &fakeSetupProvider{}
		r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
		r.Apply(context.Background(), integrationFrame(&none))
		step := stepByID(t, r.snapshot(context.Background()), setupStepIntegration)
		// §7's `skipped` finally has a producer. Reporting it rather than
		// omitting the row is what lets the control plane tell "declined"
		// from "never asked" — the waired#904 class of false success.
		if step.Status != signer.SetupStatusSkipped {
			t.Fatalf("step = %+v, want skipped", step)
		}
	})

	t.Run("targets wait for the executor", func(t *testing.T) {
		f := &fakeSetupProvider{}
		r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
		r.Apply(context.Background(), integrationFrame(&two))
		step := stepByID(t, r.snapshot(context.Background()), setupStepIntegration)
		if step.Status != signer.SetupStatusPending {
			t.Fatalf("step = %+v, want pending before any executor report", step)
		}
	})
}

func TestSetupIntegrationStepFollowsTheExecutor(t *testing.T) {
	two := []string{signer.IntegrationOpenCode, signer.IntegrationClaudeCode}
	f := &fakeSetupProvider{}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = newFakeClock().now
	r.Apply(context.Background(), integrationFrame(&two))

	note := func(phase, errText string) signer.SetupStep {
		r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
			Attached: true, Elevated: true,
			Step: management.SetupStepIntegration, Phase: phase, Error: errText,
		})
		return stepByID(t, r.snapshot(context.Background()), setupStepIntegration)
	}

	if step := note(management.SetupExecutorPhaseInstalling, ""); step.Status != signer.SetupStatusRunning {
		t.Fatalf("step = %+v, want running", step)
	}
	if step := note(management.SetupExecutorPhaseDone, ""); step.Status != signer.SetupStatusDone {
		t.Fatalf("step = %+v, want done", step)
	}
	step := note(management.SetupExecutorPhaseFailed, "open /home/u/.claude: permission denied")
	if step.Status != signer.SetupStatusFailed || step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Fatalf("step = %+v, want failed/permission_denied", step)
	}
}

// The integration rides the same lease as the engine install. Its
// terminal phases must not release the engine's install claim, or a
// second elevated executor could start installing on top of the first.
func TestSetupIntegrationDoesNotTouchTheInstallClaim(t *testing.T) {
	two := []string{signer.IntegrationOpenCode}
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.Apply(context.Background(), integrationFrame(&two))
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	if got := r.SetupState(context.Background()).InstallClaimed; got != "ollama" {
		t.Fatalf("InstallClaimed = %q, want ollama", got)
	}

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step: management.SetupStepIntegration, Phase: management.SetupExecutorPhaseDone,
	})
	if got := r.SetupState(context.Background()).InstallClaimed; got != "ollama" {
		t.Fatalf("InstallClaimed = %q after an integration report, want the engine claim untouched", got)
	}
}

// The executor needs the target list, and the empty-vs-absent difference
// has to survive the trip: it decides whether the CLI writes anything.
func TestSetupStatePublishesTheIntegrationTargets(t *testing.T) {
	none := []string{}
	one := []string{signer.IntegrationOpenClaw}

	f := &fakeSetupProvider{}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(context.Background(), integrationFrame(nil))
	if got := r.SetupState(context.Background()).Integrations; got != nil {
		t.Fatalf("Integrations = %v with no instruction, want nil", got)
	}

	r.Apply(context.Background(), integrationFrame(&none))
	got := r.SetupState(context.Background()).Integrations
	if got == nil || len(*got) != 0 {
		t.Fatalf("Integrations = %v for an all-off answer, want a non-nil empty slice", got)
	}

	r.Apply(context.Background(), integrationFrame(&one))
	got = r.SetupState(context.Background()).Integrations
	if got == nil || len(*got) != 1 || (*got)[0] != signer.IntegrationOpenClaw {
		t.Fatalf("Integrations = %v, want [openclaw]", got)
	}
}

func TestFlattenIntegrations(t *testing.T) {
	tests := []struct {
		name string
		in   *signer.DesiredIntegrations
		want string
	}{
		{"nil is no instruction", nil, ""},
		{"empty is the all-off answer", &signer.DesiredIntegrations{}, integrationsNone},
		{
			// Sorted and de-duplicated: the wire order is the control
			// plane's, and a reorder with the same contents is not a change
			// the agent should react to.
			name: "sorted and deduplicated",
			in: &signer.DesiredIntegrations{Enabled: []string{
				signer.IntegrationOpenCode, signer.IntegrationClaudeCode, signer.IntegrationOpenCode,
			}},
			want: "claude-code,opencode",
		},
		{
			// A newer control plane naming a target this build has never
			// heard of must not cost the whole instruction.
			name: "unknown targets are dropped, not fatal",
			in:   &signer.DesiredIntegrations{Enabled: []string{"cursor", signer.IntegrationOpenClaw}},
			want: "openclaw",
		},
		{
			// ...but an instruction of ONLY unknown targets is still an
			// instruction, and reads as "nothing this agent can write".
			name: "only unknown targets collapse to the all-off answer",
			in:   &signer.DesiredIntegrations{Enabled: []string{"cursor"}},
			want: integrationsNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenIntegrations(tc.in); got != tc.want {
				t.Errorf("flattenIntegrations = %q, want %q", got, tc.want)
			}
		})
	}
}
