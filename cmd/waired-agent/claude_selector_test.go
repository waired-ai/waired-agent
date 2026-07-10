package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

type stubOllamaAdapter struct{}

func (stubOllamaAdapter) Name() string                        { return "ollama" }
func (stubOllamaAdapter) EnsureRunning(context.Context) error { return nil }
func (stubOllamaAdapter) Stop(context.Context) error          { return nil }
func (stubOllamaAdapter) BaseURL() string                     { return "http://stub" }
func (stubOllamaAdapter) Health(context.Context) infruntime.Health {
	return infruntime.Health{State: infruntime.StateReady}
}

func claudeSelectorManifests() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID:      "small-local",
			Capabilities: []string{"chat"},
			Runtime:      catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
			Variants: []catalog.Variant{{
				VariantID:      "q4",
				Format:         catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       1,
				Source:         catalog.VariantSource{Type: "ollama", Tag: "small:1b"},
			}},
		},
		{
			ModelID:      "big-peer",
			Capabilities: []string{"chat"},
			Runtime:      catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
			Variants: []catalog.Variant{{
				VariantID:        "q4",
				Format:           catalog.FormatOllamaTag,
				RuntimeSupport:   []string{catalog.RuntimeOllama},
				MinRAMGB:         1,
				ParamCount:       32,
				QuantizationTier: 4,
				Source:           catalog.VariantSource{Type: "ollama", Tag: "big:32b"},
			}},
		},
	}
}

// newClaudeSelectorProvider builds a provider whose local engine serves
// small-local (ready + active) and whose mesh snapshot is supplied by
// the test.
func newClaudeSelectorProvider(t *testing.T, snap func() inferencemesh.Snapshot) *agentInferenceProvider {
	t.Helper()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Update(func(s *catalog.State) {
		if s.Models == nil {
			s.Models = map[string]catalog.ModelState{}
		}
		s.Models["small-local"] = catalog.ModelState{
			State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "small:1b",
		}
		s.Active = &catalog.ActiveSelection{ModelID: "small-local", VariantID: "q4", Runtime: catalog.RuntimeOllama}
	}); err != nil {
		t.Fatal(err)
	}
	profiler := hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			return name == "ollama", "0.31.0"
		}),
	)
	reg := infruntime.NewRegistry()
	reg.Register(stubOllamaAdapter{})
	return &agentInferenceProvider{
		cfg:            agentconfig.InferenceConfig{BundledModelID: "small-local"},
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		manifests:      claudeSelectorManifests(),
		store:          store,
		profiler:       profiler,
		registry:       reg,
		meshSnapshotFn: snap,
	}
}

func peerSnapshot(models ...string) inferencemesh.Snapshot {
	return inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:  "peer-X",
			OverlayIP: "100.96.0.10",
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Models:    models,
				LastCheck: "2099-01-01T00:00:00Z",
			},
		}},
	}
}

// withRouting installs a fixed worker routing preference on the provider —
// the node-selection knob the claudeSelector now follows (unified with
// general inference; node choice is no longer a Claude-specific policy).
func withRouting(p *agentInferenceProvider, pref state.RoutingPreference) *agentInferenceProvider {
	p.routing = func() state.RoutingPreference { return pref }
	return p
}

type fallbackRecord struct {
	class, peer, reason string
	count               int
}

func (f *fallbackRecord) hook(class, peer, reason string) {
	f.class, f.peer, f.reason = class, peer, reason
	f.count++
}

func TestClaudeSelector_WorkerPinnedServesRemote(t *testing.T) {
	snap := peerSnapshot("big:32b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "peer-X"})
	var rec fallbackRecord
	sel := &claudeSelector{p: p, onNodeFallback: rec.hook}

	// The gateway resolves the claude-* id via the resolver first; here we
	// hand the selector the peer-resolved model directly.
	cands, err := sel.SelectK(t.Context(), router.Request{Model: "big-peer", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "remote" || cands[0].PeerID != "peer-X" {
		t.Fatalf("candidate = %+v, want remote on peer-X", cands)
	}
	if rec.count != 0 {
		t.Fatalf("no fallback expected, got %+v", rec)
	}
}

func TestClaudeSelector_PinnedPeerGoneFallsBackLocal(t *testing.T) {
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return inferencemesh.Snapshot{} }),
		state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "peer-X"})
	var rec fallbackRecord
	sel := &claudeSelector{p: p, onNodeFallback: rec.hook}

	// The peer-resolved model isn't on local disk; the local retry must
	// re-target the device-active model rather than 503 the turn.
	cands, err := sel.SelectK(t.Context(), router.Request{Model: "big-peer", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK after fallback: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "local" || cands[0].ModelID != "small-local" {
		t.Fatalf("candidate = %+v, want local small-local", cands)
	}
	if rec.count != 1 || rec.class != state.ClaudeClassMain || rec.peer != "peer-X" || rec.reason != "unreachable" {
		t.Fatalf("fallback record = %+v", rec)
	}
}

func TestClaudeSelector_SubFollowsWorkerPref(t *testing.T) {
	// Node selection is unified: a subagent request follows the same worker
	// preference as any other, so a pinned worker serves it remotely too.
	snap := peerSnapshot("big:32b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "peer-X"})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "big-peer", Class: state.ClaudeClassSub}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "remote" || cands[0].PeerID != "peer-X" {
		t.Fatalf("candidate = %+v, want remote on peer-X", cands)
	}
}

func TestClaudeSelector_WorkerLocalOnlyServesLocal(t *testing.T) {
	snap := peerSnapshot("big:32b", "small:1b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModeLocalOnly})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "small-local", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "local" {
		t.Fatalf("candidate = %+v, want local", cands)
	}
}

func TestClaudeSelector_NilRoutingDefaultsLocal(t *testing.T) {
	// No worker routing wired → workerPref() defaults to auto; a request for a
	// model only this device serves stays local.
	snap := peerSnapshot("big:32b")
	p := newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap })
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "small-local", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "local" {
		t.Fatalf("candidate = %+v, want local (privacy default)", cands)
	}
}
