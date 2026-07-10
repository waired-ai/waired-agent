package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// noopRunner satisfies download.CommandRunner without spawning anything
// (the pull job goroutine PullModel launches must not touch a real
// binary in unit tests).
type noopRunner struct{}

func (noopRunner) Run(_ context.Context, _ string, _, _ []string, onLine func(string)) error {
	onLine("success")
	return nil
}

// failingRunner makes the pull job fail, simulating a transient registry
// error (the #614 trigger).
type failingRunner struct{}

func (failingRunner) Run(_ context.Context, _ string, _, _ []string, _ func(string)) error {
	return errors.New("simulated registry throttle")
}

func pullGateManifest(mtpOnly bool) catalog.Manifest {
	m := catalog.Manifest{
		ModelID: "dense-mtp",
		Variants: []catalog.Variant{
			{
				VariantID: "mtp-q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				QualityTier:    71, ParamCount: 1, QuantizationTier: 4,
				MinEngineVersion: "0.30.0",
				Source:           catalog.VariantSource{Type: catalog.SourceOllama, Tag: "dense:mtp-q4"},
			},
			{
				VariantID: "q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				QualityTier:    70, ParamCount: 1, QuantizationTier: 4,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "dense:q4"},
			},
		},
	}
	if mtpOnly {
		m.Variants = m.Variants[:1]
	}
	return m
}

func pullGateProvider(t *testing.T, m catalog.Manifest) *agentInferenceProvider {
	t.Helper()
	return pullGateProviderWithRunner(t, m, noopRunner{})
}

func pullGateProviderWithRunner(t *testing.T, m catalog.Manifest, runner download.CommandRunner) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		store:     catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:       agentconfig.InferenceConfig{AllowPull: true},
		manifests: []catalog.Manifest{m},
		puller:    download.NewPuller("ollama-fake", runner),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		// p.ollama / p.profiler nil → ollamaEngineVersion() == "" (unknown),
		// which fails closed for floored variants only.
	}
}

// TestPullModel_SkipsGatedVariantOnUnknownEngine: with the engine
// version unknown, the pull must land on the unfloored variant instead
// of the mtp tag the engine may not be able to load.
func TestPullModel_SkipsGatedVariantOnUnknownEngine(t *testing.T) {
	p := pullGateProvider(t, pullGateManifest(false))
	job, err := p.PullModel(context.Background(), "dense-mtp")
	if err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	// Join the background pull goroutine before the test returns so it is
	// not still writing state.json into t.TempDir() when testing's
	// RemoveAll cleanup runs (issue #377). runPullJob only mutates State,
	// leaving VariantID/OllamaTag intact, so the assertion below is
	// unaffected.
	p.waitForPulls()
	st, _ := p.store.Load()
	ms := st.Models[job.ModelID]
	if ms.VariantID != "q4" || ms.OllamaTag != "dense:q4" {
		t.Errorf("queued variant = %s tag=%s, want q4 / dense:q4", ms.VariantID, ms.OllamaTag)
	}
}

// TestPullModel_RefusesWhenNoVariantPasses: an mtp-only family on a
// too-old / unknown engine must refuse with the upgrade remediation
// instead of dispatching a pull that fails server-side.
func TestPullModel_RefusesWhenNoVariantPasses(t *testing.T) {
	p := pullGateProvider(t, pullGateManifest(true))
	_, err := p.PullModel(context.Background(), "dense-mtp")
	if err == nil {
		t.Fatal("PullModel should refuse when no variant passes the engine gate")
	}
	for _, want := range []string{"requires ollama >= 0.30.0", "upgrade the engine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err.Error(), want)
		}
	}
}

// TestPullModel_FailedRefreshKeepsReady: a failed re-pull of a model that
// is already ready on disk must NOT flip it to failed — serving from the
// on-disk blobs stays healthy. Reproduces #614 (a registry hiccup during a
// harness re-pull took down serving that was fine seconds earlier).
func TestPullModel_FailedRefreshKeepsReady(t *testing.T) {
	m := pullGateManifest(false)
	p := pullGateProviderWithRunner(t, m, failingRunner{})

	// Seed the model as already ready on disk.
	if err := p.store.Update(func(s *catalog.State) {
		s.Models[m.ModelID] = catalog.ModelState{
			VariantID: "q4",
			OllamaTag: "dense:q4",
			State:     catalog.ModelStateReady,
			PulledAt:  time.Now().UTC(),
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	st, _ := p.store.Load()
	ms := st.Models[m.ModelID]
	if ms.State != catalog.ModelStateReady {
		t.Errorf("state = %q after failed refresh pull, want ready (serving must survive)", ms.State)
	}
	if ms.Error == "" {
		t.Errorf("Error should record the failed refresh for observability, got empty")
	}
}

// TestPullModel_FailedFreshPullMarksFailed: the sticky-ready guard must not
// over-reach — a model that was NOT ready still transitions to failed when
// its (first) pull errors.
func TestPullModel_FailedFreshPullMarksFailed(t *testing.T) {
	m := pullGateManifest(false)
	p := pullGateProviderWithRunner(t, m, failingRunner{})

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	st, _ := p.store.Load()
	if ms := st.Models[m.ModelID]; ms.State != catalog.ModelStateFailed {
		t.Errorf("state = %q after failed fresh pull, want failed", ms.State)
	}
}
