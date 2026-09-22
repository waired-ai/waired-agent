package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/notice"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The wording of vLLM v0.29.0 (the pin) for the failures that are about the
// build rather than the machine's setup (waired-ai/waired#1480). Each line
// is vLLM's or transformers' own; the file each comes from is on
// vllmStartFailureKind. Record of today's behaviour.
const (
	vllmLogArchitecture = "ValueError: Model architectures ['FooForCausalLM'] are not supported for now. Supported architectures: dict_keys(['LlamaForCausalLM'])"
	vllmLogRemoteCode   = "ValueError: The repository for acme/m contains custom code which must be executed to correctly load the model. You can inspect the repository content at https://hf.co/acme/m.\nPlease pass the argument `trust_remote_code=True` to allow custom code to be run."
	vllmLogCapability   = "ValueError: The quantization method fp8 is not supported for the current GPU. Minimum capability: 89. Current capability: 86."
	vllmLogNoWeights    = "RuntimeError: Cannot find any model weights with `/var/lib/waired/models/hf/acme__m@0123456789ab`"
	vllmLogEstimatedLen = "ValueError: To serve at least one request with the model's max seq len (40960), (4.5 GiB KV cache is needed, which is larger than the available KV cache memory (2.1 GiB). Based on the available memory, the estimated maximum model length is 18432. Try increasing `gpu_memory_utilization` or decreasing `max_model_len` when initializing the engine."
	vllmLogFreeMemory   = "ValueError: Free memory on device cuda:0 (3.2/23.5 GiB) on startup is less than desired GPU memory utilization (0.85, 20.0 GiB). Decrease GPU memory utilization or reduce GPU memory used by other processes."
)

func TestVLLMStartFailureKind(t *testing.T) {
	for log, want := range map[string]string{
		vllmLogArchitecture:                  signer.LoadFailureArchitectureUnsupported,
		vllmLogRemoteCode:                    signer.LoadFailureRemoteCodeRequired,
		vllmLogCapability:                    signer.LoadFailureQuantizationUnsupported,
		vllmLogNoWeights:                     signer.LoadFailureWeightsMissing,
		vllmLogEstimatedLen:                  "", // memory, vllmStartFailedForMemory's
		vllmLogFreeMemory:                    "", // another program's memory: transient, not recorded
		"error: unrecognized arguments: --x": "",
	} {
		if got := vllmStartFailureKind(log); got != want {
			t.Errorf("%.50q → %q, want %q", log, got, want)
		}
	}
	mem, reason := vllmStartFailedForMemory(vllmLogEstimatedLen, false)
	if !mem || vllmEngineMaxWindow(vllmLogEstimatedLen) != 18432 {
		t.Errorf("the KV shortfall: memory=%v window=%d, want memory and 18432", mem, vllmEngineMaxWindow(vllmLogEstimatedLen))
	}
	// The reason ends the "did not load" notice, so it is a sentence, not
	// the middle of vLLM's.
	if !strings.Contains(reason, "at most 18432 tokens") || strings.Contains(reason, "estimated maximum model length") {
		t.Errorf("the KV shortfall's reason = %q", reason)
	}
}

// Every one of them says what happened and what to do, on the line `waired
// status`, the console and doctor show (runtimes[].last_error).
func TestVLLMStartupDiagnosis_TheModelsOwnFailures(t *testing.T) {
	for log, wants := range map[string][]string{
		vllmLogArchitecture: {"['FooForCausalLM']", "choose another model"},
		vllmLogRemoteCode:   {"its own code", "choose another model"},
		vllmLogCapability:   {"fp8", "compute capability 8.9 or higher", "this GPU has 8.6", "another quantization"},
		vllmLogNoWeights:    {"no safetensors weights", "waired models rm"},
		vllmLogEstimatedLen: {"18432", "smaller model"},
		vllmLogFreeMemory:   {"3.2 of 23.5 GiB", "close that program"},
	} {
		got := vllmStartupDiagnosis(log, "127.0.0.1:9510")
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("%.40q → %q, missing %q", log, got, w)
			}
		}
	}
}

// A build this engine cannot run here is recorded with its kind, held off
// like a memory stop — but no surface says "out of memory" about it: the
// status line, the published cause, the notice and the published record each
// say what did happen (waired-ai/waired#1480).
func TestRecordVLLMLoadFailure_ABuildThatCannotStart(t *testing.T) {
	p := vllmSwapProvider(t)
	m := vllmSwapManifests()[0]
	v := m.Variants[1]
	shape := vllmLoadShape(infruntime.ModelTuning{ContextLength: 200704}, "fp8", 4)
	hint := vllmStartupDiagnosis(vllmLogArchitecture, "127.0.0.1:9510")
	p.recordVLLMLoadFailure(context.Background(), m, v, shape, hint, "", signer.LoadFailureArchitectureUnsupported, 0)

	if _, blocked := p.vllmLoadBlocked(context.Background(), m, v, shape); !blocked {
		t.Error("the same build is not blocked after it could not start")
	}
	if !p.vllmIsParked() || p.parkedBecause() != parkCauseCannotStart || !p.parkedForLoadFailure() {
		t.Errorf("parked=%v cause=%v, want held off as cannot-start", p.vllmIsParked(), p.parkedBecause())
	}
	if got := p.engineStoppedReason(); !strings.Contains(got, "cannot start") || strings.Contains(got, "memory") {
		t.Errorf("status line %q", got)
	}
	if got := p.PublishedEngineStoppedCause(); got != "" {
		t.Errorf("published cause %q, want none (out_of_memory would be false)", got)
	}
	var found bool
	for _, f := range p.PublishedLoadFailures() {
		if f.ModelID == m.ModelID && f.Reason == signer.LoadFailureArchitectureUnsupported {
			found = true
		}
	}
	if !found {
		t.Errorf("published records %+v, want the architecture reason", p.PublishedLoadFailures())
	}
	ns := p.loadFailureNotices(context.Background())
	if len(ns) != 1 || ns[0].Kind != notice.KindModelDidNotLoad || strings.Contains(ns[0].Text, "memory") ||
		!strings.Contains(ns[0].Title, "did not start") {
		t.Errorf("notices %+v", ns)
	}
	// Choosing the model again overrules it, as it does a memory record.
	p.forgetVLLMLoadFailures(m)
	if !p.resumeAfterOutOfMemory("a different model was chosen") || p.parkedBecause() != parkCauseNone {
		t.Errorf("not released: cause %v", p.parkedBecause())
	}
	waitForStartDecline(t, p, "resuming to ask the engine to start")
}
