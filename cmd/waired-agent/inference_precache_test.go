package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// precacheProvider is a CPU host with room for both fixture models, so
// which model gets pre-cached is decided by the picker rather than by
// whatever RAM the test machine happens to have.
func precacheProvider(t *testing.T, r download.CommandRunner) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		store:     catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:       agentconfig.InferenceConfig{AllowPull: true},
		manifests: recTestManifests(), // heavy (tier 50) / light (tier 20)
		puller:    download.NewPuller("ollama-fake", r),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentCtx:  context.Background(),
		profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 16, 16, nil }),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			}),
			hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
				return name == "ollama", "0.31.0"
			}),
		),
	}
}

// PRODUCT CONTRACT: pre-caching an UPDATE presupposes something to
// update.
//
// computeAvailableUpdate has a "No active yet -> the picker output is
// itself the update" branch, which is right for the /inference/status
// field it also feeds but wrong as a trigger to download: on a fresh
// install state.Active is nil, so the 30-second pre-cache goroutine
// dispatched a THIRD multi-GB pull alongside the operator's model and the
// bundled fallback (#306).
func TestMaybePreCache_SkipsWhenNothingIsActive(t *testing.T) {
	r := &scriptedRunner{results: []error{nil}}
	p := precacheProvider(t, r)

	p.maybePreCache(context.Background())
	p.waitForPulls()

	if got := r.calls(); got != 0 {
		t.Fatalf("pulls dispatched with no active model = %d, want 0", got)
	}
}

// The other half of the contract: this is a suppression, not a removal.
// A host that IS serving a model must still pre-fetch the better
// candidate, so `waired runtimes refresh` stays a near-instant swap.
func TestMaybePreCache_StillRunsWhenAModelIsActive(t *testing.T) {
	r := &scriptedRunner{results: []error{nil}}
	p := precacheProvider(t, r)
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["light"] = catalog.ModelState{
			State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "light:2b",
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "light", VariantID: "q4",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	p.maybePreCache(context.Background())
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pulls dispatched while serving the lighter model = %d, want 1 "+
			"(the better candidate is still worth pre-fetching)", got)
	}
	if got := modelStateOf(t, p, "heavy").State; got != catalog.ModelStateReady {
		t.Fatalf("pre-cached model state = %q, want %q", got, catalog.ModelStateReady)
	}
}
