package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
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

func TestClaudeSelector_WorkerPinnedServesRemote(t *testing.T) {
	snap := peerSnapshot("big:32b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "peer-X"})
	sel := &claudeSelector{p: p}

	// The gateway resolves the claude-* id via the resolver first; here we
	// hand the selector the peer-resolved model directly.
	cands, err := sel.SelectK(t.Context(), router.Request{Model: "big-peer", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "remote" || cands[0].PeerID != "peer-X" {
		t.Fatalf("candidate = %+v, want remote on peer-X", cands)
	}
}

// TestClaudeSelector_PinnedPeerGoneFailsClosed pins the PRODUCT CONTRACT
// #325 settled: a pinned worker that is not in the mesh must surface
// ErrPinnedPeerUnreachable on the Claude surface exactly as it does on the
// general gateway. It INVERTS the former
// TestClaudeSelector_PinnedPeerGoneFallsBackLocal, which asserted the
// opposite (a silent local retry against the device-active model) — that
// retry is what made a pinned worker look dead while this machine's GPU
// quietly answered every turn.
func TestClaudeSelector_PinnedPeerGoneFailsClosed(t *testing.T) {
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return inferencemesh.Snapshot{} }),
		state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "peer-X"})
	sel := &claudeSelector{p: p}

	// small-local IS ready on this device, so a local retry would have
	// succeeded — the test is only meaningful because of that.
	cands, err := sel.SelectK(t.Context(), router.Request{Model: "big-peer", Class: state.ClaudeClassMain}, 1)
	if err == nil {
		t.Fatalf("pinned peer absent must fail, got candidates %+v", cands)
	}
	if !errors.Is(err, router.ErrPinnedPeerUnreachable) {
		t.Fatalf("error = %v, want ErrPinnedPeerUnreachable", err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = %+v, want none", cands)
	}
	// The error must name the pin so the gateway can report which worker
	// is down (the WARN emit used to log an empty peer_id).
	var pin *router.PinnedPeerUnreachableError
	if !errors.As(err, &pin) || pin.PeerDisplayID != "peer-X" {
		t.Fatalf("error does not name the pinned peer: %v", err)
	}
}

// TestClaudeSelector_PinnedPeerLacksModelFailsClosed is the second half of
// the same contract: the former retry also caught ErrModelNotReady, so a
// reachable pin with nothing servable fell back locally too. Both shapes
// now propagate.
func TestClaudeSelector_PinnedPeerLacksModelFailsClosed(t *testing.T) {
	// The pin is up and reachable but serves neither catalog model.
	snap := peerSnapshot("unrelated:7b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "peer-X"})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "small-local", Class: state.ClaudeClassMain}, 1)
	if err == nil {
		t.Fatalf("pinned peer without the model must fail, got candidates %+v", cands)
	}
	if !errors.Is(err, router.ErrModelNotReady) {
		t.Fatalf("error = %v, want ErrModelNotReady", err)
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

// TestClaudeSelector_PeerOnlyDoesNotFallBackLocal pins the PRODUCT
// CONTRACT that makes peer-only worth having on the Claude surface too
// (#327): peer-only must surface the error instead of quietly running the
// turn on this machine. #325 removed the last silent-local retry from this
// selector, so no mode may reintroduce one.
func TestClaudeSelector_PeerOnlyDoesNotFallBackLocal(t *testing.T) {
	// Empty mesh, and the local engine IS ready to serve small-local.
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return inferencemesh.Snapshot{} }),
		state.RoutingPreference{Mode: state.RoutingModePeerOnly})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "small-local", Class: state.ClaudeClassMain}, 1)
	if err == nil {
		t.Fatalf("peer-only with no peer must fail, got candidates %+v", cands)
	}
	if !errors.Is(err, router.ErrModelNotReady) {
		t.Fatalf("error = %v, want ErrModelNotReady", err)
	}
}

// TestClaudeSelector_PeerOnlyServesRemote is the positive half: with a
// peer that can serve, the turn runs there.
func TestClaudeSelector_PeerOnlyServesRemote(t *testing.T) {
	snap := peerSnapshot("big:32b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModePeerOnly})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "big-peer", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "remote" || cands[0].PeerID != "peer-X" {
		t.Fatalf("candidate = %+v, want remote on peer-X", cands)
	}
}

// TestNodeDirectivePref_CarriesTheOrderingPreferences is a defect found
// on real hardware: with `waired worker set --min-model-size=large`, the
// `claude-waired-peer` entry served a MEDIUM model, because the directive
// rebuilt the preference from the mode alone and dropped the floor.
//
// A /model directive says WHERE inference may run. Which of several
// admissible computers to prefer, and how small a model is acceptable,
// are separate axes set on a separate surface — the same shape
// waired-agent#1040 found on the pin, one field over.
func TestNodeDirectivePref_CarriesTheOrderingPreferences(t *testing.T) {
	operator := state.RoutingPreference{
		Mode:         state.RoutingModeAuto,
		Prefer:       state.RoutingPreferSize,
		MinModelSize: "large",
	}
	peers := []inferencemesh.PeerView{{DeviceID: "dev_named", DeviceName: "linux-gpu"}}
	cases := []struct {
		name      string
		directive string
	}{
		{"peer-only", gateway.ModelWairedPeer},
		{"public-only", gateway.ModelWairedPublic},
		{"a named machine", claudecode.PeerDirectiveID("linux-gpu")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := nodeDirectivePref(c.directive, peers, operator)
			if err != nil || !ok {
				t.Fatalf("nodeDirectivePref = (_, %v, %v)", ok, err)
			}
			if got.pref.Prefer != state.RoutingPreferSize {
				t.Errorf("Prefer = %q, want size — the directive said nothing about it", got.pref.Prefer)
			}
			if got.pref.MinModelSize != "large" {
				t.Errorf("MinModelSize = %q, want large — a /model pick does not lift the operator's floor",
					got.pref.MinModelSize)
			}
		})
	}

	// The pinned arm returns the operator's preference wholesale, so it
	// carries them by construction; pinned here as the property, not the
	// mechanism.
	pinned := state.RoutingPreference{
		Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_x",
		Prefer: state.RoutingPreferSpeed, MinModelSize: "medium",
	}
	got, ok, err := nodeDirectivePref(gateway.ModelWairedPeer, peers, pinned)
	if err != nil || !ok {
		t.Fatalf("pinned: (_, %v, %v)", ok, err)
	}
	if got.pref.MinModelSize != "medium" || got.pref.Prefer != state.RoutingPreferSpeed {
		t.Errorf("pinned arm dropped the ordering preferences: %+v", got.pref)
	}
}
