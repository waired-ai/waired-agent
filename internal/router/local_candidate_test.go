package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// localFor is the LocalNode a serving host with this model and window
// reports. Helpers rather than literals so a row says only what it varies.
func localFor(tag string) LocalNode {
	return LocalNode{
		DeviceID:    "self",
		DisplayName: "this-computer",
		Serving:     true,
		Runtime:     catalog.RuntimeOllama,
		EngineTag:   tag,
		ModelID:     "qwen3-8b-instruct",
		VariantID:   "q4-gguf",
	}
}

// TestBuildLocalCandidate is the seam below the ordering: a pure
// (LocalNode, window, want) → (candidate, ok, drop).
//
// Product contract, ratifying source waired-agent#1302 and the owner ruling
// of 2026-08-29 on waired-agent#1128 ("ローカルと peer は区別しない"): this
// device passes the same filters, in the same order, as a peer.
func TestBuildLocalCandidate(t *testing.T) {
	want := func(t *testing.T) meshWant {
		t.Helper()
		o, v := wantSetsFor([]catalog.Manifest{qwen()})
		return meshWant{ollama: o, vllm: v, modelID: ""}
	}

	t.Run("serving a tag the request wants", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}})
		c, ok, _ := s.buildLocalCandidate(localFor("qwen3:8b-q4_K_M"), 0, want(t))
		if !ok {
			t.Fatal("no candidate for a serving host that matches the want set")
		}
		if !c.local {
			t.Error("candidate is not marked local")
		}
		if c.public || c.silent || c.priority != 0 {
			t.Errorf("public=%v silent=%v priority=%d, want the own-network zero values", c.public, c.silent, c.priority)
		}
		if c.inFlight != 0 || c.loadFraction != 0 || c.mapAgeMS != 0 {
			t.Errorf("in_flight/load/map_age are peer axes and must stay zero: %+v", c)
		}
	})

	t.Run("not serving", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}})
		ln := localFor("qwen3:8b-q4_K_M")
		ln.Serving = false
		if _, ok, _ := s.buildLocalCandidate(ln, 0, want(t)); ok {
			t.Error("a host that is not serving offered a candidate")
		}
	})

	t.Run("no device id yet", func(t *testing.T) {
		// Before the first network map. Nothing keyed by device id would
		// resolve, so there is no candidate to rank.
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}})
		ln := localFor("qwen3:8b-q4_K_M")
		ln.DeviceID = ""
		if _, ok, _ := s.buildLocalCandidate(ln, 0, want(t)); ok {
			t.Error("a host with no device id offered a candidate")
		}
	})

	t.Run("local serving turned off", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalServingOff: true})
		if _, ok, _ := s.buildLocalCandidate(localFor("qwen3:8b-q4_K_M"), 0, want(t)); ok {
			t.Error("the operator turned local serving off and it offered a candidate anyway")
		}
	})

	t.Run("public share asked for somebody else's computer", func(t *testing.T) {
		// waired-agent#901: this host is the population that entry exists
		// to leave.
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, PublicOnly: true})
		if _, ok, _ := s.buildLocalCandidate(localFor("qwen3:8b-q4_K_M"), 0, want(t)); ok {
			t.Error("a public-only request was offered this device")
		}
	})

	t.Run("serving a tag nothing asked for", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}})
		if _, ok, _ := s.buildLocalCandidate(localFor("something:else"), 0, want(t)); ok {
			t.Error("a tag outside the want set offered a candidate")
		}
	})

	t.Run("declared window below the request's floor", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}})
		ln := localFor("qwen3:8b-q4_K_M")
		ln.ContextWindow = 200_704
		if _, ok, _ := s.buildLocalCandidate(ln, 1_000_000, want(t)); ok {
			t.Error("a host declaring a smaller window than the request needs was offered")
		}
		// 0 declares nothing, and must read as unknown rather than as
		// "serves nothing" — the same rule a peer's 0 has (waired#1031).
		ln.ContextWindow = 0
		if _, ok, _ := s.buildLocalCandidate(ln, 1_000_000, want(t)); !ok {
			t.Error("a host declaring nothing was excluded by a window floor")
		}
	})

	t.Run("below the operator's model-size floor", func(t *testing.T) {
		s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, MinModelSize: "large"})
		_, ok, drop := s.buildLocalCandidate(localFor("qwen3:8b-q4_K_M"), 0, want(t))
		if ok {
			t.Error("the floor did not exclude this device's own engine")
		}
		if !drop.belowFloor {
			t.Error("the floor exclusion did not travel, so nothing can name it")
		}
	})
}

// TestLocalRTT is the starvation guard, and it is the one ranking key whose
// honest value would break the mechanism.
//
// Product contract, ratifying source waired-agent#1302: a device that has
// never compared itself with the mesh reports the same "unknown" distance a
// peer nobody has pinged reports. With a real 0 it wins every cold round on
// distance alone, ParallelProbe's index-0 fast path then probes nobody,
// PrefillWindow never learns a peer's rate — peer speeds are only ever
// learned on the request path — and it wins the next round for the same
// reason, forever.
func TestLocalRTT(t *testing.T) {
	measured := localFor("qwen3:8b-q4_K_M")
	measured.Prefill = &PrefillRate{VariantID: "q4-gguf", Rungs: []PrefillRung{{Depth: 4096, Tokps: 900}}}
	if got := localRTT(measured); got != 0 {
		t.Errorf("localRTT with a reading = %d, want 0 (there is no network leg)", got)
	}
	if got := localRTT(localFor("qwen3:8b-q4_K_M")); got != RTTUnknown {
		t.Errorf("localRTT with no reading = %d, want RTTUnknown", got)
	}
}

// TestRoundSpeeds pins that this device joins the round's speed map under
// its own id, that the peer view is copied rather than mutated, and that
// the congestion divisor travels.
func TestRoundSpeeds(t *testing.T) {
	peers := map[string]PeerSpeed{
		"peer-A": {VariantID: "q4-gguf", Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 500}}},
	}
	ln := localFor("qwen3:8b-q4_K_M")
	ln.CapacityUsed = 2
	ln.Prefill = &PrefillRate{VariantID: "q4-gguf", Rungs: []PrefillRung{{Depth: 4096, Tokps: 900}}}

	got := roundSpeeds(peers, ln)
	if len(peers) != 1 {
		t.Fatalf("the live peer view was mutated: %+v", peers)
	}
	self, ok := got["self"]
	if !ok {
		t.Fatal("this device is not in the round's speed map")
	}
	if self.CapacityUsed != 2 {
		t.Errorf("CapacityUsed = %d, want 2 — the congestion divisor", self.CapacityUsed)
	}
	if r, ok := self.Rungs[4096]; !ok || r.Tokps != 900 {
		t.Errorf("rungs = %+v, want the published reading", self.Rungs)
	}
	if _, ok := got["peer-A"]; !ok {
		t.Error("the peer entries did not survive the copy")
	}

	t.Run("an unmeasured device carries no rungs", func(t *testing.T) {
		// nil rungs, not an empty map: RoundRung skips entries with no
		// readings, and an empty map would make it think this device had
		// been measured at no depth at all.
		got := roundSpeeds(peers, localFor("qwen3:8b-q4_K_M"))
		if got["self"].Rungs != nil {
			t.Errorf("rungs = %+v, want nil", got["self"].Rungs)
		}
	})

	t.Run("no device id leaves the map alone", func(t *testing.T) {
		ln := localFor("qwen3:8b-q4_K_M")
		ln.DeviceID = ""
		if got := roundSpeeds(peers, ln); len(got) != 1 {
			t.Errorf("got %d entries, want the peer view unchanged", len(got))
		}
	})
}

// TestShuffleWithinTiers is the herd break (waired-agent#1303, S4).
//
// Product contract: reordering happens only INSIDE a rank tier, tiers stay
// contiguous, and a nil tie-break leaves the deterministic order that every
// ordering test relies on.
func TestShuffleWithinTiers(t *testing.T) {
	mk := func(ids []string, tiers []int) []meshCandidate {
		out := make([]meshCandidate, len(ids))
		for i := range ids {
			out[i] = meshCandidate{deviceID: ids[i], rankTier: tiers[i]}
		}
		return out
	}

	t.Run("nil leaves the order alone", func(t *testing.T) {
		c := mk([]string{"a", "b", "c"}, []int{0, 0, 1})
		shuffleWithinTiers(c, nil)
		if c[0].deviceID != "a" || c[1].deviceID != "b" || c[2].deviceID != "c" {
			t.Errorf("order moved with no tie-break: %+v", c)
		}
	})

	t.Run("a tier of one is never asked", func(t *testing.T) {
		asked := 0
		c := mk([]string{"a", "b"}, []int{0, 1})
		shuffleWithinTiers(c, func(n int) int { asked++; return 0 })
		if asked != 0 {
			t.Errorf("the tie-break was asked %d times about tiers of one", asked)
		}
	})

	t.Run("it reorders inside a tier and nowhere else", func(t *testing.T) {
		// The fake takes and records the real n, so the test can check
		// what it was asked rather than only that something moved.
		var ns []int
		c := mk([]string{"a", "b", "c", "d"}, []int{0, 0, 0, 1})
		shuffleWithinTiers(c, func(n int) int { ns = append(ns, n); return n - 1 })
		if len(ns) == 0 {
			t.Fatal("the tie-break was never consulted")
		}
		for _, n := range ns {
			if n < 2 || n > 3 {
				t.Errorf("asked for a pick in [0,%d); the tier has 3 members", n)
			}
		}
		if c[3].deviceID != "d" {
			t.Errorf("a candidate crossed a tier boundary: %+v", c)
		}
		seen := map[string]bool{}
		for _, x := range c[:3] {
			seen[x.deviceID] = true
		}
		if len(seen) != 3 {
			t.Errorf("the tier lost or duplicated a member: %+v", c[:3])
		}
	})

	t.Run("tiers stay contiguous", func(t *testing.T) {
		c := mk([]string{"a", "b", "c", "d"}, []int{0, 0, 1, 1})
		shuffleWithinTiers(c, func(n int) int { return 0 })
		for i := 1; i < len(c); i++ {
			if c[i].rankTier < c[i-1].rankTier {
				t.Fatalf("tiers are no longer ascending: %+v", c)
			}
		}
	})
}

// TestSelectK_LocalRanksWithTheMesh is waired-agent#1302's symptom #1, end
// to end through SelectK: a ready local model no longer takes the turn
// without the mesh being looked at, and a busy local engine loses to an
// idle peer.
//
// Product contract, ratifying source waired-agent#1302 + the owner ruling of
// 2026-08-29 on waired-agent#1128/#1129.
func TestSelectK_LocalRanksWithTheMesh(t *testing.T) {
	peerTag := "qwen3:8b-q4_K_M"
	snap := inferencemesh.Snapshot{
		SelfDeviceID: "self",
		Peers:        []inferencemesh.PeerView{mkPeerWithCap("peer-B", peerTag, 4)},
	}
	base := func(ln LocalNode, speeds map[string]PeerSpeed) Inputs {
		return Inputs{
			Manifests:      []catalog.Manifest{qwen()},
			LocalState:     readyState(),
			Hardware:       goodHardware(),
			Runtimes:       registryWithOllama(),
			MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
			LocalNode:      func() LocalNode { return ln },
			PeerSpeeds:     func() map[string]PeerSpeed { return speeds },
			RoutingMode:    state.RoutingModeAuto,
		}
	}
	fast := func(tokps float64) *PrefillRate {
		return &PrefillRate{VariantID: "q4-gguf", Rungs: []PrefillRung{{Depth: 4096, Tokps: tokps}}}
	}

	t.Run("an idle local engine still wins, and the mesh was consulted", func(t *testing.T) {
		ln := localFor(peerTag)
		ln.Capacity, ln.CapacityUsed, ln.Prefill = 2, 0, fast(900)
		s := NewSelector(base(ln, map[string]PeerSpeed{
			"peer-B": {VariantID: "q4-gguf", Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 900}}},
		}))
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if cands[0].ExecutionMode != "local" {
			t.Fatalf("winner = %q, want local: %+v", cands[0].ExecutionMode, cands)
		}
		// The point of the change: the peer is in the list. Before
		// waired-agent#1302 the mesh was never built at all.
		if len(cands) < 2 {
			t.Fatalf("got %d candidates, want the mesh ranked alongside: %+v", len(cands), cands)
		}
		if cands[0].PeerID != "" {
			t.Errorf("the local candidate carries a peer id: %q", cands[0].PeerID)
		}
		sel, ok := cands[0].Commit()
		if !ok {
			t.Fatal("the local candidate refused to commit")
		}
		if sel.Release == nil {
			t.Error("Release is nil; the caller defers it unconditionally")
		}
	})

	t.Run("a busy local engine loses to an idle peer", func(t *testing.T) {
		// Same raw speed on both; this device has two conversations in
		// use, so the congestion divisor makes it the slower candidate.
		// That is the half of the ruling that stops the owner's own turn
		// from co-occupying an engine peers are already on.
		ln := localFor(peerTag)
		ln.Capacity, ln.CapacityUsed, ln.Prefill = 2, 2, fast(900)
		s := NewSelector(base(ln, map[string]PeerSpeed{
			"peer-B": {VariantID: "q4-gguf", CapacityUsed: 0, Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 900}}},
		}))
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if cands[0].ExecutionMode != "remote" {
			t.Fatalf("winner = %q, want the idle peer: %+v", cands[0].ExecutionMode, cands)
		}
		// Demoted, never excluded: the owner's machine is still an answer.
		found := false
		for _, c := range cands {
			if c.ExecutionMode == "local" {
				found = true
			}
		}
		if !found {
			t.Error("this device was dropped from the list rather than ranked below the peer")
		}
	})

	t.Run("serving the OLD model through a switch", func(t *testing.T) {
		// waired-agent#1302 symptom #2. The request named no model, so
		// the node that answers chooses it; this device is serving the
		// old one and says so. Before the change the auto arm resolved to
		// the NEW model, found it "downloading", and left for the mesh
		// while peers were being served the old one by this very host.
		ln := localFor(peerTag)
		ln.PendingModelID = "qwen3.5-4b"
		ln.Capacity, ln.CapacityUsed, ln.Prefill = 2, 0, fast(900)
		in := base(ln, nil)
		in.LocalState = emptyState() // the NEW model is not on disk
		s := NewSelector(in)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if cands[0].ExecutionMode != "local" {
			t.Fatalf("winner = %q, want the old model on this device: %+v", cands[0].ExecutionMode, cands)
		}
		if cands[0].EngineModel != peerTag {
			t.Errorf("EngineModel = %q, want the loaded model %q", cands[0].EngineModel, peerTag)
		}
		joined := strings.Join(cands[0].Decision.Reason, "\n")
		if !strings.Contains(joined, "qwen3.5-4b") {
			t.Errorf("the reasons do not say what the switch is arriving at:\n%s", joined)
		}
	})

	t.Run("an unmeasured device is not punished", func(t *testing.T) {
		// The nil rule (docs/decisions/20260822/0218): never punish an
		// endpoint you have not measured. Unmeasured here also means the
		// distance key reads unknown (see TestLocalRTT), so the round ties
		// and this device is still a candidate rather than last by default.
		ln := localFor(peerTag)
		ln.Capacity = 2
		s := NewSelector(base(ln, map[string]PeerSpeed{
			"peer-B": {VariantID: "q4-gguf", Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 900}}},
		}))
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		found := false
		for _, c := range cands {
			if c.ExecutionMode == "local" {
				found = true
			}
		}
		if !found {
			t.Errorf("an unmeasured device was left out of the list: %+v", cands)
		}
	})
}

// TestSelectK_AutoFallsBackWhenThisDeviceCannotDescribeItself is the boot
// window, and it is the regression most likely to reach a first-run user.
//
// The local reading is empty whenever the device cannot describe itself yet
// — no device id before the first network map, no engine tag before the
// active model is recorded. A single-machine install must keep serving its
// own turns through those windows, so the arm falls through to the one that
// always did rather than reporting a mesh verdict.
func TestSelectK_AutoFallsBackWhenThisDeviceCannotDescribeItself(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return inferencemesh.Snapshot{} },
		LocalNode:      func() LocalNode { return LocalNode{} },
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v — a ready local model must still be served", err)
	}
	if len(cands) != 1 || cands[0].ExecutionMode != "local" {
		t.Fatalf("got %+v, want the single local candidate the pre-#1302 arm produced", cands)
	}
}

// TestSelectK_LocalIsNotGatedByTheOutboundTracker: this device's own entry
// is not refused by the per-peer in-flight tracker, and does not consume a
// slot in it.
//
// Product contract, ratifying source waired-agent#1302. LocalInFlight counts
// this requester's OUTBOUND overlay requests per peer; a turn served on this
// device is not one of those, so gating on it would refuse this device for
// work it never sent anywhere. Its own occupancy is priced in the speed
// divisor instead — one axis, one meaning.
func TestSelectK_LocalIsNotGatedByTheOutboundTracker(t *testing.T) {
	snap := inferencemesh.Snapshot{SelfDeviceID: "self"}
	tracker := NewInFlightTracker()
	// Saturate the tracker under THIS device's id, at the capacity the
	// local reading reports. A peer in this state is dropped by the
	// admission pre-filter.
	release, ok := tracker.Acquire("self", 1)
	if !ok {
		t.Fatal("could not seed the tracker")
	}
	defer release()

	ln := localFor("qwen3:8b-q4_K_M")
	ln.Capacity = 1
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalNode:      func() LocalNode { return ln },
		LocalInFlight:  tracker,
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v — this device was refused for outbound requests it never sent", err)
	}
	if len(cands) != 1 || cands[0].ExecutionMode != "local" {
		t.Fatalf("got %+v, want the local candidate", cands)
	}
	// And committing it takes nothing from the tracker, so a second
	// selection is not starved by the first.
	before := tracker.InFlight("self")
	if _, ok := cands[0].Commit(); !ok {
		t.Fatal("the local candidate refused to commit")
	}
	if after := tracker.InFlight("self"); after != before {
		t.Errorf("the outbound tracker moved from %d to %d on a local commit", before, after)
	}
}

// TestSelectK_AGapInTheLocalReadingDoesNotSendTheTurnAway is the regression
// real hardware caught, and unit tests did not.
//
// Measured on pc-mbp14-m5 (2026-09-12), twenty seconds after a daemon
// restart: this device's own reading was still empty — the engine had not
// finished coming back — while the mesh snapshot was fully populated. The
// ranked arm found no local candidate, found peers, and sent the turn to a
// 125B model on another computer at 585 ms rtt while this one held a ready
// 35B-A3B. Before waired-agent#1302 that turn stayed here.
//
// PRODUCT CONTRACT. A reading this device could not take is not an ordering
// input: absence of a reading is not evidence that this computer cannot
// serve. The arm is entered only when the reading exists, or when local was
// not an answer anyway.
func TestSelectK_AGapInTheLocalReadingDoesNotSendTheTurnAway(t *testing.T) {
	// A populated mesh, exactly as the measured host saw it.
	snap := inferencemesh.Snapshot{
		SelfDeviceID: "self",
		Peers:        []inferencemesh.PeerView{mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 2)},
	}
	in := Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(), // this device's own model IS ready
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		// The gap: the device cannot describe itself yet.
		LocalNode: func() LocalNode { return LocalNode{} },
	}
	cands, err := NewSelector(in).SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 1 || cands[0].ExecutionMode != "local" {
		t.Fatalf("got %+v, want this device's own ready model — a gap in the reading sent the turn to a peer", cands)
	}

	t.Run("but a device that genuinely cannot serve still reaches the mesh", func(t *testing.T) {
		// The other half: when local was not an answer anyway, an empty
		// reading costs nothing and the mesh is where the turn goes.
		in := in
		in.LocalState = emptyState()
		cands, err := NewSelector(in).SelectK(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("SelectK: %v", err)
		}
		if len(cands) == 0 || cands[0].ExecutionMode != "remote" {
			t.Fatalf("got %+v, want the peer", cands)
		}
	})
}

// TestSelectK_TheRankedArmDoesNotSayTheMeshWasATryingPoint: the reason the
// pre-#1302 arm printed — "this host has no candidate; trying the mesh" —
// describes a branch the ranked arm does not have, and printing it beside
// the local candidate's own line contradicted it.
//
// Observed on pc-mbp14-m5 before this was fixed: both lines in one trace.
func TestSelectK_TheRankedArmDoesNotSayTheMeshWasATryingPoint(t *testing.T) {
	snap := inferencemesh.Snapshot{
		SelfDeviceID: "self",
		Peers:        []inferencemesh.PeerView{mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 2)},
	}
	ln := localFor("qwen3:8b-q4_K_M")
	ln.Capacity = 2
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalNode:      func() LocalNode { return ln },
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	joined := strings.Join(cands[0].Decision.Reason, "\n")
	if strings.Contains(joined, "trying the mesh") {
		t.Errorf("the ranked arm still describes the mesh as a fallback:\n%s", joined)
	}
	if !strings.Contains(joined, "ranked with the other computers") {
		t.Errorf("the trace does not say this computer is in the list:\n%s", joined)
	}
}
