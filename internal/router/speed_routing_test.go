package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The rc4 review mesh of waired-agent#1082, as candidates. An engine-less
// host's turns went to the peer running the biggest model, which answered
// a 30k-token first turn in 9 min 10 s while another of the same person's
// machines answered it in 43 s.
//
// The peer that won was not trading quality for speed. It lost on both:
// by the catalog's own quality_tier the 35B-A3B (90) beats the 122B-A10B
// (83), while `score` — raw parameter count times the quantization
// ladder — put the 122B ahead by 3.5x.
func rc4Mesh() []meshCandidate {
	return []meshCandidate{
		{deviceID: "apu", score: 488e9, sizeClass: hostfit.ModelSizeLarge, rttMS: 10},
		{deviceID: "m5", score: 140e9, sizeClass: hostfit.ModelSizeMedium, rttMS: 10},
		{deviceID: "m4", score: 36e9, sizeClass: hostfit.ModelSizeSmall, rttMS: 10},
	}
}

// rc4Speeds are the measured prefill rates, as this requester would hold
// them: every host at the same rung, which is what makes them comparable.
func rc4Speeds() map[string]PeerSpeed {
	rung := func(tokps float64) map[int]PrefillRung {
		return map[int]PrefillRung{4096: {Depth: 4096, Tokps: tokps}}
	}
	return map[string]PeerSpeed{
		"apu": {VariantID: "v", Rungs: rung(54)},
		"m5":  {VariantID: "v", Rungs: rung(690)},
		"m4":  {VariantID: "v", Rungs: rung(117)},
	}
}

func order(cands []meshCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.deviceID)
	}
	return out
}

func eq(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSortMeshCandidates_PreferSpeedPicksTheFastPeer is waired-agent#1082
// as a guard.
//
// Product contract — owner ruling, 2026-08-29 (waired-agent#1128):
// "速度優先（こっちがデフォルト）".
func TestSortMeshCandidates_PreferSpeedPicksTheFastPeer(t *testing.T) {
	cands := rc4Mesh()
	assignSpeedRanks(cands, rc4Speeds())
	sortMeshCandidates(cands, state.RoutingPreferSpeed)
	if got := order(cands); !eq(got, "m5", "m4", "apu") {
		t.Errorf("order = %v, want m5 (690 tok/s) first and apu (54) last", got)
	}
}

// TestSortMeshCandidates_PreferSizeKeepsTheOldOrder: the operator who
// asks for the biggest model gets it, and gets the nine minutes with it.
// That is the choice being offered, and it has to still be available.
func TestSortMeshCandidates_PreferSizeKeepsTheOldOrder(t *testing.T) {
	cands := rc4Mesh()
	assignSpeedRanks(cands, rc4Speeds())
	sortMeshCandidates(cands, state.RoutingPreferSize)
	if got := order(cands); !eq(got, "apu", "m5", "m4") {
		t.Errorf("order = %v, want the score order", got)
	}
}

// TestSortMeshCandidates_EmptyPreferIsSpeed pins the default, which is
// also what an agent predating the field behaves as.
func TestSortMeshCandidates_EmptyPreferIsSpeed(t *testing.T) {
	cands := rc4Mesh()
	assignSpeedRanks(cands, rc4Speeds())
	sortMeshCandidates(cands, "")
	if got := order(cands); !eq(got, "m5", "m4", "apu") {
		t.Errorf("order = %v, want the speed order", got)
	}
}

// TestSortMeshCandidates_NoSpeedReadingsLeavesTodaysOrder is the nil rule
// (docs/decisions/20260822/0218) as an ordering: a requester that has
// measured nothing ranks exactly as it did before this existed.
func TestSortMeshCandidates_NoSpeedReadingsLeavesTodaysOrder(t *testing.T) {
	cands := rc4Mesh()
	assignSpeedRanks(cands, nil)
	sortMeshCandidates(cands, state.RoutingPreferSpeed)
	if got := order(cands); !eq(got, "apu", "m5", "m4") {
		t.Errorf("order = %v, want the score order when nothing is measured", got)
	}
}

// TestAssignSpeedRanks_UnmeasuredPeerIsRankedOptimistically: ranking it
// last would punish an endpoint nobody has measured, which the nil rule
// forbids — and it is the probe round that fetches a peer's published
// rate, so a peer ranked last is a peer that never gets measured.
func TestAssignSpeedRanks_UnmeasuredPeerIsRankedOptimistically(t *testing.T) {
	cands := rc4Mesh()
	speeds := rc4Speeds()
	delete(speeds, "m4") // never probed by this requester
	assignSpeedRanks(cands, speeds)

	var m4, m5 int
	for _, c := range cands {
		switch c.deviceID {
		case "m4":
			m4 = c.speedBucket
		case "m5":
			m5 = c.speedBucket
		}
	}
	if m4 != m5 {
		t.Errorf("unmeasured m4 got bucket %d, want the best known bucket %d", m4, m5)
	}
	sortMeshCandidates(cands, state.RoutingPreferSpeed)
	if got := order(cands); got[len(got)-1] != "apu" {
		t.Errorf("order = %v, want the measured-slow peer last", got)
	}
}

// TestAssignSpeedRanks_ScoresTheWholeRoundAtOneDepth. Prefill throughput
// falls with depth, so comparing a reading at 4,096 against one at 32,768
// would be decided by the depths. One depth for the round is also what
// keeps the key a total order: a per-pair "deepest common rung" is not
// one, and sort.SliceStable given a non-transitive comparison answers
// arbitrarily.
func TestAssignSpeedRanks_ScoresTheWholeRoundAtOneDepth(t *testing.T) {
	cands := []meshCandidate{{deviceID: "deep"}, {deviceID: "shallow"}}
	speeds := map[string]PeerSpeed{
		// "deep" is genuinely faster, and at 32,768 it reads 400. The
		// round can only be compared at 4,096, where it reads 900.
		"deep": {Rungs: map[int]PrefillRung{
			4096: {Depth: 4096, Tokps: 900}, 32768: {Depth: 32768, Tokps: 400},
		}},
		"shallow": {Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 100}}},
	}
	assignSpeedRanks(cands, speeds)
	sortMeshCandidates(cands, state.RoutingPreferSpeed)
	if got := order(cands); !eq(got, "deep", "shallow") {
		t.Errorf("order = %v, want deep first — both scored at 4,096", got)
	}

	// And with no shared depth, the key says nothing at all.
	cands2 := []meshCandidate{{deviceID: "a", score: 1}, {deviceID: "b", score: 2}}
	assignSpeedRanks(cands2, map[string]PeerSpeed{
		"a": {Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 100}}},
		"b": {Rungs: map[int]PrefillRung{32768: {Depth: 32768, Tokps: 900}}},
	})
	for _, c := range cands2 {
		if c.speedBucket != 0 {
			t.Errorf("%s got bucket %d; with no shared depth the key must be silent", c.deviceID, c.speedBucket)
		}
	}
}

// TestAssignSpeedRanks_CongestionDividesTheRate is the owner's formula:
// 素のprefill速度 / (既存セッション数 + 1). The peer's own in-flight count
// includes its owner's work, which is exactly right — a machine busy with
// its owner's turn is busy.
func TestAssignSpeedRanks_CongestionDividesTheRate(t *testing.T) {
	cands := []meshCandidate{{deviceID: "idle"}, {deviceID: "busy"}}
	assignSpeedRanks(cands, map[string]PeerSpeed{
		// The busy peer is nominally faster and still loses: 400/1 = 400
		// against 900/3 = 300.
		"idle": {Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 400}}},
		"busy": {CapacityUsed: 2, Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 900}}},
	})
	sortMeshCandidates(cands, state.RoutingPreferSpeed)
	if got := order(cands); !eq(got, "idle", "busy") {
		t.Errorf("order = %v, want the idle peer first", got)
	}
}

// TestSpeedBucketOf_BandsRatherThanRawNumbers: a measurement that agrees
// with another to 10 % has not earned the right to overturn a standing
// order by 1 %. Bucketing is also what keeps RankTier meaningful — a
// continuous key would put every candidate in its own tier and silently
// retire the residency tie-break of waired-agent#880.
func TestSpeedBucketOf_BandsRatherThanRawNumbers(t *testing.T) {
	// Within a 25 % band.
	if a, b := speedBucketOf(1.0), speedBucketOf(1.2); a != b {
		t.Errorf("1.0 and 1.2 landed in buckets %d and %d; they are within one band", a, b)
	}
	// Beyond it, and in the right direction: slower means a higher bucket.
	if a, b := speedBucketOf(1.0), speedBucketOf(2.0); b <= a {
		t.Errorf("buckets %d then %d; slower must sort later", a, b)
	}
	if a, b := speedBucketOf(1.0), speedBucketOf(0.5); b >= a {
		t.Errorf("buckets %d then %d; faster must sort earlier", a, b)
	}
	if got := speedBucketOf(0); got != 0 {
		t.Errorf("speedBucketOf(0) = %d, want the no-information bucket", got)
	}
}

// TestSameRankExceptDeviceID_ReadsTheSpeedKey guards the tier predicate
// against the new key. TestSameRankExceptDeviceID_ListsEverySortKey walks
// both functions with go/ast, but only names what is missing; this states
// the consequence.
func TestSameRankExceptDeviceID_ReadsTheSpeedKey(t *testing.T) {
	a := meshCandidate{deviceID: "a", speedBucket: 1}
	b := meshCandidate{deviceID: "b", speedBucket: 4}
	if sameRankExceptDeviceID(a, b) {
		t.Error("two candidates the speed key separates are not the same rank; " +
			"the #880 residency tie-break would overturn it instead of breaking a tie")
	}
}
