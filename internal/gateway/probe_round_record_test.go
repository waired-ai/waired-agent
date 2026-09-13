package gateway

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestFallbackReason_DescribesTheFirstChoice is waired-agent#1355.
//
// Product contract, ratifying source: the doc comment this helper replaced
// ("operators see why did my preferred peer get skipped"), which the code
// under it did not do. X-Waired-Fallback-From names results[0]'s candidate,
// so the reason must be about that candidate and no other.
func TestFallbackReason_DescribesTheFirstChoice(t *testing.T) {
	ready := router.ProbeResult{Outcome: router.ProbeOK, Status: router.HealthStatus{
		EngineReady: true, ShareEnabled: true, CapacityTotal: 1,
	}}
	measuring := router.ProbeResult{Outcome: router.ProbeOK, Status: router.HealthStatus{
		EngineReady: true, ShareEnabled: true, Measuring: true,
	}}
	unreachable := router.ProbeResult{Outcome: router.ProbeTransportError}

	cases := []struct {
		name    string
		results []router.ProbeResult
		want    string
	}{
		{
			// Measured on a four-computer mesh: the first choice answered
			// ready and a concurrent request took its only slot. The header
			// read "measuring", the third candidate's state.
			name:    "first choice ready, lost its slot at commit",
			results: []router.ProbeResult{ready, ready, measuring},
			want:    "capacity_full",
		},
		{
			name:    "first choice ready, nobody else failed either",
			results: []router.ProbeResult{ready, ready},
			want:    "capacity_full",
		},
		{
			name:    "first choice not ready: its own reason, not a later one",
			results: []router.ProbeResult{unreachable, measuring, ready},
			want:    "transport_error",
		},
		{
			name:    "first choice measuring",
			results: []router.ProbeResult{measuring, unreachable, ready},
			want:    "measuring",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fallbackReason(tc.results); got != tc.want {
				t.Errorf("fallbackReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTryProbeAndCommit_NothingProbedRecordsNoPeer: when this device ranks
// first, ParallelProbe probes nothing, and the remote candidates behind it
// must not be recorded as having answered.
//
// Record of a defect found while measuring waired-agent#1354: the zero
// ProbeResult reads as ProbeOK with an empty status, so each of those peers
// was written into the PrefillWindow with nothing in flight, and a busy peer
// behind this device's own engine was ranked as idle on the next request.
func TestTryProbeAndCommit_NothingProbedRecordsNoPeer(t *testing.T) {
	local := router.NewLocalCandidate(router.Selection{
		EndpointID:    "local-ollama",
		ModelID:       "qwen3-8b-instruct",
		Runtime:       "ollama",
		ExecutionMode: "local",
	})
	var recorded []string
	h := &HandlerSet{deps: Deps{
		Selector: &candidatesSelector{cands: []router.Candidate{
			local,
			phase8RemoteCandidate("peer-busy"),
			phase8RemoteCandidate("peer-other"),
		}},
		OnPeerProbe: func(deviceID string, _ router.HealthStatus) {
			recorded = append(recorded, deviceID)
		},
	}}
	got, ok, err := h.tryProbeAndCommit(t.Context(), router.Request{Model: "waired/default"})
	if err != nil || !ok {
		t.Fatalf("tryProbeAndCommit = ok %v, err %v; want the local candidate committed", ok, err)
	}
	if got.Sel.ExecutionMode != "local" {
		t.Fatalf("committed %q, want the local candidate", got.Sel.ExecutionMode)
	}
	if len(recorded) != 0 {
		t.Errorf("OnPeerProbe called for %v, but no peer was probed", recorded)
	}
}

// assigningFake records which of the two ranking entries the probe round
// used. It hands back real candidates so the round can commit one.
type assigningFake struct {
	cands              []router.Candidate
	assigned, selectKs int
}

func (f *assigningFake) Select(_ context.Context, _ router.Request) (router.Selection, error) {
	return router.Selection{}, nil
}

func (f *assigningFake) SelectK(_ context.Context, _ router.Request, _ int) ([]router.Candidate, error) {
	f.selectKs++
	return f.cands, nil
}

func (f *assigningFake) SelectKAssigned(_ context.Context, _ router.Request, _ int) ([]router.Candidate, error) {
	f.assigned++
	return f.cands, nil
}

// TestTryProbeAndCommit_RanksThroughTheAssigningEntry: a probe round is the
// one caller that is about to dispatch, so it is the one that must rank and
// count in one step (waired-agent#1354). The counting itself is pinned in
// internal/router (TestSelectKAssigned_*); this pins that the gateway reaches
// it. Product contract, ratifying source: owner decision 2026-09-14
// (docs/decisions/20260914/0420-assignment-is-counted-when-it-is-made.md).
func TestTryProbeAndCommit_RanksThroughTheAssigningEntry(t *testing.T) {
	fake := &assigningFake{cands: []router.Candidate{router.NewLocalCandidate(router.Selection{
		EndpointID: "local-ollama", ModelID: "qwen3-8b-instruct", Runtime: "ollama", ExecutionMode: "local",
	})}}
	h := &HandlerSet{deps: Deps{Selector: fake}}
	if _, ok, err := h.tryProbeAndCommit(t.Context(), router.Request{Model: "waired/default"}); err != nil || !ok {
		t.Fatalf("tryProbeAndCommit = ok %v, err %v", ok, err)
	}
	if fake.assigned != 1 || fake.selectKs != 0 {
		t.Errorf("SelectKAssigned calls = %d, SelectK calls = %d; want 1 and 0", fake.assigned, fake.selectKs)
	}
}
