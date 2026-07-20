package router

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// publicPeerDeviceID is the single synthetic foreign identifier every
// leak assertion in this file keys off. It is deliberately obviously
// fake: waired-agent is a public repo and real device identifiers must
// never appear in fixtures.
const (
	publicPeerDeviceID = "dev_foreign00000001"
	publicPeerAlias    = "guest-a7f3"
)

// qwenTier is qwen() with an explicit quality tier so the auto-mode
// comparison has something to compare. The engine tag matches qwen()'s,
// which is what a peer advertises.
func qwenTier(tier int) catalog.Manifest {
	m := qwen()
	m.Variants[0].QualityTier = tier
	return m
}

// mkPublicPeer builds a Public Share provider peer: a foreign device
// injected by the control plane under a grant.
func mkPublicPeer(deviceID, pseudonym, tag string) inferencemesh.PeerView {
	p := mkPeer(deviceID, tag, true, false)
	p.Grant = &signer.PeerGrant{
		ID:        "grant_test0001",
		Kind:      "public",
		Role:      "provider",
		Pseudonym: pseudonym,
	}
	return p
}

func allowAll() PublicPolicy {
	return PublicPolicy{Mode: PublicModeExplicit, Consented: true, Main: true, Sub: true}
}

// publicSelector builds a Selector whose local model is NOT ready, so
// every Select falls through to the mesh — the only path public
// candidates can appear on.
func publicSelector(t *testing.T, policy PublicPolicy, peers ...inferencemesh.PeerView) (*Selector, *[]PublicNudge, *int) {
	t.Helper()
	var nudges []PublicNudge
	demands := 0
	snap := inferencemesh.Snapshot{Peers: peers}
	s := NewSelector(Inputs{
		Manifests:           []catalog.Manifest{qwenTier(50)},
		LocalState:          emptyState(),
		Hardware:            goodHardware(),
		Runtimes:            registryWithOllama(),
		MeshSnapshotFn:      func() inferencemesh.Snapshot { return snap },
		PublicPolicyFn:      func() PublicPolicy { return policy },
		OnPublicNudge:       func(n PublicNudge) { nudges = append(nudges, n) },
		OnPublicGrantDemand: func() { demands++ },
	})
	return s, &nudges, &demands
}

func TestPublicGate_ModeAndConsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy PublicPolicy
		want   bool
	}{
		{"zero value fails closed", PublicPolicy{}, false},
		{"off", PublicPolicy{Mode: PublicModeOff, Consented: true, Main: true, Sub: true}, false},
		{"explicit", PublicPolicy{Mode: PublicModeExplicit, Consented: true, Main: true, Sub: true}, true},
		{"both class toggles off", PublicPolicy{Mode: PublicModeExplicit, Consented: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := publicSelector(t, tc.policy,
				mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
			cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
			got := err == nil && len(cands) > 0
			if got != tc.want {
				t.Fatalf("public candidate admitted = %v, want %v (err=%v)", got, tc.want, err)
			}
		})
	}
}

func TestPublicGate_ClassToggles(t *testing.T) {
	for _, tc := range []struct {
		name       string
		class      string
		main, sub  bool
		wantAdmits bool
	}{
		{"main allowed", state.ClaudeClassMain, true, false, true},
		{"main blocked", state.ClaudeClassMain, false, true, false},
		{"sub allowed", state.ClaudeClassSub, false, true, true},
		{"sub blocked", state.ClaudeClassSub, true, false, false},
		// General (non-Claude) inference has no toggle of its own: it
		// rides on "am I willing to use strangers' machines at all".
		{"general with main on", "", true, false, true},
		{"general with sub on", "", false, true, true},
		{"general with both off", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := PublicPolicy{Mode: PublicModeExplicit, Consented: true, Main: tc.main, Sub: tc.sub}
			s, _, _ := publicSelector(t, policy,
				mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
			cands, err := s.SelectK(t.Context(), Request{Model: "waired/default", Class: tc.class}, 3)
			got := err == nil && len(cands) > 0
			if got != tc.wantAdmits {
				t.Fatalf("admitted = %v, want %v (err=%v)", got, tc.wantAdmits, err)
			}
		})
	}
}

func TestPublicGate_MinQualityTier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		floor   int
		wantOK  bool
		comment string
	}{
		{name: "no floor", floor: 0, wantOK: true},
		{name: "floor met exactly", floor: 50, wantOK: true},
		{name: "floor above peer", floor: 51, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := allowAll()
			policy.MinQualityTier = tc.floor
			s, _, _ := publicSelector(t, policy,
				mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
			cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
			got := err == nil && len(cands) > 0
			if got != tc.wantOK {
				t.Fatalf("admitted = %v, want %v", got, tc.wantOK)
			}
		})
	}
}

func TestPublicGate_AutoComparesAgainstOwnBestTier(t *testing.T) {
	public := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	own := mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false)

	t.Run("own peer ties the public tier so auto rejects", func(t *testing.T) {
		// Both advertise the same variant, so tiers are equal and the
		// strict > comparison excludes the stranger's machine.
		policy := allowAll()
		policy.Mode = PublicModeAuto
		s, _, _ := publicSelector(t, policy, own, public)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		for _, c := range cands {
			if c.PeerID == publicPeerDeviceID {
				t.Fatal("auto mode admitted a public peer that does not beat the own tier")
			}
		}
	})

	t.Run("explicit mode skips the comparison", func(t *testing.T) {
		policy := allowAll() // explicit
		s, _, _ := publicSelector(t, policy, own, public)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		found := false
		for _, c := range cands {
			if c.PeerID == publicPeerDeviceID {
				found = true
			}
		}
		if !found {
			t.Fatal("explicit mode did not admit the public peer")
		}
	})

	t.Run("no own nodes at all still admits under auto", func(t *testing.T) {
		// beat == 0, so any tier > 0 qualifies. A consumer with nothing
		// online is exactly who Public Share is for.
		policy := allowAll()
		policy.Mode = PublicModeAuto
		s, _, _ := publicSelector(t, policy, public)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil || len(cands) == 0 {
			t.Fatalf("auto mode rejected the only available node (err=%v, n=%d)", err, len(cands))
		}
	})
}

// TestPublicCandidates_NeverOutrankOwn covers both halves of the
// ordering guarantee: the sort key, and the re-partition that has to
// survive the sticky and pinned hoists (which move a candidate to index
// 0 keyed on deviceID alone).
func TestPublicCandidates_NeverOutrankOwn(t *testing.T) {
	public := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	own := mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false)

	t.Run("sort key", func(t *testing.T) {
		s, _, _ := publicSelector(t, allowAll(), public, own)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if len(cands) < 2 {
			t.Fatalf("want both candidates, got %d", len(cands))
		}
		if cands[0].PeerID != "dev_own00000001" {
			t.Fatalf("public peer outranked own: cands[0] = %q", cands[0].PeerID)
		}
	})

	t.Run("sticky binding to a public peer does not hoist it above own", func(t *testing.T) {
		// The regression this guards: makeMeshCandidate's commit closure
		// Touches the sticky store for public peers too, so one public
		// selection would otherwise pin the conversation to a stranger's
		// machine for the whole sticky TTL — even after an own node
		// comes back.
		sticky := NewStickyStore(5*time.Minute, time.Now)
		sticky.Touch("conv-1", publicPeerDeviceID)
		snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{public, own}}
		policy := allowAll()
		s := NewSelector(Inputs{
			Manifests:      []catalog.Manifest{qwenTier(50)},
			LocalState:     emptyState(),
			Hardware:       goodHardware(),
			Runtimes:       registryWithOllama(),
			MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
			PublicPolicyFn: func() PublicPolicy { return policy },
			Sticky:         sticky,
		})
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1"}, 5)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if cands[0].PeerID != "dev_own00000001" {
			t.Fatalf("sticky hoisted the public peer to index 0: %q", cands[0].PeerID)
		}
	})
}

// TestPublicCandidate_NeverExposesForeignDeviceID is the leak canary:
// the real foreign identifier must not appear in any field a display
// surface reads. Selection.Runtime is the one exception — it is
// functional (the peer adapter dials from it) and display sites
// substitute PeerDisplayID.
func TestPublicCandidate_NeverExposesForeignDeviceID(t *testing.T) {
	s, _, _ := publicSelector(t, allowAll(),
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
		mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	var public *Candidate
	for i := range cands {
		if cands[i].PeerID == publicPeerDeviceID {
			public = &cands[i]
		}
	}
	if public == nil {
		t.Fatal("public candidate not produced")
	}
	if public.PeerDisplayID != publicPeerAlias {
		t.Fatalf("PeerDisplayID = %q, want %q", public.PeerDisplayID, publicPeerAlias)
	}
	if strings.Contains(public.EndpointID, publicPeerDeviceID) {
		t.Errorf("EndpointID leaks the foreign device id: %q", public.EndpointID)
	}
	for _, r := range public.Decision.Reason {
		if strings.Contains(r, publicPeerDeviceID) {
			t.Errorf("Decision.Reason leaks the foreign device id: %q", r)
		}
	}
	// The own candidate's fallback trace lists the public peer as a
	// runner-up — by pseudonym only.
	for _, c := range cands {
		for _, f := range c.Decision.Fallback {
			if strings.Contains(f.EndpointID, publicPeerDeviceID) || strings.Contains(f.Runtime, publicPeerDeviceID) {
				t.Errorf("fallback trace leaks the foreign device id: %+v", f)
			}
		}
	}
	sel, ok := public.Commit()
	if !ok {
		t.Fatal("Commit failed")
	}
	if sel.PeerDisplayID != publicPeerAlias {
		t.Errorf("Selection.PeerDisplayID = %q", sel.PeerDisplayID)
	}
	if strings.Contains(sel.EndpointID, publicPeerDeviceID) {
		t.Errorf("Selection.EndpointID leaks: %q", sel.EndpointID)
	}
}

// TestPublicCandidate_SkippedWithoutPseudonym fails the peer closed: a
// grant with no pseudonym cannot be displayed safely, and routing to a
// peer we cannot name is worse than not routing to it.
func TestPublicCandidate_SkippedWithoutPseudonym(t *testing.T) {
	p := mkPublicPeer(publicPeerDeviceID, "", "qwen3:8b-q4_K_M")
	s, _, _ := publicSelector(t, allowAll(), p)
	if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil {
		t.Fatal("a grant peer with no pseudonym was admitted")
	}
}

// TestPublicConsumerGrant_NotARoutingTarget: a foreign peer present as a
// CONSUMER (a guest using our engine) must never be routed to.
func TestPublicConsumerGrant_NotARoutingTarget(t *testing.T) {
	p := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	p.Grant.Role = "consumer"
	s, _, _ := publicSelector(t, allowAll(), p)
	if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil {
		t.Fatal("a consumer-role grant peer was admitted as a routing target")
	}
}

func TestPublicGrantDemand_EmittedWhenNoGrantHeld(t *testing.T) {
	t.Run("policy admits but no grant peer in the map", func(t *testing.T) {
		s, _, demands := publicSelector(t, allowAll()) // empty snapshot
		if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil {
			t.Fatal("expected the request to fail with no candidates")
		}
		if *demands != 1 {
			t.Fatalf("demand signals = %d, want 1", *demands)
		}
	})

	t.Run("silent when a grant peer is already present", func(t *testing.T) {
		// A public peer exists but is excluded by the tier floor: we hold
		// a grant, so waking the acquirer would achieve nothing.
		policy := allowAll()
		policy.MinQualityTier = 99
		s, _, demands := publicSelector(t, policy,
			mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
		_, _ = s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if *demands != 0 {
			t.Fatalf("demand signals = %d, want 0", *demands)
		}
	})

	t.Run("silent when policy is off", func(t *testing.T) {
		s, _, demands := publicSelector(t, PublicPolicy{})
		_, _ = s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if *demands != 0 {
			t.Fatalf("demand signals = %d, want 0", *demands)
		}
	})
}

func TestPublicNudge_OnlyBeforeConsent(t *testing.T) {
	t.Run("unconsented and own nodes came up short", func(t *testing.T) {
		s, nudges, _ := publicSelector(t, PublicPolicy{}) // never consented
		_, _ = s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if len(*nudges) != 1 {
			t.Fatalf("nudges = %d, want 1", len(*nudges))
		}
		n := (*nudges)[0]
		if n.Reason != NudgeReasonNoCandidate {
			t.Errorf("Reason = %q", n.Reason)
		}
		if n.ModelID != "qwen3-8b-instruct" {
			t.Errorf("ModelID = %q", n.ModelID)
		}
	})

	t.Run("consented but switched off is never nudged", func(t *testing.T) {
		s, nudges, _ := publicSelector(t, PublicPolicy{Mode: PublicModeOff, Consented: true})
		_, _ = s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if len(*nudges) != 0 {
			t.Fatalf("nudges = %d, want 0 — the user already made this choice", len(*nudges))
		}
	})

	t.Run("not emitted when an own node serves the request", func(t *testing.T) {
		s, nudges, _ := publicSelector(t, PublicPolicy{},
			mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))
		if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if len(*nudges) != 0 {
			t.Fatalf("nudges = %d, want 0", len(*nudges))
		}
	})
}

// TestNilPublicPolicyAdmitsNothing pins the fail-closed default that
// loop prevention rests on: the overlay-side Selector leaves
// PublicPolicyFn unset, so even with a grant peer visible in the mesh a
// request arriving FROM a peer can never be re-routed onward to a public
// node. The mesh IS wired here, so the assertion is about the policy
// input and not about the absent snapshot.
func TestNilPublicPolicyAdmitsNothing(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
	}}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenTier(50)},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		// PublicPolicyFn deliberately nil — the overlay posture.
	})
	if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil {
		t.Fatal("a nil public policy admitted a grant peer")
	}
}

// TestPublicSideSignals_AllOverloadedPath covers the second shortfall
// exit: own nodes have the model but every one is at capacity.
func TestPublicSideSignals_AllOverloadedPath(t *testing.T) {
	own := mkPeerWithCap("dev_own00000001", "qwen3:8b-q4_K_M", 1)
	tracker := NewInFlightTracker()
	rel, ok := tracker.Acquire("dev_own00000001", 1)
	if !ok {
		t.Fatal("setup: could not fill the peer")
	}
	defer rel()

	var nudges []PublicNudge
	demands := 0
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{own}}
	s := NewSelector(Inputs{
		Manifests:           []catalog.Manifest{qwenTier(50)},
		LocalState:          emptyState(),
		Hardware:            goodHardware(),
		Runtimes:            registryWithOllama(),
		MeshSnapshotFn:      func() inferencemesh.Snapshot { return snap },
		LocalInFlight:       tracker,
		PublicPolicyFn:      func() PublicPolicy { return allowAll() },
		OnPublicNudge:       func(n PublicNudge) { nudges = append(nudges, n) },
		OnPublicGrantDemand: func() { demands++ },
	})

	_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if !errors.Is(err, ErrAllPeersOverloaded) {
		t.Fatalf("err = %v, want ErrAllPeersOverloaded", err)
	}
	if demands != 1 {
		t.Errorf("demand signals = %d, want 1", demands)
	}
	// Consented in this fixture, so no nudge — but the reason recorded
	// on the shortfall must be the overloaded one, which the demand
	// branch above proves was reached.
	if len(nudges) != 0 {
		t.Errorf("nudges = %d, want 0 (already consented)", len(nudges))
	}
}

// The side signals must NOT fire when the mesh came up empty but the
// request was still served — peer-preferred routing consults the mesh
// first and then falls through to a ready local engine.
func TestPublicSideSignals_SilentWhenServedLocally(t *testing.T) {
	var nudges []PublicNudge
	demands := 0
	snap := inferencemesh.Snapshot{} // no peers at all
	s := NewSelector(Inputs{
		Manifests:           []catalog.Manifest{qwenTier(50)},
		LocalState:          readyState(), // local engine CAN serve
		Hardware:            goodHardware(),
		Runtimes:            registryWithOllama(),
		MeshSnapshotFn:      func() inferencemesh.Snapshot { return snap },
		RoutingMode:         state.RoutingModePeerPreferred,
		PublicPolicyFn:      func() PublicPolicy { return allowAll() },
		OnPublicNudge:       func(n PublicNudge) { nudges = append(nudges, n) },
		OnPublicGrantDemand: func() { demands++ },
	})

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil || len(cands) == 0 {
		t.Fatalf("expected a local candidate (err=%v, n=%d)", err, len(cands))
	}
	if demands != 0 || len(nudges) != 0 {
		t.Fatalf("side signals fired for a request that ran locally: demands=%d nudges=%d", demands, len(nudges))
	}
}

// ownBestTier unions three sources; the peer loop is covered by the auto
// tests above, these cover the other two.
func TestOwnBestTier_LocalAndSelfSources(t *testing.T) {
	manifests := []catalog.Manifest{qwenTier(50)}

	t.Run("ready local variant", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: manifests, LocalState: readyState()})
		if got := s.ownBestTier(inferencemesh.Snapshot{}); got != 50 {
			t.Fatalf("ownBestTier = %d, want 50 from the ready local variant", got)
		}
	})

	t.Run("snapshot self", func(t *testing.T) {
		self := mkPeer("dev_self00000001", "qwen3:8b-q4_K_M", true, false)
		s := NewSelector(Inputs{Manifests: manifests, LocalState: emptyState()})
		if got := s.ownBestTier(inferencemesh.Snapshot{Self: self}); got != 50 {
			t.Fatalf("ownBestTier = %d, want 50 from Snapshot.Self", got)
		}
	})

	t.Run("unreachable self contributes nothing", func(t *testing.T) {
		self := mkPeer("dev_self00000001", "qwen3:8b-q4_K_M", false, false)
		s := NewSelector(Inputs{Manifests: manifests, LocalState: emptyState()})
		if got := s.ownBestTier(inferencemesh.Snapshot{Self: self}); got != 0 {
			t.Fatalf("ownBestTier = %d, want 0", got)
		}
	})

	t.Run("public peers are excluded from the own baseline", func(t *testing.T) {
		pub := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
		s := NewSelector(Inputs{Manifests: manifests, LocalState: emptyState()})
		if got := s.ownBestTier(inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{pub}}); got != 0 {
			t.Fatalf("ownBestTier = %d, want 0 — a public peer must not raise the bar it has to clear", got)
		}
	})
}
