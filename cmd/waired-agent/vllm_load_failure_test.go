package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// Which failed vLLM starts are "the model does not fit here"
// (waired-agent#1515). Each marker is vLLM's or torch's own wording.
func TestVLLMStartFailedForMemory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		log        string
		overBudget bool
		want       bool
		wantReason string // checked when set
	}{
		{"CUDA OOM", "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB", false, true, ""},
		{"no room for the KV cache", "ValueError: No available memory for the cache blocks. Try increasing `gpu_memory_utilization`", false, true, ""},
		{"window over the KV pool", "ValueError: The model's max seq len (200704) is larger than the maximum number of tokens that can be stored in KV cache (98304)", false, true, ""},
		{"weights over the budget, any failure", "RuntimeError: Engine core initialization failed", true, true, ""},
		// What a real host logged when 23.4 GB of weights met a 20.8 GB
		// budget: the KV complaint is the symptom, the weights the cause.
		{"weights over the budget, the KV complaint", "ValueError: No available memory for the cache blocks", true, true,
			"the model's weights are larger than the GPU memory vLLM may use on this computer"},
		{"the KV complaint alone", "ValueError: No available memory for the cache blocks", false, true,
			"No available memory for the cache blocks"},
		{"a flag the venv does not know", "error: unrecognized arguments: --kv-offloading-size", false, false, ""},
		{"the port is taken", "OSError: [Errno 98] Address already in use", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := vllmStartFailedForMemory(tc.log, tc.overBudget)
			if got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// A build that did not start here is remembered in the shape it was asked
// for, and the engine is held off with the out-of-memory reason, whose
// sentence is the owner-approved one (#1464).
func TestRecordVLLMLoadFailure(t *testing.T) {
	p := vllmSwapProvider(t)
	m := vllmSwapManifests()[0]
	v := m.Variants[1] // the safetensors build
	shape := vllmLoadShape(infruntime.ModelTuning{ContextLength: 200704}, "fp8", 4)

	if _, blocked := p.vllmLoadBlocked(context.Background(), m, v, shape); blocked {
		t.Fatal("blocked before anything was recorded")
	}
	p.recordVLLMLoadFailure(context.Background(), m, v, shape, "CUDA out of memory", "the model did not fit")

	if _, blocked := p.vllmLoadBlocked(context.Background(), m, v, shape); !blocked {
		t.Error("the same build in the same shape is not blocked after it failed")
	}
	smaller := vllmLoadShape(infruntime.ModelTuning{ContextLength: 131072}, "fp8", 4)
	if _, blocked := p.vllmLoadBlocked(context.Background(), m, v, smaller); blocked {
		t.Error("a different shape is blocked; only the same attempt is")
	}
	if !p.vllmIsParked() || p.parkedBecause() != parkCauseOutOfMemory {
		t.Errorf("parked=%v cause=%v, want held off for memory", p.vllmIsParked(), p.parkedBecause())
	}
	if got := p.engineStoppedReason(); got == "" {
		t.Error("no reason on the status line for a memory stop")
	}

	// Choosing the model again overrules the record and resumes.
	p.forgetVLLMLoadFailures(m)
	if _, blocked := p.vllmLoadBlocked(context.Background(), m, v, shape); blocked {
		t.Error("still blocked after the model was chosen again")
	}
	if !p.resumeAfterOutOfMemory("a different model was chosen") {
		t.Fatal("resumeAfterOutOfMemory did nothing for a vLLM memory stop")
	}
	if p.vllmIsParked() || p.parkedBecause() != parkCauseNone {
		t.Errorf("parked=%v cause=%v after resuming, want neither", p.vllmIsParked(), p.parkedBecause())
	}
	waitForStartDecline(t, p, "resuming to ask the engine to start")
}

// The operator's own stop is theirs: a memory failure does not overwrite
// it, and a model switch does not lift it.
func TestParkVLLMForOutOfMemory_TheOperatorsStopWins(t *testing.T) {
	p := vllmSwapProvider(t)
	p.setVLLMParked(true)
	p.noteParked(parkCauseOperator)
	p.parkVLLMForOutOfMemory(vllmBlockedLoad{}, "CUDA out of memory")
	if p.parkedBecause() != parkCauseOperator {
		t.Errorf("cause = %v, want the operator's stop kept", p.parkedBecause())
	}
	if p.resumeAfterOutOfMemory("a different model was chosen") || !p.vllmIsParked() {
		t.Error("a model switch lifted the operator's own stop")
	}
}

// A switch on a vLLM host clears the model's records for every build: the
// person chose the model, not a build of it.
func TestSwapPreferredModel_VLLMClearsTheModelsLoadFailures(t *testing.T) {
	p := vllmSwapProvider(t)
	m := vllmSwapManifests()[0]
	shape := vllmLoadShape(infruntime.ModelTuning{ContextLength: 200704}, "fp8", 4)
	p.recordVLLMLoadFailure(context.Background(), m, m.Variants[1], shape, "CUDA out of memory", "")
	if err := p.store.Update(func(s *catalog.State) {
		s.VLLMModels = map[string]catalog.ModelState{
			"hybrid": {State: catalog.ModelStateReady, VariantID: "safetensors", LocalPath: t.TempDir()},
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SwapPreferredModel(context.Background(), "hybrid"); err != nil {
		t.Fatalf("SwapPreferredModel: %v", err)
	}
	if _, blocked := p.vllmLoadBlocked(context.Background(), m, m.Variants[1], shape); blocked {
		t.Error("the model was chosen again and its record still blocks the start")
	}
	if p.vllmIsParked() {
		t.Error("the memory stop was not lifted by choosing the model again")
	}
}

// The review that releases a memory stop once its record no longer applies
// reads the record of the build that failed to start, and on vLLM that is
// not the active selection: the model that was answering stays active until
// the chosen one is ready (waired-agent#1515). Read through the active
// selection it found no record, released the stop, and the bootstrap
// stopped the engine again — every 15 seconds on a real host.
func TestReviewOutOfMemoryPark_VLLMHoldsWhileTheRecordApplies(t *testing.T) {
	p := vllmSwapProvider(t)
	m := vllmSwapManifests()[0]
	v := m.Variants[1]
	shape := vllmLoadShape(infruntime.ModelTuning{ContextLength: 200704}, "fp8", 16)
	// The model that answered during the download is still the active one.
	if err := p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "qwen3.5-9b", VariantID: "w4a16"}
	}); err != nil {
		t.Fatal(err)
	}
	p.recordVLLMLoadFailure(context.Background(), m, v, shape, "No available memory for the cache blocks", "")

	p.reviewOutOfMemoryPark(context.Background())
	if !p.vllmIsParked() || p.parkedBecause() != parkCauseOutOfMemory {
		t.Fatalf("parked=%v cause=%v after the review, want still held off: the record still applies",
			p.vllmIsParked(), p.parkedBecause())
	}
	// The notice names the model that did not start, not the active one.
	ns := p.loadFailureNotices(context.Background())
	if len(ns) != 1 || !strings.Contains(ns[0].Title, "hybrid") {
		t.Errorf("notices = %+v, want one naming the model that did not start", ns)
	}

	// A new engine build: the record no longer describes this computer,
	// and the review releases the stop.
	sha := activeVariantSHA(p.manifests, m.ModelID, v.VariantID)
	if err := p.store.Update(func(s *catalog.State) {
		r := s.FailedLoads[sha]
		r.Context.EngineVersion = "0.0.1-an-older-build"
		s.FailedLoads[sha] = r
	}); err != nil {
		t.Fatal(err)
	}
	p.reviewOutOfMemoryPark(context.Background())
	if p.vllmIsParked() || p.parkedBecause() != parkCauseNone {
		t.Errorf("parked=%v cause=%v, want released once the record no longer applies",
			p.vllmIsParked(), p.parkedBecause())
	}
	waitForStartDecline(t, p, "the release to ask the engine to start")
	if ns := p.loadFailureNotices(context.Background()); len(ns) != 0 {
		t.Errorf("notices = %+v after the release, want none", ns)
	}
}

// Choosing another model lifts the stop and its notice together. The record
// stays — it is still true of that build — but the engine is no longer held
// off for it, and a notice asking the person to switch would ask for what
// they have just done.
func TestResumeAfterOutOfMemory_VLLMDropsTheNotice(t *testing.T) {
	p := vllmSwapProvider(t)
	m := vllmSwapManifests()[0]
	shape := vllmLoadShape(infruntime.ModelTuning{ContextLength: 200704}, "fp8", 16)
	p.recordVLLMLoadFailure(context.Background(), m, m.Variants[1], shape, "CUDA out of memory", "")
	if ns := p.loadFailureNotices(context.Background()); len(ns) != 1 {
		t.Fatalf("notices = %+v, want one for the start that failed", ns)
	}
	if !p.resumeAfterOutOfMemory("a different model was chosen") {
		t.Fatal("resumeAfterOutOfMemory did nothing for a vLLM memory stop")
	}
	waitForStartDecline(t, p, "resuming to ask the engine to start")
	if ns := p.loadFailureNotices(context.Background()); len(ns) != 0 {
		t.Errorf("notices = %+v after another model was chosen, want none", ns)
	}
}
