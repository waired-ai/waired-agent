package gateway

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// Residency as a tie-break in peer selection (waired-agent#880).
//
// The gap it closes is 17-56 s of first-token latency on the measured fleet
// (waired-agent#861), and what it replaces is `deviceID` ascending — the
// deterministic-pick suffix, which is arbitrary as far as the request is
// concerned. What it must NOT do is outrank anything: quality, priority,
// error rate, distance and load all still decide, and a peer that wins on
// any of them keeps winning while it is cold.

func readyWith(resident *bool) router.ProbeResult {
	return router.ProbeResult{
		Outcome: router.ProbeOK,
		Status: router.HealthStatus{
			EngineReady:   true,
			ShareEnabled:  true,
			ModelResident: resident,
		},
	}
}

func notReady() router.ProbeResult {
	return router.ProbeResult{Outcome: router.ProbeOK, Status: router.HealthStatus{}}
}

func tiers(t ...int) []router.Candidate {
	out := make([]router.Candidate, len(t))
	for i, tier := range t {
		out[i] = router.Candidate{ExecutionMode: "remote", RankTier: tier}
	}
	return out
}

func allSettled(n int) []bool {
	s := make([]bool, n)
	for i := range s {
		s[i] = true
	}
	return s
}

func TestBestSettledReady_ResidencyBreaksATieAndNothingElse(t *testing.T) {
	yes, no := true, false

	for _, tc := range []struct {
		name    string
		cands   []router.Candidate
		results []router.ProbeResult
		want    int
		because string
	}{
		{
			name:    "a warm peer wins a tie against a cold one",
			cands:   tiers(0, 0),
			results: []router.ProbeResult{readyWith(&no), readyWith(&yes)},
			want:    1,
			because: "these two are indistinguishable to the Selector, and one of them can answer now",
		},
		{
			name:    "rank still decides across tiers",
			cands:   tiers(0, 1),
			results: []router.ProbeResult{readyWith(&no), readyWith(&yes)},
			want:    0,
			because: "the leader outranks on a real key; residency must not overturn that",
		},
		{
			name:    "a warm leader is simply taken",
			cands:   tiers(0, 0),
			results: []router.ProbeResult{readyWith(&yes), readyWith(&yes)},
			want:    0,
		},
		{
			name:    "a peer that has not looked is not demoted",
			cands:   tiers(0, 0),
			results: []router.ProbeResult{readyWith(nil), readyWith(&yes)},
			want:    0,
			because: "nil is \"has not looked\", never \"cold\"",
		},
		{
			name:    "a peer that has not looked is not promoted either",
			cands:   tiers(0, 0),
			results: []router.ProbeResult{readyWith(&no), readyWith(nil)},
			want:    0,
			because: "there is no warm peer here, so the ranking stands",
		},
		{
			name:    "a cold peer still beats a not-ready one",
			cands:   tiers(0, 0),
			results: []router.ProbeResult{notReady(), readyWith(&no)},
			want:    1,
			because: "readiness is a filter, residency only orders what survives it",
		},
		{
			name:    "a warm peer that is not ready is not a candidate",
			cands:   tiers(0, 0),
			results: []router.ProbeResult{readyWith(&no), notReady()},
			want:    0,
		},
		{
			name:    "the search does not run past the tier",
			cands:   tiers(0, 1, 1),
			results: []router.ProbeResult{readyWith(&no), readyWith(&no), readyWith(&yes)},
			want:    0,
			because: "the warm peer is two ranks down, which is a ranking question, not a tie",
		},
		{
			name:    "no tier information is the permissive answer",
			cands:   []router.Candidate{{}, {}},
			results: []router.ProbeResult{readyWith(&no), readyWith(&yes)},
			want:    1,
			because: "a hand-built Candidate is tier 0 throughout, so it gets the tie-break",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, decided := bestSettledReady(tc.cands, tc.results, allSettled(len(tc.results)))
			if !decided {
				t.Fatalf("decided = false with everything settled")
			}
			if idx != tc.want {
				t.Errorf("winner = %d, want %d: %s", idx, tc.want, tc.because)
			}
		})
	}
}

// TestBestSettledReady_WaitsForTheRestOfTheTier is what keeps the tie-break
// from breaking the function's own contract: it decides only when the answer
// can no longer change. Committing to a cold leader while an equally-ranked
// peer is still being probed would do exactly that.
func TestBestSettledReady_WaitsForTheRestOfTheTier(t *testing.T) {
	yes, no := true, false
	cands := tiers(0, 0)
	results := []router.ProbeResult{readyWith(&no), {}}

	if _, decided := bestSettledReady(cands, results, []bool{true, false}); decided {
		t.Fatal("decided while a peer of the same rank was still unsettled")
	}
	// The same leader, once the neighbour arrives warm.
	results[1] = readyWith(&yes)
	idx, decided := bestSettledReady(cands, results, []bool{true, true})
	if !decided || idx != 1 {
		t.Errorf("bestSettledReady = (%d, %v), want (1, true)", idx, decided)
	}
}

// A WARM leader commits immediately: there is nothing a neighbour could say
// that would improve on it, so waiting would be latency for nothing.
func TestBestSettledReady_WarmLeaderDoesNotWaitForItsTier(t *testing.T) {
	yes := true
	idx, decided := bestSettledReady(tiers(0, 0),
		[]router.ProbeResult{readyWith(&yes), {}}, []bool{true, false})
	if !decided || idx != 0 {
		t.Errorf("bestSettledReady = (%d, %v), want (0, true)", idx, decided)
	}
}

// So does a leader that said nothing about residency: it cannot be improved
// on by a rule that only fires on a known-cold leader.
func TestBestSettledReady_SilentLeaderDoesNotWaitForItsTier(t *testing.T) {
	idx, decided := bestSettledReady(tiers(0, 0),
		[]router.ProbeResult{readyWith(nil), {}}, []bool{true, false})
	if !decided || idx != 0 {
		t.Errorf("bestSettledReady = (%d, %v), want (0, true)", idx, decided)
	}
}

// The pre-existing contracts, restated against the new signature: an
// unsettled candidate that outranks every ready one blocks the decision, and
// an all-settled round with nothing ready is the brief-queue trigger.
func TestBestSettledReady_UnchangedContracts(t *testing.T) {
	yes := true
	if _, decided := bestSettledReady(tiers(0, 0),
		[]router.ProbeResult{{}, readyWith(&yes)}, []bool{false, true}); decided {
		t.Error("decided while a better-ranked candidate was unsettled")
	}
	idx, decided := bestSettledReady(tiers(0, 0),
		[]router.ProbeResult{notReady(), notReady()}, allSettled(2))
	if !decided || idx != -1 {
		t.Errorf("bestSettledReady = (%d, %v), want (-1, true)", idx, decided)
	}
}
