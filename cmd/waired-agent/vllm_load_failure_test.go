package main

import (
	"context"
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
	}{
		{"CUDA OOM", "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB", false, true},
		{"no room for the KV cache", "ValueError: No available memory for the cache blocks. Try increasing `gpu_memory_utilization`", false, true},
		{"window over the KV pool", "ValueError: The model's max seq len (200704) is larger than the maximum number of tokens that can be stored in KV cache (98304)", false, true},
		{"weights over the budget, any failure", "RuntimeError: Engine core initialization failed", true, true},
		{"a flag the venv does not know", "error: unrecognized arguments: --kv-offloading-size", false, false},
		{"the port is taken", "OSError: [Errno 98] Address already in use", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := vllmStartFailedForMemory(tc.log, tc.overBudget); got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
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
	p.parkVLLMForOutOfMemory("CUDA out of memory")
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
		s.Models = map[string]catalog.ModelState{
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
