package router

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestPhase7Integration_StickyAffinityHoldsAcrossManyRequests is the
// flagship behaviour: when the same X-Waired-Conversation-Id (passed
// through Request.StickyID) hits the gateway repeatedly, every
// request must pin to the same peer until that peer goes stale or
// over-capacity. Mirrors the llm-d "session affinity" benchmark
// scenario at a single-process scope: cache hits track 1:1 with
// peers that the Selector keeps pinning.
func TestPhase7Integration_StickyAffinityHoldsAcrossManyRequests(t *testing.T) {
	manifest := qwen()
	manifest.Variants[0].ParamCount = 8_000_000_000
	manifest.Variants[0].QuantizationTier = 4

	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 8),
			mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 8),
			mkPeerWithCap("peer-C", "qwen3:8b-q4_K_M", 8),
		},
	}
	sticky := NewStickyStore(5*time.Minute, time.Now)
	tracker := NewInFlightTracker()

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{manifest},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		Sticky:         sticky,
		LocalInFlight:  tracker,
	})

	// 20 requests sharing the same conversation. Selector should pin
	// them all to whoever wins the first selection.
	pin := ""
	for i := 0; i < 20; i++ {
		sel, err := s.Select(t.Context(), Request{Model: "waired/default", StickyID: "conv-flagship"})
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		if pin == "" {
			pin = sel.Runtime
		}
		if sel.Runtime != pin {
			t.Errorf("req %d: routed to %q, want pinned %q (sticky regressed)", i, sel.Runtime, pin)
		}
		sel.Release()
	}
}

// TestPhase7Integration_OverflowBalancesAcrossPeers covers the
// "primary peer saturated, overflow goes elsewhere" path. Saturate
// peer-A to its cap, then send 6 more requests with **unique**
// conversation IDs (no sticky bias). They must distribute across the
// remaining peers under capacity.
func TestPhase7Integration_OverflowBalancesAcrossPeers(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 2),
			mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 4),
			mkPeerWithCap("peer-C", "qwen3:8b-q4_K_M", 4),
		},
	}
	tracker := NewInFlightTracker()
	// Saturate peer-A entirely.
	rA1, _ := tracker.Acquire("peer-A", 2)
	rA2, _ := tracker.Acquire("peer-A", 2)
	defer rA1()
	defer rA2()

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})

	picks := map[string]int{}
	releases := []func(){}
	for i := 0; i < 6; i++ {
		sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		picks[sel.Runtime]++
		releases = append(releases, sel.Release)
	}
	if picks["remote:peer-A"] != 0 {
		t.Errorf("saturated peer-A received %d requests; want 0", picks["remote:peer-A"])
	}
	if picks["remote:peer-B"]+picks["remote:peer-C"] != 6 {
		t.Errorf("overflow: peer-B+peer-C got %d/%d/%d (want sum to 6)",
			picks["remote:peer-B"], picks["remote:peer-C"], 6)
	}
	for _, r := range releases {
		r()
	}
}

// TestPhase7Integration_AllPeersSaturatedReturns503Sentinel exercises
// the ErrAllPeersOverloaded path that the gateway will turn into
// HTTP 503 waired_all_peers_overloaded.
func TestPhase7Integration_AllPeersSaturatedReturns503Sentinel(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 1),
			mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	rA, _ := tracker.Acquire("peer-A", 1)
	rB, _ := tracker.Acquire("peer-B", 1)
	defer rA()
	defer rB()

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})

	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrAllPeersOverloaded) {
		t.Fatalf("want ErrAllPeersOverloaded; got %v", err)
	}
}

// TestPhase7Integration_ScoreWinsOverDeviceIDAsc proves the Phase 7
// score axis actually overrides the legacy deviceID-asc behaviour.
// Two peers carry the same model from manifests with DIFFERENT
// Phase 7 scores; the higher-score peer wins even though deviceID-asc
// would have picked the other.
func TestPhase7Integration_ScoreWinsOverDeviceIDAsc(t *testing.T) {
	// peer-A serves a low-score variant (8B q4, score 32e9).
	// peer-Z serves a high-score variant (70B fp16, score 560e9).
	// We use two manifests with the SAME ollama tag to force a
	// model match on both peers; the variant the candidate picks
	// up is whichever manifest is the alias resolution target.
	//
	// Since LookupByAlias returns the first manifest that matches,
	// and Selector.Select aliases through it, we craft a single
	// manifest with both variants present. Both peers carry the
	// same Models tag; the Selector resolves to manifest-only
	// scoring and picks the variant whose Source.Tag matches.

	manifest := catalog.Manifest{
		ModelID:       "qwen3-mega",
		ModelAliases:  []string{"waired/mega"},
		ContextLength: 8192,
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{
			{
				VariantID:        "small-q4",
				Format:           catalog.FormatOllamaTag,
				RuntimeSupport:   []string{catalog.RuntimeOllama},
				ParamCount:       8_000_000_000,
				QuantizationTier: 4,
				QualityTier:      40,
				Source:           catalog.VariantSource{Type: "ollama", Tag: "small:tag"},
			},
			{
				VariantID:        "mega-fp16",
				Format:           catalog.FormatOllamaTag,
				RuntimeSupport:   []string{catalog.RuntimeOllama},
				ParamCount:       70_000_000_000,
				QuantizationTier: 8,
				QualityTier:      90,
				Source:           catalog.VariantSource{Type: "ollama", Tag: "mega:tag"},
			},
		},
	}
	// peer-A carries only "small:tag" (low score); peer-Z carries
	// "mega:tag" (high score). Without score, peer-A would win on
	// deviceID-asc; with score, peer-Z must win.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID: "peer-A",
				InferenceState: &signer.InferenceState{
					Reachable: true, Type: signer.InferenceTypeOllama,
					Models: []string{"small:tag"}, LastCheck: "2026-05-14T18:00:00Z",
				},
			},
			{
				DeviceID: "peer-Z",
				InferenceState: &signer.InferenceState{
					Reachable: true, Type: signer.InferenceTypeOllama,
					Models: []string{"mega:tag"}, LastCheck: "2026-05-14T18:00:00Z",
				},
			},
		},
	}

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{manifest},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/mega"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("score axis should win: got %q, want remote:peer-Z (mega-fp16 score=560e9 > small-q4 32e9)", sel.Runtime)
	}
}

// TestPhase7Integration_ReleaseFreesSlotForRetry mirrors the
// gateway's defer-release contract: after a request completes,
// the slot becomes available immediately, so the next request can
// fill the same peer.
func TestPhase7Integration_ReleaseFreesSlotForRetry(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})

	// First request fills the only slot.
	sel1, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("req 1: %v", err)
	}
	// Second request immediately afterwards should fail (cap=1).
	_, err2 := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err2, ErrAllPeersOverloaded) {
		t.Fatalf("req 2 (cap full): want ErrAllPeersOverloaded; got %v", err2)
	}
	// Release the first.
	sel1.Release()
	// Third request should now succeed.
	sel3, err3 := s.Select(t.Context(), Request{Model: "waired/default"})
	if err3 != nil {
		t.Fatalf("req 3 (slot freed): %v", err3)
	}
	if sel3.Runtime != "remote:peer-A" {
		t.Errorf("expected remote:peer-A; got %q", sel3.Runtime)
	}
	sel3.Release()
}

// TestPhase7Integration_ConcurrentSelectsRespectCapacity runs many
// goroutines selecting against a shared tracker; the in-flight count
// must never exceed Capacity at any sampling point. Run with -race.
func TestPhase7Integration_ConcurrentSelectsRespectCapacity(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 4),
		},
	}
	tracker := NewInFlightTracker()
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	successes := 0
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
				if err != nil {
					continue
				}
				mu.Lock()
				successes++
				mu.Unlock()
				if got := tracker.InFlight("peer-A"); int(got) > 4 {
					t.Errorf("InFlight %d > cap 4 during race", got)
				}
				sel.Release()
			}
		}()
	}
	wg.Wait()
	if got := tracker.InFlight("peer-A"); got != 0 {
		t.Errorf("after balanced release: InFlight=%d, want 0", got)
	}
	if successes == 0 {
		t.Error("no requests succeeded across 16×50 attempts; admission may be over-tight")
	}
}
