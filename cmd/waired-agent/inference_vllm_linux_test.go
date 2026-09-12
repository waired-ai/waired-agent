//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
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
	// args records the argv of every invocation. Recorded rather than
	// dropped: which files the pull asks for IS the behaviour under test
	// in waired-agent#1298, and a fake that swallowed the argument would
	// make that case unwritable.
	mu   sync.Mutex
	args [][]string
	// onRun runs while the pull is in flight. The progress a pull reports
	// is forgotten when it finishes (dlProgress.forget), so mid-pull is
	// the only place it can be observed at all.
	onRun func()
}

func (f *fakeHFRunner) Run(_ context.Context, _ string, args, _ []string, onLine func(string)) error {
	f.mu.Lock()
	f.args = append(f.args, append([]string(nil), args...))
	f.mu.Unlock()
	if f.onRun != nil {
		f.onRun()
	}
	for _, l := range f.lines {
		onLine(l)
	}
	return f.err
}

func (f *fakeHFRunner) lastArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.args) == 0 {
		return nil
	}
	return f.args[len(f.args)-1]
}

// fakeHFLister answers the repository listing without a network.
type fakeHFLister struct {
	files []download.HFRepoFile
	err   error
}

func (f fakeHFLister) ListTopLevel(context.Context, string, string) ([]download.HFRepoFile, error) {
	return f.files, f.err
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
	p := &agentInferenceProvider{
		store:    catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		stateDir: t.TempDir(), // no venv → engineVersionFor(vllm) == ""
		// PreferredModelID is the operator's CHOICE; BundledModelID is the
		// hardware recommendation. vllmTarget reads only the first
		// (waired-agent#1298), so both are set here and the tests below
		// pin which one drives a start.
		cfg: agentconfig.InferenceConfig{
			AllowPull: true, BundledModelID: "gpt-oss-20b", PreferredModelID: "gpt-oss-20b",
		},
		manifests:  []catalog.Manifest{mixedVLLMManifest()},
		dlProgress: newDownloadProgress(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	p.setServingEngine(catalog.RuntimeVLLM)
	return p
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
	puller := download.NewHFPuller("hf-fake", &fakeHFRunner{lines: []string{"done"}})

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
	puller := download.NewHFPuller("hf-fake", &fakeHFRunner{err: io.ErrUnexpectedEOF})

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

	puller := download.NewHFPuller("hf-fake", &fakeHFRunner{err: io.ErrUnexpectedEOF})
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

// PRODUCT CONTRACT (waired-agent#1298): the weights pull asks for the
// repository's TOP LEVEL by name, not for the repository.
//
// openai/gpt-oss-20b is 41.30 GB whole and 13.79 GB at its top level — the
// surplus is the same weights again in two other formats, under original/
// and metal/, neither of which a vLLM host loads. A pattern cannot express
// this: `*.safetensors` matches original/model.safetensors.
//
// The same listing is where the byte total comes from, which is the other
// half of the defect: parseHFProgressLine emits a percentage and no bytes,
// and the aggregator drops every event without a total, so the wizard's
// model row read 0 / 0 for the whole of a multi-gigabyte download.
func TestDownloadHFWeights_AsksForTheTopLevelAndKnowsItsSize(t *testing.T) {
	p := vllmTestProvider(t)
	p.hfFiles = fakeHFLister{files: []download.HFRepoFile{
		{Name: "config.json", Size: 1_000},
		{Name: "model-00001-of-00002.safetensors", Size: 4_000_000_000},
		{Name: "model-00002-of-00002.safetensors", Size: 3_000_000_000},
	}}
	m, variant := mixedVLLMManifest(), mixedVLLMManifest().Variants[1]
	var completed, total int64
	var sawProgress bool
	runner := &fakeHFRunner{lines: []string{"done"}}
	runner.onRun = func() { completed, total, _, sawProgress = p.dlProgress.aggregate(m.ModelID) }
	puller := download.NewHFPuller("hf-fake", runner)

	if _, err := p.downloadHFWeights(context.Background(), m.ModelID, variant, puller, false); err != nil {
		t.Fatalf("downloadHFWeights: %v", err)
	}

	args := strings.Join(runner.lastArgs(), " ")
	for _, want := range []string{"config.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		if !strings.Contains(args, want) {
			t.Errorf("argv %q does not ask for %q", args, want)
		}
	}

	if !sawProgress {
		t.Fatal("no byte progress was reported while the pull ran")
	}
	if total != 7_000_001_000 {
		t.Errorf("total = %d, want the listing's sum 7000001000", total)
	}
	// Announced before the first byte lands, so the figure is whole from
	// the start instead of growing as files appear.
	if completed != 0 {
		t.Errorf("completed = %d, want 0 with nothing on disk", completed)
	}
}

// PRODUCT CONTRACT (waired-agent#1298): narrowing only happens when the
// weights are IN the narrowed set. A repository that keeps its shards in a
// subdirectory — an `nvfp4/` or `fp8/` build, which is the shelf
// waired-agent#575 is filling in — would otherwise have its config and
// tokenizer fetched, report 100%, and leave the engine to fail on a model
// with no weights.
func TestDownloadHFWeights_NoWeightsAtTheTopLevelTakesTheWholeRepo(t *testing.T) {
	p := vllmTestProvider(t)
	p.hfFiles = fakeHFLister{files: []download.HFRepoFile{
		{Name: "config.json", Size: 1_000},
		{Name: "tokenizer.json", Size: 2_000},
		{Name: "README.md", Size: 500},
	}}
	m, variant := mixedVLLMManifest(), mixedVLLMManifest().Variants[1]
	var sawProgress bool
	runner := &fakeHFRunner{lines: []string{"done"}}
	runner.onRun = func() { _, _, _, sawProgress = p.dlProgress.aggregate(m.ModelID) }
	puller := download.NewHFPuller("hf-fake", runner)

	if _, err := p.downloadHFWeights(context.Background(), m.ModelID, variant, puller, false); err != nil {
		t.Fatalf("downloadHFWeights: %v", err)
	}
	args := runner.lastArgs()
	if len(args) < 3 || args[2] != "--local-dir" {
		t.Fatalf("argv = %v, want a whole-repo download with no file names", args)
	}
	if sawProgress {
		t.Error("byte progress was reported for a listing the pull did not use")
	}
}

// A listing that cannot be read must not refuse the pull: the fetch falls
// back to the whole repository, exactly as it behaved before
// waired-agent#1298, and the byte row goes back to reporting nothing.
func TestDownloadHFWeights_ListingFailureFallsBackToTheWholeRepo(t *testing.T) {
	p := vllmTestProvider(t)
	p.hfFiles = fakeHFLister{err: io.ErrUnexpectedEOF}
	m, variant := mixedVLLMManifest(), mixedVLLMManifest().Variants[1]
	var sawProgress bool
	runner := &fakeHFRunner{lines: []string{"done"}}
	runner.onRun = func() { _, _, _, sawProgress = p.dlProgress.aggregate(m.ModelID) }
	puller := download.NewHFPuller("hf-fake", runner)

	if _, err := p.downloadHFWeights(context.Background(), m.ModelID, variant, puller, false); err != nil {
		t.Fatalf("downloadHFWeights: %v", err)
	}
	args := runner.lastArgs()
	if len(args) < 2 || args[0] != "download" || args[1] != variant.Source.RepoID {
		t.Fatalf("argv = %v, want a whole-repo download", args)
	}
	if args[2] != "--local-dir" {
		t.Errorf("argv = %v, want no file names between the repo and --local-dir", args)
	}
	if sawProgress {
		t.Error("byte progress was reported from a listing that failed")
	}
}

// vllmTarget must choose the vLLM (safetensors) variant, not the ollama tag,
// for a mixed-variant model.
func TestVLLMTarget_PicksVLLMVariant(t *testing.T) {
	p := vllmTestProvider(t)
	m, v, _, err := p.vllmTarget()
	if err != nil {
		t.Fatalf("vllmTarget: %v", err)
	}
	if m.ModelID != "gpt-oss-20b" || v.Source.Type != catalog.SourceHuggingFace {
		t.Fatalf("target=%s/%s, want gpt-oss-20b / huggingface", m.ModelID, v.Source.Type)
	}
}

// vllmTarget errors when the CHOSEN model is ollama-only — the "opted into
// vLLM but the model can't run on it" case. chosen stays true: someone did
// pick, and the pick is the problem, so this one IS a fault and the
// bootstrap records it.
func TestVLLMTarget_NoVLLMVariant(t *testing.T) {
	p := vllmTestProvider(t)
	p.manifests = []catalog.Manifest{{
		ModelID: "gpt-oss-20b",
		Variants: []catalog.Variant{{
			VariantID: "q4", RuntimeSupport: []string{catalog.RuntimeOllama},
			Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "gpt-oss:20b-q4"},
		}},
	}}
	_, _, chosen, err := p.vllmTarget()
	if err == nil {
		t.Fatal("vllmTarget should fail for an ollama-only model")
	}
	if !chosen {
		t.Error("chosen = false, want true: a model was picked, it just cannot run here")
	}
	if errors.Is(err, errVLLMNoModelChosen) {
		t.Error("an unusable choice must not read as no choice — the bootstrap ignores the latter")
	}
}

// PRODUCT CONTRACT (waired-agent#1298): with several vLLM builds of one
// model, the one this HOST fits is served — not the first one listed.
//
// FirstPullableVariant answers "can this engine load it at all" and stops
// at the first yes, which is the right question only while a model ships
// one variant per engine. glm-5.2 already ships two safetensors builds,
// and waired-agent#575 adds more. The ollama side was moved onto
// FamilyBestFit in waired-agent#1265; this is the vLLM half.
func TestVLLMTarget_PicksTheVariantTheHostFits(t *testing.T) {
	p := vllmTestProvider(t)
	p.profiler = hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return []hardware.GPU{{Vendor: "nvidia", Model: "test", VRAMTotalMB: 24576}},
				hardware.Accelerators{CUDA: true}, nil
		}))
	p.cfg.PreferredModelID = "two-builds"
	p.manifests = []catalog.Manifest{{
		ModelID:       "two-builds",
		ContextLength: 262144,
		Variants: []catalog.Variant{
			{
				// Listed FIRST and far too large for the card.
				VariantID: "huge", Format: catalog.FormatSafetensors,
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      196608, EstimatedWeightGB: 180, QualityTier: 95,
				KVBytesPerTokenFP16: 32768,
				Source:              catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "org/huge"},
			},
			{
				VariantID: "fits", Format: catalog.FormatSafetensors,
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      12288, EstimatedWeightGB: 5, QualityTier: 40,
				KVBytesPerTokenFP16: 12288,
				Source:              catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "org/fits"},
			},
		},
	}}

	_, v, _, err := p.vllmTarget()
	if err != nil {
		t.Fatalf("vllmTarget: %v", err)
	}
	if v.VariantID == "huge" {
		t.Fatal("the first-listed variant was served on a card a quarter its size")
	}
	if v.VariantID != "fits" {
		t.Errorf("variant = %q, want \"fits\": manifest order was followed instead of the host", v.VariantID)
	}
}

// PRODUCT CONTRACT (waired-agent#1298): with nothing chosen, vllmTarget
// does NOT fall back to the bundled model. The bundled id is the hardware
// auto-selector's recommendation, computed against whichever engine the
// picker named, and on a wizard-driven vLLM install it is routinely an
// ollama-only model. Starting on it is how the engine refused seconds
// after the venv appeared, minutes before the operator reached the picker.
func TestVLLMTarget_DoesNotFallBackToTheBundledModel(t *testing.T) {
	p := vllmTestProvider(t)
	p.cfg.PreferredModelID = "" // nothing chosen; BundledModelID stays set
	m, _, chosen, err := p.vllmTarget()
	if !errors.Is(err, errVLLMNoModelChosen) {
		t.Fatalf("vllmTarget = (%q, err=%v), want errVLLMNoModelChosen", m.ModelID, err)
	}
	if chosen {
		t.Error("chosen = true with no preference set")
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
	p.setVLLM(adapter)

	entry := p.runtimeStatusFor(context.Background(), "vllm", hardware.Profile{})
	if entry.ContextLength != 59392 {
		t.Errorf("ContextLength = %d, want 59392", entry.ContextLength)
	}
	if !strings.Contains(entry.TuningWarning, "clamped") {
		t.Errorf("TuningWarning = %q, want the clamp note", entry.TuningWarning)
	}
}

// PRODUCT CONTRACT (waired-agent#1026): the port the engine is spawned on
// is the RESOLVED one, so an agent.json carrying the old default lands on
// the waired-owned port rather than vLLM's 8000.
//
// The seam is deliberately here rather than at the adapter: the adapter's
// own fallback is for a hand-built config, and the defect was that this
// call site read cfg.VLLMPort raw. On a host where something else owned
// 8000 — a container publishing a range, a dev server — the API server
// could not bind, every retry failed the same way, and the wizard's
// benchmark was the only thing that showed it.
func TestVLLMSpawnUsesTheResolvedPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  int
		want int
	}{
		{"unset resolves to the waired-owned port", agentconfig.VLLMPortAuto, agentconfig.DefaultVLLMBundledPort},
		{"a serialized legacy 8000 flips", 8000, agentconfig.DefaultVLLMBundledPort},
		{"an operator's own port is kept", 9485, 9485},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := vllmTestProvider(t)
			p.cfg.VLLMPort = tc.cfg
			if got := p.cfg.ResolvedVLLMPort(); got != tc.want {
				t.Fatalf("ResolvedVLLMPort() = %d, want %d", got, tc.want)
			}
			// probeTargetForActive is the other reader of the same
			// number — the benchmark, the depth bench and the mesh probe
			// all dial what it returns — and it now resolves through the
			// same function. It is not asserted here because it reads
			// catalog.DefaultStatePath() rather than a store it is handed,
			// which is the boot-frozen shape waired-agent#948 is about;
			// its own test lands with that fix.
		})
	}
}

// The linux half of the compile-time assertion in engine_dead_test.go.
// servingEngineDead reaches the give-up latch through an interface assertion
// that fails OPEN, so a vLLM adapter missing the method would keep a given-up
// host advertising itself to the mesh with nothing in the test suite noticing
// (waired-agent#1138).
var _ interface{ FailureLatched() bool } = (*infruntime.VLLMAdapter)(nil)
