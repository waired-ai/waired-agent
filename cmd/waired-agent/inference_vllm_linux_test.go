//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeHFRunner satisfies download.HFRunner without shelling out to a real
// huggingface-cli. It emits the provided lines and returns err.
type fakeHFRunner struct {
	lines []string
	err   error
}

func (f fakeHFRunner) Run(_ context.Context, _ string, _, _ []string, onLine func(string)) error {
	for _, l := range f.lines {
		onLine(l)
	}
	return f.err
}

// mixedVLLMManifest is a model that ships both an ollama tag and a vLLM
// safetensors variant, so engine selection actually has a choice to make.
func mixedVLLMManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "gpt-oss-20b",
		ContextLength: 8192,
		Variants: []catalog.Variant{
			{
				VariantID: "q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "gpt-oss:20b-q4"},
			},
			{
				VariantID:      "mxfp4-safetensors",
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				DType:          "auto",
				Source:         catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "openai/gpt-oss-20b"},
			},
		},
	}
}

func vllmTestProvider(t *testing.T) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		store:      catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		stateDir:   t.TempDir(), // no venv → engineVersionFor(vllm) == ""
		engine:     catalog.RuntimeVLLM,
		cfg:        agentconfig.InferenceConfig{AllowPull: true, BundledModelID: "gpt-oss-20b", VLLMPort: 8000},
		manifests:  []catalog.Manifest{mixedVLLMManifest()},
		dlProgress: newDownloadProgress(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// resolveVLLMTensorParallel: auto follows the identical-GPU rule; an
// explicit override wins but is clamped to the detected NVIDIA GPU
// count (an over-sized TP makes vLLM die during NCCL world setup);
// an explicit 1 forces single-GPU and is never auto-upgraded.
func TestResolveVLLMTensorParallel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	l4 := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}
	dualL4 := hardware.Profile{GPUs: []hardware.GPU{l4, l4}}
	quadL4 := hardware.Profile{GPUs: []hardware.GPU{l4, l4, l4, l4}}

	if got := resolveVLLMTensorParallel(0, dualL4, logger); got != 2 {
		t.Errorf("auto on 2xL4 = %d, want 2", got)
	}
	if got := resolveVLLMTensorParallel(1, quadL4, logger); got != 1 {
		t.Errorf("override 1 on 4-GPU host = %d, want 1 (forced single GPU)", got)
	}
	if got := resolveVLLMTensorParallel(4, dualL4, logger); got != 2 {
		t.Errorf("override 4 on 2-GPU host = %d, want 2 (clamped)", got)
	}
	if got := resolveVLLMTensorParallel(0, hardware.Profile{}, logger); got != 1 {
		t.Errorf("auto on CPU-only host = %d, want 1", got)
	}
	if got := resolveVLLMTensorParallel(2, hardware.Profile{}, logger); got != 1 {
		t.Errorf("override on CPU-only host = %d, want 1 (no NVIDIA GPU)", got)
	}
}

// downloadHFWeights must drive the model to Ready with the on-disk path and
// repo recorded, and register a local vLLM endpoint the router can select.
func TestDownloadHFWeights_RecordsReadyAndEndpoint(t *testing.T) {
	p := vllmTestProvider(t)
	m := mixedVLLMManifest()
	variant := m.Variants[1] // the vLLM safetensors variant
	puller := download.NewHFPuller("hf-fake", fakeHFRunner{lines: []string{"done"}})

	localDir, err := p.downloadHFWeights(context.Background(), m.ModelID, variant, puller, false)
	if err != nil {
		t.Fatalf("downloadHFWeights: %v", err)
	}
	if want := p.hfLocalDir("openai/gpt-oss-20b"); localDir != want {
		t.Fatalf("localDir=%q, want %q", localDir, want)
	}

	st, _ := p.store.Load()
	ms := st.Models[m.ModelID]
	if ms.State != catalog.ModelStateReady {
		t.Errorf("model state=%q, want ready", ms.State)
	}
	if ms.LocalPath != localDir || ms.HFRepo != "openai/gpt-oss-20b" {
		t.Errorf("recorded LocalPath=%q HFRepo=%q, want %q / openai/gpt-oss-20b", ms.LocalPath, ms.HFRepo, localDir)
	}
	foundVLLMEndpoint := false
	for _, ep := range st.Endpoints {
		if ep.Runtime == catalog.RuntimeVLLM && ep.ModelID == m.ModelID {
			foundVLLMEndpoint = true
		}
	}
	if !foundVLLMEndpoint {
		t.Errorf("no local vLLM endpoint recorded; endpoints=%v", st.Endpoints)
	}
}

// A failing download must mark the model Failed with the error, not leave it
// stuck "downloading".
func TestDownloadHFWeights_FailureRecordsFailedState(t *testing.T) {
	p := vllmTestProvider(t)
	m := mixedVLLMManifest()
	variant := m.Variants[1]
	puller := download.NewHFPuller("hf-fake", fakeHFRunner{err: io.ErrUnexpectedEOF})

	if _, err := p.downloadHFWeights(context.Background(), m.ModelID, variant, puller, false); err == nil {
		t.Fatal("expected download error")
	}
	st, _ := p.store.Load()
	if ms := st.Models[m.ModelID]; ms.State != catalog.ModelStateFailed {
		t.Errorf("model state=%q, want failed", ms.State)
	}
}

// TestDownloadHFWeights_RefreshFailureKeepsReady: a failed refresh (refresh=true)
// of an already-ready vLLM model must keep it ready, mirroring the ollama
// sticky-state fix (#614).
func TestDownloadHFWeights_RefreshFailureKeepsReady(t *testing.T) {
	p := vllmTestProvider(t)
	m := mixedVLLMManifest()
	variant := m.Variants[1]

	// Seed the model as already ready on disk.
	if err := p.store.Update(func(s *catalog.State) {
		s.Models[m.ModelID] = catalog.ModelState{
			VariantID: variant.VariantID,
			HFRepo:    variant.Source.RepoID,
			LocalPath: p.hfLocalDir(variant.Source.RepoID),
			State:     catalog.ModelStateReady,
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	puller := download.NewHFPuller("hf-fake", fakeHFRunner{err: io.ErrUnexpectedEOF})
	if _, err := p.downloadHFWeights(context.Background(), m.ModelID, variant, puller, true); err == nil {
		t.Fatal("expected download error")
	}
	st, _ := p.store.Load()
	ms := st.Models[m.ModelID]
	if ms.State != catalog.ModelStateReady {
		t.Errorf("state = %q after failed refresh, want ready (serving must survive)", ms.State)
	}
	if ms.Error == "" {
		t.Errorf("Error should record the failed refresh, got empty")
	}
}

// vllmTarget must choose the vLLM (safetensors) variant, not the ollama tag,
// for a mixed-variant model.
func TestVLLMTarget_PicksVLLMVariant(t *testing.T) {
	p := vllmTestProvider(t)
	m, v, ok := p.vllmTarget()
	if !ok {
		t.Fatal("vllmTarget: expected a vLLM-capable model")
	}
	if m.ModelID != "gpt-oss-20b" || v.Source.Type != catalog.SourceHuggingFace {
		t.Fatalf("target=%s/%s, want gpt-oss-20b / huggingface", m.ModelID, v.Source.Type)
	}
}

// vllmTarget returns ok=false when the selected model is ollama-only — the
// "opted into vLLM but the model can't run on it" case.
func TestVLLMTarget_NoVLLMVariant(t *testing.T) {
	p := vllmTestProvider(t)
	p.manifests = []catalog.Manifest{{
		ModelID: "gpt-oss-20b",
		Variants: []catalog.Variant{{
			VariantID: "q4", RuntimeSupport: []string{catalog.RuntimeOllama},
			Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "gpt-oss:20b-q4"},
		}},
	}}
	if _, _, ok := p.vllmTarget(); ok {
		t.Fatal("vllmTarget should be false for an ollama-only model")
	}
}

// PullModel routes an HF/vLLM variant through dispatchHFPull (not the ollama
// puller); without an installed venv that surfaces a clear error rather than
// the old "phase A only supports ollama" rejection.
func TestPullModel_HFVariant_RoutesToHFPath(t *testing.T) {
	p := vllmTestProvider(t)
	_, err := p.PullModel(context.Background(), "gpt-oss-20b")
	if err == nil {
		t.Fatal("expected an error (no vLLM venv installed)")
	}
	if !strings.Contains(err.Error(), "vllm") || !strings.Contains(err.Error(), "venv") {
		t.Errorf("error %q should mention the vllm venv (proves it reached dispatchHFPull)", err.Error())
	}
}

// #675: the vllm runtime-status entry carries the exported context
// window and its tuning warning (ollama parity), read through the
// adapter behind p.vllm.
func TestRuntimeStatusFor_VLLMCarriesTuning(t *testing.T) {
	p := vllmTestProvider(t)
	p.registry = infruntime.NewRegistry()
	adapter := infruntime.NewVLLMAdapter(infruntime.VLLMConfig{})
	adapter.SetAppliedTuning(infruntime.ModelTuning{
		ModelID: "gpt-oss-20b", VariantID: "mxfp4",
		ContextLength: 59392,
		Warning:       "context window clamped to 59392 tokens (model native 131072) so the KV cache fits GPU memory at gpu-memory-utilization=0.85, TP=1",
	})
	p.registry.Register(adapter)
	p.vllm = adapter

	entry := p.runtimeStatusFor(context.Background(), "vllm", hardware.Profile{})
	if entry.ContextLength != 59392 {
		t.Errorf("ContextLength = %d, want 59392", entry.ContextLength)
	}
	if !strings.Contains(entry.TuningWarning, "clamped") {
		t.Errorf("TuningWarning = %q, want the clamp note", entry.TuningWarning)
	}
}
