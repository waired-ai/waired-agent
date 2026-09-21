package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// One engine's record of a model says nothing about the other engine
// (waired-agent#1520).
//
// The case a real host reached through the product alone: a host serving
// with ollama measured its speed with vLLM (#1298), which recorded the
// model Ready. With one record per model id, ollama was then taken to
// have it — `waired models pull` answered at once, the tuning was sized
// for the vLLM build, and the benchmark asked ollama for a model it had
// never pulled and got a 404.

// dualManifest is a model both engines can serve, each with its own build.
func dualManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID: "dual",
		Variants: []catalog.Variant{
			{VariantID: "q8", RuntimeSupport: []string{catalog.RuntimeOllama},
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "dual:q8"}},
			{VariantID: "bf16", RuntimeSupport: []string{catalog.RuntimeVLLM},
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "acme/dual"}},
		},
	}
}

// vllmOnlyProvider serves with engine, and only vLLM has fetched "dual".
func vllmOnlyProvider(t *testing.T, engine string) *agentInferenceProvider {
	t.Helper()
	p := activeReaderProvider(t)
	p.manifests = []catalog.Manifest{dualManifest()}
	p.setServingEngine(engine)
	if err := p.store.Update(func(s *catalog.State) {
		s.SetModel(catalog.RuntimeVLLM, "dual", catalog.ModelState{
			VariantID: "bf16", HFRepo: "acme/dual", LocalPath: "/models/hf/acme__dual", State: catalog.ModelStateReady,
		})
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// `waired models ls` and the setup row show the serving engine's records
// (owner decision 2026-09-22: only the current engine's models).
func TestEngineRows_TheListAndTheSetupRowAreTheServingEngines(t *testing.T) {
	for _, tc := range []struct {
		engine      string
		wantState   string
		wantVariant string
	}{
		{catalog.RuntimeOllama, catalog.ModelStateNotPresent, ""},
		{catalog.RuntimeVLLM, catalog.ModelStateReady, "bf16"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			p := vllmOnlyProvider(t, tc.engine)
			entries := p.ListModels(context.Background())
			if len(entries) != 1 || entries[0].State != tc.wantState || entries[0].VariantID != tc.wantVariant {
				t.Errorf("ListModels = %+v, want state %q variant %q", entries, tc.wantState, tc.wantVariant)
			}
			if st, _, _ := p.setupModelState("dual"); st != tc.wantState {
				t.Errorf("setupModelState = %q, want %q", st, tc.wantState)
			}
			// No record reads as not present, the way the list shows it.
			if got := stateOrDefault(p.bundledModelState("dual").State, catalog.ModelStateNotPresent); got != tc.wantState {
				t.Errorf("bundledModelState = %q, want %q", got, tc.wantState)
			}
		})
	}
}

// What Active names is read from the Active engine's records: an ollama
// selection of a model only vLLM has fetched has no name ollama answers
// to, and is not ready.
func TestEngineRows_AnOllamaSelectionOfAModelOnlyVLLMHas(t *testing.T) {
	p := vllmOnlyProvider(t, catalog.RuntimeOllama)
	if err := p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "dual", VariantID: "bf16"}
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.activeEngineModel(); got != "" {
		t.Errorf("activeEngineModel = %q, want empty: ollama has no model by that name", got)
	}
	if a, s := p.activeEngineTags(); a != "" || s != "" {
		t.Errorf("activeEngineTags = (%q, %q), want two empties", a, s)
	}
	p.registry = infruntime.NewRegistry()
	st, _ := p.store.Load()
	f := p.subsystemFacts(context.Background(), hardware.Profile{}, st)
	if f.ModelKnown || f.ModelState == catalog.ModelStateReady {
		t.Errorf("subsystemFacts = known %v state %q, want the model unknown to ollama", f.ModelKnown, f.ModelState)
	}
}

// The mirror: a vLLM selection reads vLLM's records, so the repo id is the
// name, and ollama's record of the same model does not stand in.
func TestEngineRows_AVLLMSelectionReadsVLLMsRecord(t *testing.T) {
	p := vllmOnlyProvider(t, catalog.RuntimeVLLM)
	if err := p.store.Update(func(s *catalog.State) {
		s.SetModel(catalog.RuntimeOllama, "dual", catalog.ModelState{VariantID: "q8", OllamaTag: "dual:q8", State: catalog.ModelStateReady})
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "dual", VariantID: "bf16"}
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.activeEngineModel(); got != "acme/dual" {
		t.Errorf("activeEngineModel = %q, want acme/dual", got)
	}
}

// The router is told which engine's records are this device's.
func TestBaseRouterInputs_NamesTheServingEngine(t *testing.T) {
	for _, engine := range []string{catalog.RuntimeOllama, catalog.RuntimeVLLM} {
		p := vllmOnlyProvider(t, engine)
		p.registry = infruntime.NewRegistry()
		p.profiler = hardware.NewProfiler(t.TempDir(),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 16, 16, nil }),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			}),
			hardware.WithEngineVersion(func(context.Context, string) (bool, string) { return false, "" }),
			hardware.WithUMA(func(context.Context, *hardware.Profile) {}),
		)
		if got := p.baseRouterInputs(context.Background()).ServingEngine; got != engine {
			t.Errorf("serving with %s: router inputs name %q", engine, got)
		}
	}
}

// A vLLM build is switched to by the vLLM start path: commitBuild moves
// only ollama's records between builds, and an endpoint of the other engine
// keeps the build it names.
func TestCommitBuild_PerEngine(t *testing.T) {
	st := catalog.State{}
	st.SetModel(catalog.RuntimeOllama, "dual", catalog.ModelState{VariantID: "q4", State: catalog.ModelStateReady})
	st.SetModel(catalog.RuntimeVLLM, "dual", catalog.ModelState{VariantID: "bf16", State: catalog.ModelStateReady})
	st.StagedVariants = map[string]catalog.ModelState{"dual": {VariantID: "q8", State: catalog.ModelStateReady}}
	st.Endpoints = map[string]catalog.EndpointState{
		"ollama": {Runtime: catalog.RuntimeOllama, ModelID: "dual", VariantID: "q4"},
		"vllm":   {Runtime: catalog.RuntimeVLLM, ModelID: "dual", VariantID: "bf16"},
	}

	// q8 is ollama's staged build: it is never vLLM's.
	if commitBuild(&st, catalog.RuntimeVLLM, "dual", "q8") {
		t.Error("commitBuild moved vLLM's record onto ollama's staged build")
	}
	if !commitBuild(&st, catalog.RuntimeOllama, "dual", "q8") {
		t.Fatal("commitBuild did not promote ollama's staged build")
	}
	if ms, _ := st.ModelFor(catalog.RuntimeOllama, "dual"); ms.VariantID != "q8" {
		t.Errorf("ollama's record = %q, want q8", ms.VariantID)
	}
	if ms, _ := st.ModelFor(catalog.RuntimeVLLM, "dual"); ms.VariantID != "bf16" {
		t.Errorf("vLLM's record = %q, want bf16 untouched", ms.VariantID)
	}
	if e := st.Endpoints["vllm"]; e.VariantID != "bf16" {
		t.Errorf("vLLM's endpoint = %q, want bf16 untouched", e.VariantID)
	}
	if e := st.Endpoints["ollama"]; e.VariantID != "q8" {
		t.Errorf("ollama's endpoint = %q, want q8", e.VariantID)
	}
}
