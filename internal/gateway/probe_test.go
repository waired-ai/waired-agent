package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// stubRT is the test fixture for parallelProbe. Each peer ID maps to
// a response (status, body) plus an optional delay simulating per-
// probe latency.
type stubRT struct {
	status  int
	body    string
	delay   time.Duration
	hits    atomic.Int32
	dialErr error
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.hits.Add(1)
	if s.dialErr != nil {
		return nil, s.dialErr
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     http.StatusText(s.status),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// candFor builds a minimal remote Candidate keyed on peerID. The
// fields ParallelProbe touches are ExecutionMode and PeerID; nothing
// else matters here.
func candFor(peerID string) router.Candidate {
	return router.Candidate{
		EndpointID:    "remote-" + peerID + "-ollama-qwen3-8b",
		ModelID:       "qwen3-8b-instruct",
		Runtime:       "remote:" + peerID,
		PeerID:        peerID,
		ExecutionMode: "remote",
	}
}

func localCandidate() router.Candidate {
	return router.Candidate{
		EndpointID:    "local-ollama-qwen3-8b",
		ModelID:       "qwen3-8b-instruct",
		Runtime:       "ollama",
		ExecutionMode: "local",
	}
}

func readyBody(used, total int) string {
	return fmt.Sprintf(`{"engine_ready":true,"model_id":"qwen3:8b-q4_K_M","capacity_total":%d,"capacity_used":%d,"paused":false,"share_enabled":true}`, total, used)
}

// TestParallelProbe_LocalCandidateFastPath confirms a local
// candidate skips probing entirely. The /healthz coordinator is for
// the mesh path only — running an HTTP probe against your own
// loopback engine is wasted RTT.
func TestParallelProbe_LocalCandidateFastPath(t *testing.T) {
	cands := []router.Candidate{localCandidate()}
	calls := atomic.Int32{}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		calls.Add(1)
		return nil, "", errors.New("lookup must not be called for local")
	}
	winner, all := ParallelProbe(context.Background(), cands, lookup, 50*time.Millisecond)
	if winner != 0 {
		t.Errorf("winner = %d, want 0", winner)
	}
	if calls.Load() != 0 {
		t.Errorf("lookup invoked %d times for a local candidate; want 0", calls.Load())
	}
	if !all[0].IsReady() {
		t.Errorf("local candidate must be synthesised as ready")
	}
}

// TestParallelProbe_FirstReadyWins exercises the central contract:
// among three remote candidates, the one whose probe returns ready
// first is the winner. The other two get their results recorded but
// don't influence the winner.
func TestParallelProbe_BestRankedReadyWins(t *testing.T) {
	rtA := &stubRT{status: 200, body: readyBody(0, 4), delay: 5 * time.Millisecond}
	rtB := &stubRT{status: 200, body: readyBody(4, 4), delay: 5 * time.Millisecond}
	rtC := &stubRT{status: 200, body: readyBody(2, 4), delay: 5 * time.Millisecond}
	lookups := map[string]*stubRT{"peer-A": rtA, "peer-B": rtB, "peer-C": rtC}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		rt, ok := lookups[peerID]
		if !ok {
			return nil, "", fmt.Errorf("no stub for %q", peerID)
		}
		return rt, "http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B"), candFor("peer-C")}
	winner, all := ParallelProbe(context.Background(), cands, lookup, 100*time.Millisecond)
	// peer-A and peer-C are both ready. peer-A is ranked higher, so it
	// wins regardless of which probe answered first.
	if winner != 0 {
		t.Errorf("winner = %d, want 0 (peer-A, the best-ranked ready candidate)", winner)
	}
	// peer-B is capacity_full → must be in results as not-ready.
	if all[1].IsReady() {
		t.Errorf("peer-B (capacity full) must not be ready: %+v", all[1])
	}
}

// TestParallelProbe_StickyOrderingRespected makes the position-0
// promise concrete: when candidate[0] is ready, it wins even if another
// candidate answers its probe sooner. The sticky-bound peer that the
// Selector hoisted to position 0 keeps the conversation's KV cache.
//
// This is the case that used to go the other way. The winner was the
// first arrival, so on a mesh of probeFanoutK peers or fewer — every
// candidate probed at once — the Selector's whole ranking was decided
// by /healthz latency instead. See
// docs/decisions/20260805/1703-probe-honours-the-selector-ranking.md.
func TestParallelProbe_StickyOrderingRespected(t *testing.T) {
	// Both ready, but peer-Z is the sticky-bound candidate at index 0.
	rtZ := &stubRT{status: 200, body: readyBody(0, 4)}
	rtA := &stubRT{status: 200, body: readyBody(0, 4)}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		switch peerID {
		case "peer-Z":
			return rtZ, "http://peer-Z:55000", nil
		case "peer-A":
			return rtA, "http://peer-A:55000", nil
		}
		return nil, "", fmt.Errorf("no stub for %q", peerID)
	}
	cands := []router.Candidate{candFor("peer-Z"), candFor("peer-A")}
	// peer-Z answers a full 10 ms after peer-A. Under the old
	// first-arrival rule peer-A won this every time.
	rtZ.delay = 10 * time.Millisecond
	winner, _ := ParallelProbe(context.Background(), cands, lookup, 500*time.Millisecond)
	if winner != 0 {
		t.Errorf("winner = %d, want 0 (peer-Z, the sticky-bound head)", winner)
	}
}

// TestParallelProbe_FallsPastAHeadThatIsNotReady is the other half of
// the ranking rule: honouring the order must not mean waiting for a
// peer that already said no. peer-A refuses, so peer-B wins — and
// because a refusal settles the ranking immediately, without waiting
// out the budget.
func TestParallelProbe_FallsPastAHeadThatIsNotReady(t *testing.T) {
	rtA := &stubRT{status: 200, body: readyBody(4, 4)} // capacity full
	rtB := &stubRT{status: 200, body: readyBody(0, 4)}
	rtC := &stubRT{status: 200, body: readyBody(0, 4)}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		return map[string]*stubRT{"peer-A": rtA, "peer-B": rtB, "peer-C": rtC}[peerID],
			"http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B"), candFor("peer-C")}
	start := time.Now()
	winner, _ := ParallelProbe(context.Background(), cands, lookup, 2*time.Second)
	elapsed := time.Since(start)
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (peer-B, best-ranked ready)", winner)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v; a settled not-ready head must release the decision at once, not at the budget", elapsed)
	}
}

// TestParallelProbe_HeadThatHangsCostsOnlyTheBudget bounds what
// honouring the ranking can cost. A head that never answers holds the
// decision, but only until the budget expires — which is what the
// budget is for. The lower-ranked ready peer then wins.
func TestParallelProbe_HeadThatHangsCostsOnlyTheBudget(t *testing.T) {
	rtA := &stubRT{status: 200, body: readyBody(0, 4), delay: 2 * time.Second}
	rtB := &stubRT{status: 200, body: readyBody(0, 4)}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		return map[string]*stubRT{"peer-A": rtA, "peer-B": rtB}[peerID],
			"http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B")}
	start := time.Now()
	winner, _ := ParallelProbe(context.Background(), cands, lookup, 50*time.Millisecond)
	elapsed := time.Since(start)
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (peer-B, after the head timed out)", winner)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v, want the 50 ms budget to bound it", elapsed)
	}
}

// TestParallelProbe_AllFailReturnsMinusOne is the brief-queue trigger
// condition. Every probe failed → winnerIdx = -1; the gateway then
// sleeps 250 ms and tries SelectK + ParallelProbe once more.
func TestParallelProbe_AllFailReturnsMinusOne(t *testing.T) {
	rtA := &stubRT{dialErr: errors.New("connect refused")}
	rtB := &stubRT{status: 503, body: "overloaded"}
	rtC := &stubRT{status: 200, body: readyBody(4, 4)} // capacity full
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		return map[string]*stubRT{"peer-A": rtA, "peer-B": rtB, "peer-C": rtC}[peerID],
			"http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B"), candFor("peer-C")}
	winner, all := ParallelProbe(context.Background(), cands, lookup, 100*time.Millisecond)
	if winner != -1 {
		t.Errorf("winner = %d, want -1 (all failed)", winner)
	}
	// All three results must be populated.
	for i, r := range all {
		if r.IsReady() {
			t.Errorf("all[%d] = %+v unexpectedly ready", i, r)
		}
	}
	// Each peer's reason must differ — sanity-check the classifier.
	if all[0].Outcome != router.ProbeTransportError {
		t.Errorf("peer-A outcome = %v, want TransportError", all[0].Outcome)
	}
	if all[1].Outcome != router.ProbeTransportError {
		t.Errorf("peer-B outcome = %v, want TransportError", all[1].Outcome)
	}
	if all[2].Outcome != router.ProbeOK || all[2].FailureReason() != "capacity_full" {
		t.Errorf("peer-C outcome=%v reason=%q, want OK/capacity_full", all[2].Outcome, all[2].FailureReason())
	}
}

// TestParallelProbe_CancelsLosersOnFirstReady confirms the cancel
// propagates to in-flight probes. peer-A is the best-ranked candidate
// and returns ready immediately; peer-B is delayed 200 ms. Nothing
// slower can change the answer once the head is ready, so ParallelProbe
// must return before peer-B completes and peer-B's RoundTrip must
// observe context cancellation.
func TestParallelProbe_CancelsLosersOnceTheWinnerIsSettled(t *testing.T) {
	rtA := &stubRT{status: 200, body: readyBody(0, 4)}
	rtB := &stubRT{status: 200, body: readyBody(0, 4), delay: 200 * time.Millisecond}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		return map[string]*stubRT{"peer-A": rtA, "peer-B": rtB}[peerID],
			"http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B")}
	start := time.Now()
	winner, _ := ParallelProbe(context.Background(), cands, lookup, 500*time.Millisecond)
	elapsed := time.Since(start)
	if winner != 0 {
		t.Errorf("winner = %d, want 0 (peer-A)", winner)
	}
	// peer-B's delay is 200ms; if cancel works, ParallelProbe returns
	// much faster.
	if elapsed > 150*time.Millisecond {
		t.Errorf("ParallelProbe took %v with first-ready cancel; want < 150ms", elapsed)
	}
}

// TestParallelProbe_LegacyPeerTreatedReady ensures a Phase 7 peer
// (404 on /healthz) is selected as winner over later-arriving
// failures. Mixed-version mesh deployments must work.
func TestParallelProbe_LegacyPeerTreatedReady(t *testing.T) {
	rtA := &stubRT{dialErr: errors.New("connect refused")}
	rtB := &stubRT{status: 404, body: "not found"} // legacy peer
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		return map[string]*stubRT{"peer-A": rtA, "peer-B": rtB}[peerID],
			"http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B")}
	winner, all := ParallelProbe(context.Background(), cands, lookup, 100*time.Millisecond)
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (legacy peer)", winner)
	}
	if all[1].Outcome != router.ProbeLegacyPeer {
		t.Errorf("legacy peer outcome = %v, want ProbeLegacyPeer", all[1].Outcome)
	}
}

// TestParallelProbe_TimeoutBudgetHonored confirms the per-call budget
// is the upper bound on wait time. Two probes both delayed past the
// budget → no winner, ParallelProbe returns at the budget mark.
func TestParallelProbe_TimeoutBudgetHonored(t *testing.T) {
	rtA := &stubRT{status: 200, body: readyBody(0, 4), delay: 500 * time.Millisecond}
	rtB := &stubRT{status: 200, body: readyBody(0, 4), delay: 500 * time.Millisecond}
	lookup := func(peerID string) (http.RoundTripper, string, error) {
		return map[string]*stubRT{"peer-A": rtA, "peer-B": rtB}[peerID],
			"http://" + peerID + ":55000", nil
	}
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B")}
	start := time.Now()
	winner, _ := ParallelProbe(context.Background(), cands, lookup, 50*time.Millisecond)
	elapsed := time.Since(start)
	if winner != -1 {
		t.Errorf("winner = %d, want -1 (timeout)", winner)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("ParallelProbe waited %v past the 50ms budget", elapsed)
	}
}

// TestParallelProbe_NilLookupSurfaceErrPerCandidate guards against a
// gateway wiring bug: when the caller passes a nil lookup, every
// remote probe surfaces a TransportError. This is preferred over a
// panic.
func TestParallelProbe_NilLookupSurfaceErrPerCandidate(t *testing.T) {
	cands := []router.Candidate{candFor("peer-A"), candFor("peer-B")}
	winner, all := ParallelProbe(context.Background(), cands, nil, 50*time.Millisecond)
	if winner != -1 {
		t.Errorf("winner = %d, want -1", winner)
	}
	for i, r := range all {
		if r.Outcome != router.ProbeTransportError {
			t.Errorf("all[%d].Outcome = %v, want ProbeTransportError", i, r.Outcome)
		}
	}
}

// TestParallelProbe_EmptyCandidatesIsNoWinner covers the SelectK-
// returned-empty (shouldn't happen given upstream guards, but
// defense in depth).
func TestParallelProbe_EmptyCandidatesIsNoWinner(t *testing.T) {
	winner, all := ParallelProbe(context.Background(), nil, nil, 50*time.Millisecond)
	if winner != -1 || all != nil {
		t.Errorf("empty cands → winner=%d, all=%+v; want (-1, nil)", winner, all)
	}
}
