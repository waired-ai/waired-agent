package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
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
// InferenceStatus.BenchmarkRecommendation was never
// assigned by anything, from the initial populate onwards. Everything
// downstream was in place and waiting: the catalog handler copies both
// fields (internal/management/inference_catalog.go), the tray renders
// "⚠ Faster model recommended — switch to …" plus a confirmation popup
// from them (internal/gui/tray/state.go, tray.go), and four docs-site
// pages describe the feature. The row simply never appeared on any host.
func TestStatus_CarriesTheBenchmarkRecommendation(t *testing.T) {
	// 400 s per request is well over the line, and the ladder has a
	// lighter family that fits.
	p := statusRecProvider(t, BenchResult{TokensPerSec: 10, TurnSeconds: 400, Capacity: 1})

	got := p.Status(context.Background())
	if got.BenchmarkRecommendation == nil {
		t.Fatal("a host measured far over the line offered no lighter model; " +
			"the tray row and its popup are unreachable")
	}
	if got.BenchmarkRecommendation.ToModelID != "light" {
		t.Errorf("ToModelID = %q, want the lighter family",
			got.BenchmarkRecommendation.ToModelID)
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
	if got.BenchmarkRecommendation != nil {
		t.Errorf("an unmeasured host was offered a switch: %+v", got.BenchmarkRecommendation)
	}
}
