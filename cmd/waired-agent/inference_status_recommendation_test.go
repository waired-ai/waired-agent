package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// statusRecProvider is the smallest provider Status(ctx) can be asked
// about: the recommendation ladder's store and manifests, a profiler with
// enough RAM for both fixture families, and a benchmark result.
func statusRecProvider(t *testing.T, bench BenchResult) *agentInferenceProvider {
	t.Helper()
	p := &agentInferenceProvider{
		store:     storeWithActive(t),
		manifests: recTestManifests(),
		cfg:       agentconfig.InferenceConfig{},
		logger:    slog.New(slog.DiscardHandler),
		registry:  infruntime.NewRegistry(),
		profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 16, 16, nil }),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			})),
	}
	p.SetLastBench(bench)
	return p
}

// PRODUCT CONTRACT (waired-agent#1150): the model-switch suggestion
// reaches the surfaces built to show it.
//
// InferenceStatus.BenchmarkRecommendation / .BenchmarkUpgrade were never
// assigned by anything, from the initial populate onwards. Everything
// downstream was in place and waiting: the catalog handler copies both
// fields (internal/management/inference_catalog.go), the tray renders
// "⚠ Lighter model recommended — switch to …" plus a confirmation popup
// from them (internal/gui/tray/state.go, tray.go), and four docs-site
// pages describe the feature. The row simply never appeared on any host.
func TestStatus_CarriesTheBenchmarkRecommendation(t *testing.T) {
	// 10 tok/s is well under the 60 interactive floor, and the ladder has
	// a lighter family that fits.
	p := statusRecProvider(t, BenchResult{TokensPerSec: 10, Capacity: 1})

	got := p.Status(context.Background())
	if got.BenchmarkRecommendation == nil {
		t.Fatal("a host measured far below the interactive floor offered no lighter model; " +
			"the tray row and its popup are unreachable")
	}
	if got.BenchmarkRecommendation.ToModelID != "light" {
		t.Errorf("ToModelID = %q, want the lighter family",
			got.BenchmarkRecommendation.ToModelID)
	}
	if got.BenchmarkUpgrade != nil {
		t.Errorf("both directions were offered at once: %+v", got.BenchmarkUpgrade)
	}
}

// The inverse direction rides the same wiring, and the tray reads it
// through a different field precisely so an old client cannot render an
// upgrade as "local inference is slow — switch to the lighter model X".
func TestStatus_CarriesTheBenchmarkUpgrade(t *testing.T) {
	// storeWithActive serves "heavy"; measured fast, the ladder has
	// nothing above it, so the fixture below starts from "light".
	p := statusRecProvider(t, BenchResult{TokensPerSec: 400, Capacity: 4})
	if err := p.store.Update(func(s *catalog.State) { s.Active.ModelID = "light" }); err != nil {
		t.Fatalf("switch to the lighter family: %v", err)
	}

	got := p.Status(context.Background())
	if got.BenchmarkUpgrade == nil {
		t.Fatal("a host with headroom was offered nothing better")
	}
	if got.BenchmarkUpgrade.ToModelID != "heavy" {
		t.Errorf("ToModelID = %q, want the heavier family", got.BenchmarkUpgrade.ToModelID)
	}
	if got.BenchmarkRecommendation != nil {
		t.Errorf("both directions were offered at once: %+v", got.BenchmarkRecommendation)
	}
}

// A host with no measurement offers nothing, rather than comparing a zero
// rate against the floor and proposing a lighter model to somebody nobody
// has measured.
func TestStatus_NoBenchmarkOffersNothing(t *testing.T) {
	p := statusRecProvider(t, BenchResult{})
	p.benchMu.Lock()
	p.lastBench = nil
	p.benchMu.Unlock()

	got := p.Status(context.Background())
	if got.BenchmarkRecommendation != nil || got.BenchmarkUpgrade != nil {
		t.Errorf("an unmeasured host was offered a switch: lighter=%+v upgrade=%+v",
			got.BenchmarkRecommendation, got.BenchmarkUpgrade)
	}
}
