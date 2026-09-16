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
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// retiredFallbackProvider is a provider over the SHIPPED catalog on a
// 24 GB NVIDIA host, so the fallback picks what the product picks there.
func retiredFallbackProvider(t *testing.T, cfg agentconfig.InferenceConfig) *agentInferenceProvider {
	t.Helper()
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	profiler := hardware.NewProfiler(t.TempDir(),
		hardware.WithOSArch(func() (string, string) { return "linux", "amd64" }),
		hardware.WithRAM(func(context.Context) (int, int, error) { return 64, 60, nil }),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return []hardware.GPU{{Vendor: "nvidia", Model: "test 24 GB", VRAMTotalMB: 24 * 1024}}, hardware.Accelerators{}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			return name == "ollama", infruntime.OllamaPinnedVersion
		}),
	)
	return &agentInferenceProvider{
		cfg:       cfg,
		manifests: manifests,
		store:     catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		profiler:  profiler,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The model a name retired with no successor falls back to is the one
// PickModel gives this host now (docs/decisions/20260916/0340, decision 4).
func wantRecommendedHere(t *testing.T, p *agentInferenceProvider) string {
	t.Helper()
	offered, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	pick, err := router.PickModel(router.PickInput{
		Catalog: offered, Hardware: p.profiler.Profile(context.Background()),
		Engine: catalog.RuntimeOllama, EngineVersion: infruntime.OllamaPinnedVersion,
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	return pick.Manifest.ModelID
}

func TestRetiredWithNoSuccessor_WrittenNamesFallBackToTheRecommendedModel(t *testing.T) {
	p := retiredFallbackProvider(t, agentconfig.InferenceConfig{
		PreferredModelID: "gpt-oss-20b",
		BundledModelID:   "openai/gpt-oss-120b",
	})
	want := wantRecommendedHere(t, p)
	if want == "" {
		t.Fatal("PickModel chose nothing on a 24 GB card; the fixture proves nothing")
	}

	if m, ok := p.preferredManifest(); !ok || m.ModelID != want {
		t.Errorf("preferredManifest = %q ok=%v, want the recommended %q", m.ModelID, ok, want)
	}
	if got := p.bundledModelID(); got != want {
		t.Errorf("bundledModelID = %q, want the recommended %q", got, want)
	}
	// waired/default follows the written choice, so it has to name a model
	// the router can look up by alias.
	st, _ := p.store.Load()
	if got := defaultCodingModelID(p.resolvedModelCfg(), st); got != want {
		t.Errorf("defaultCodingModelID = %q, want the recommended %q", got, want)
	}
}

// A name retired TO a successor still resolves to that successor, not to
// the host's pick: the fallback is only for a retirement with none.
func TestRetiredToASuccessor_WrittenNamesKeepTheSuccessor(t *testing.T) {
	p := retiredFallbackProvider(t, agentconfig.InferenceConfig{PreferredModelID: "qwen2.5-coder-0.5b"})
	if m, ok := p.preferredManifest(); !ok || m.ModelID != "qwen3.5-0.8b" {
		t.Errorf("preferredManifest = %q ok=%v, want the successor qwen3.5-0.8b", m.ModelID, ok)
	}
}

// An instruction given now is refused with what to do next, not
// substituted and not reported as unknown.
func TestRetiredWithNoSuccessor_InstructionsAreRefused(t *testing.T) {
	p := retiredFallbackProvider(t, agentconfig.InferenceConfig{AllowPull: true})
	const want = `"gpt-oss-20b" was retired with no replacement; choose another model`

	if _, err := p.PullModel(context.Background(), "gpt-oss-20b"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("PullModel err = %v, want %q", err, want)
	}
	if _, err := p.SwapPreferredBuild(context.Background(), "gpt-oss-20b", "", ""); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("SwapPreferredBuild err = %v, want %q", err, want)
	}
	if _, err := p.PullModel(context.Background(), "no-such-model"); err == nil || !strings.Contains(err.Error(), `unknown model "no-such-model"`) {
		t.Errorf("PullModel(unknown) err = %v, want the unknown-model answer", err)
	}
}
