package main

import (
	"strings"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// oomBody is the verbatim reply measured on sv-mag (RTX PRO 4000
// Blackwell, ollama 0.32.13) serving qwen3.8:27b-mtp-q4_K_M-wb2048 for a
// ~2,000-token prompt, 2026-08-27 (waired-agent#1038). It lived in the
// depth benchmark's test file until that sweep was retired
// (waired-agent#1169); the fixture outlives it because the path it
// exercises does.
const oomBody = `{"error":"an error was encountered while running the model: CUDA error\nCUDA error: out of memory"}`

// TestOnEngineFitFailure_SurvivesTheRetiredSweep.
//
// PRODUCT CONTRACT — owner ruling 2026-09-04, recorded in
// docs/decisions/20260904/0000-retire-the-long-context-sweep.md.
//
// Retiring the #624 long-context sweep removed one of the two ways this
// agent learns that the accelerator cannot serve the window it is
// configured for. This pins the other one, which is the one that
// matters: a REAL request's 5xx. Written because the deletion could
// quietly have taken the whole capability with it — the sweep is what
// #1058 wired to the handler, and it is easy to read the handler as
// belonging to it.
//
// What is asserted is the agent-side half. The adapter-side classifier
// and debounce have their own tests in internal/runtime.
func TestOnEngineFitFailure_SurvivesTheRetiredSweep(t *testing.T) {
	a := newTestAdapter(t)
	a.SetAppliedTuning(infruntime.ModelTuning{
		ModelID:       "heavy",
		ContextLength: 200704,
		WindowFits:    true,
	})
	p := &agentInferenceProvider{ollama: a}
	a.SetOnFitFailure(p.onEngineFitFailure)

	// The wire path: what a served request gets back when the GPU runs
	// out of memory part-way through a long prompt.
	a.ReportUpstreamFailure(500, []byte(oomBody))

	deadline := time.Now().Add(2 * time.Second)
	var got infruntime.ModelTuning
	for time.Now().Before(deadline) {
		got = a.AppliedTuning()
		if !got.WindowFits {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got.WindowFits {
		t.Fatal("WindowFits is still true — nothing recorded that this host cannot hold its window")
	}
	if !got.Degraded {
		t.Error("Degraded = false, want true")
	}
	if got.ModelID != "heavy" || got.ContextLength != 200704 {
		t.Errorf("the recorded tuning lost its identity: %+v", got)
	}
	// The engine's own words reach the warning, which is what `waired
	// doctor` and `waired models ls --detail` show a person.
	if !strings.Contains(got.Warning, "ran out of memory") {
		t.Errorf("Warning does not say what happened: %q", got.Warning)
	}
	if !strings.Contains(got.Warning, "CUDA error") {
		t.Errorf("Warning drops the engine's own sentence: %q", got.Warning)
	}
}

// TestOnEngineFitFailure_UntunedEngineRecordsNothing is a record of
// today's behaviour: with no applied tuning there is no configuration to
// blame, so the handler stands down rather than inventing one.
func TestOnEngineFitFailure_UntunedEngineRecordsNothing(t *testing.T) {
	a := newTestAdapter(t)
	p := &agentInferenceProvider{ollama: a}

	p.onEngineFitFailure("CUDA error: out of memory")

	if got := a.AppliedTuning(); got.Degraded || got.Warning != "" {
		t.Errorf("tuning = %+v, want untouched", got)
	}
}
