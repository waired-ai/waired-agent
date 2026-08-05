package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// count reports how many times the fake engine has been (re)spawned — one per
// EnsureRunning, so a swap bounce (Stop → EnsureRunning) increments it.
func (s *fakeSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newSwapTestAdapter(t *testing.T) (*infruntime.OllamaAdapter, *fakeSpawner) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	sp := &fakeSpawner{}
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: sp, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	return a, sp
}

func cpuSwapProfiler(t *testing.T) *hardware.Profiler {
	t.Helper()
	return hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			return name == "ollama", "0.31.0"
		}),
	)
}

// TestEffectivePreferredModel_OverrideBeatsStaleCfg is the #812 regression guard
// for the frozen-cfg blocker: cfg.PreferredModelID is a boot snapshot, so after
// an in-process switch every reader must resolve the override, not the snapshot.
func TestEffectivePreferredModel_OverrideBeatsStaleCfg(t *testing.T) {
	manifests := recTestManifests() // "heavy", "light"
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"light": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "light:2b"},
		}
	}); err != nil {
		t.Fatal(err)
	}
	p := &agentInferenceProvider{
		cfg:       agentconfig.InferenceConfig{PreferredModelID: "heavy"},
		manifests: manifests,
		store:     store,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Before any switch: effective == the frozen boot cfg.
	if got := p.effectivePreferredModelID(); got != "heavy" {
		t.Fatalf("pre-swap effectivePreferredModelID = %q, want heavy", got)
	}
	// Simulate an in-process switch to "light".
	id := "light"
	p.preferredOverride.Store(&id)

	if got := p.effectivePreferredModelID(); got != "light" {
		t.Errorf("effectivePreferredModelID = %q, want light", got)
	}
	if m, ok := p.preferredManifest(); !ok || m.ModelID != "light" {
		t.Errorf("preferredManifest = %+v ok=%v, want light", m, ok)
	}
	if got := p.effectiveCfg().PreferredModelID; got != "light" {
		t.Errorf("effectiveCfg().PreferredModelID = %q, want light", got)
	}
	st, _ := store.Load()
	if got := defaultCodingModelID(p.effectiveCfg(), st); got != "light" {
		t.Errorf("defaultCodingModelID = %q, want light (coding aliases must follow the switch)", got)
	}
	if tm, _, ok := resolveTuningTarget(p.effectiveCfg(), manifests, st); !ok || tm.ModelID != "light" {
		t.Errorf("resolveTuningTarget = %+v ok=%v, want light (serve tuning must follow the switch)", tm, ok)
	}
}

// TestSwapPreferredModel_CrossEngineNeedsRestart: a switch that cannot apply in
// process (here the host serves vLLM) returns errSwapNeedsRestart so the handler
// falls back to the supervised restart.
func TestSwapPreferredModel_CrossEngineNeedsRestart(t *testing.T) {
	p := &agentInferenceProvider{
		cfg:       agentconfig.InferenceConfig{},
		manifests: recTestManifests(),
		store:     catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	p.setServingEngine(catalog.RuntimeVLLM) // serving vLLM → no in-process ollama swap
	if _, err := p.SwapPreferredModel(t.Context(), "heavy"); !errors.Is(err, errSwapNeedsRestart) {
		t.Errorf("cross-engine SwapPreferredModel err = %v, want errSwapNeedsRestart", err)
	}
}

// TestSwapPreferredModel_OnDiskFlipsActiveAndBounces: an on-disk same-engine
// switch flips Active to the new model and bounces the ollama subprocess in
// process (no whole-agent restart).
func TestSwapPreferredModel_OnDiskFlipsActiveAndBounces(t *testing.T) {
	manifests := recTestManifests()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"heavy": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "heavy:8b"},
			"light": {State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "light:2b"},
		}
		s.Active = &catalog.ActiveSelection{
			Runtime: catalog.RuntimeOllama, ModelID: "heavy", VariantID: "q4", DecidedBy: "auto",
		}
	}); err != nil {
		t.Fatal(err)
	}

	adapter, sp := newSwapTestAdapter(t)
	if err := adapter.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	spawnsBefore := sp.count()

	p := &agentInferenceProvider{
		cfg:       agentconfig.InferenceConfig{PreferredModelID: "heavy", BundledModelID: "heavy"},
		manifests: manifests,
		store:     store,
		ollama:    adapter,
		profiler:  cpuSwapProfiler(t),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	downloading, err := p.SwapPreferredModel(context.Background(), "light")
	if err != nil {
		t.Fatalf("SwapPreferredModel: %v", err)
	}
	if downloading {
		t.Errorf("light is on disk; downloading should be false")
	}
	if got := p.effectivePreferredModelID(); got != "light" {
		t.Errorf("override not published: effectivePreferredModelID = %q, want light", got)
	}

	// The reconcile runs asynchronously on the agent context: wait for Active
	// to flip to "light" AND the engine to have re-spawned (the bounce).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := store.Load()
		if st.Active != nil && st.Active.ModelID == "light" && sp.count() > spawnsBefore {
			if st.Active.DecidedBy != "user" {
				t.Errorf("Active.DecidedBy = %q, want user", st.Active.DecidedBy)
			}
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := store.Load()
	t.Fatalf("swap did not complete: Active=%+v spawns=%d (before=%d)", st.Active, sp.count(), spawnsBefore)
}

// TestSwapPreferredModel_PullDispatchFailureIsReported is
// waired-agent#257: a switch whose download could not even be STARTED
// used to return (false, nil), which every caller read as "applied".
//
// Product contract, not a record of today's behaviour: the setup
// reconciler derives its model_pull row from what this returns, and a
// nil error there left that row pending forever — admission is once per
// desired model value, so nothing ever retried it.
//
// The two things the error has to carry are asserted separately: the
// seam sentinel, which tells the management handler that restarting
// would not help, and the CAUSE sentinel, which is what
// classifyModelRejection reads to pick the §7 code the wizard renders.
func TestSwapPreferredModel_PullDispatchFailureIsReported(t *testing.T) {
	p := &agentInferenceProvider{
		// Pulls turned off: PullModel refuses before it touches the network,
		// which is the failure mode #257 was reported from.
		cfg:       agentconfig.InferenceConfig{AllowPull: false},
		manifests: recTestManifests(),
		store:     catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		profiler:  cpuSwapProfiler(t),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	downloading, err := p.SwapPreferredModel(t.Context(), "light")
	if err == nil {
		t.Fatalf("SwapPreferredModel err = nil; a dispatch that never started must not read as applied")
	}
	if !errors.Is(err, management.ErrModelSwitchUnavailable) {
		t.Errorf("err = %v, want it to wrap management.ErrModelSwitchUnavailable "+
			"(the handler falls back to a pointless restart without it)", err)
	}
	if !errors.Is(err, errPullsDisabled) {
		t.Errorf("err = %v, want the cause sentinel kept in the chain "+
			"(classifyModelRejection reads it)", err)
	}
	if downloading {
		t.Errorf("downloading = true after a failed dispatch")
	}
	if psm := p.pendingSwapModel.Load(); psm != nil {
		t.Errorf("pendingSwapModel = %q, want cleared — no pull is coming to complete it", *psm)
	}
	// The published preference is deliberately KEPT: it is the operator's
	// stated choice and it applies by itself once pulls are possible again.
	if got := p.effectivePreferredModelID(); got != "light" {
		t.Errorf("effectivePreferredModelID = %q, want light (the choice is not rolled back)", got)
	}
}
