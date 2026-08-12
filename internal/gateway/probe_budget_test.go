package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

func remoteCandidateWithRTT(peerID string, rttMS uint32) router.Candidate {
	c := phase8RemoteCandidate(peerID)
	c.RTTMS = rttMS
	return c
}

// TestProbeBudgetFor covers the sizing rule that replaced the flat 50 ms
// budget (waired-agent#624). Record of today's behaviour for the exact
// constants; the requirement it encodes is the owner ruling that the
// budget be generous relative to RTT (20260812).
func TestProbeBudgetFor(t *testing.T) {
	tests := []struct {
		name  string
		cands []router.Candidate
		want  time.Duration
	}{
		{
			name:  "no candidates gets the floor",
			cands: nil,
			want:  probeBudgetFloor,
		},
		{
			name:  "a near peer is held at the floor, not 6x a millisecond",
			cands: []router.Candidate{remoteCandidateWithRTT("near", 1)},
			want:  probeBudgetFloor,
		},
		{
			// The mesh in the report measured rtt_ms=52, where the old
			// flat 50 ms budget was under one round trip and the probe
			// needs two (the peer adapter sets DisableKeepAlives).
			name:  "the reported mesh RTT scales past the floor",
			cands: []router.Candidate{remoteCandidateWithRTT("measured", 52)},
			want:  312 * time.Millisecond,
		},
		{
			name:  "a distant peer is capped at the ceiling",
			cands: []router.Candidate{remoteCandidateWithRTT("far", 4000)},
			want:  probeBudgetCeiling,
		},
		{
			// The probes run concurrently under one deadline, so the
			// farthest candidate sets it: sizing to the nearest would cut
			// the others off mid-flight.
			name: "the farthest candidate sets the budget",
			cands: []router.Candidate{
				remoteCandidateWithRTT("near", 1),
				remoteCandidateWithRTT("mid", 52),
				remoteCandidateWithRTT("far", 100),
			},
			want: 600 * time.Millisecond,
		},
		{
			// No disco pong ever matched, which is also every relay-only
			// peer. There is no distance to scale by, and guessing small
			// is what makes a far peer permanently unroutable.
			name:  "an unmeasured peer gets the ceiling",
			cands: []router.Candidate{remoteCandidateWithRTT("relay-only", router.RTTUnknown)},
			want:  probeBudgetCeiling,
		},
		{
			name: "a local candidate contributes nothing",
			cands: []router.Candidate{
				router.NewLocalCandidate(router.Selection{ExecutionMode: "local"}),
			},
			want: probeBudgetFloor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeBudgetFor(tc.cands); got != tc.want {
				t.Errorf("probeBudgetFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSelectAndProbe_PeerSlowerThanOneRTTStillServes is the #624
// regression test: a peer that answers its readiness probe in more time
// than the old flat 50 ms budget allowed, but well inside what its
// measured RTT justifies, must be served rather than 503'd.
//
// Under the previous constant every probe on a mesh measuring 52 ms RTT
// timed out — a probe costs at least two round trips because the peer
// adapter disables connection reuse — so every mesh request failed on
// every host, in both directions.
func TestSelectAndProbe_PeerSlowerThanOneRTTStillServes(t *testing.T) {
	const rttMS = 52
	// Longer than the retired 50 ms budget, comfortably inside the
	// 6×52 ms this peer now earns.
	peer := &stubRT{status: 200, body: readyBody(0, 4), delay: 120 * time.Millisecond}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	sel := &phase8MultiSelector{cands: []router.Candidate{remoteCandidateWithRTT("peer-A", rttMS)}}
	h := buildPhase8Gateway(t, sel, map[string]http.RoundTripper{"peer-A": peer}, upstream.URL)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a peer answering in %v was rejected (body=%s)",
			resp.StatusCode, peer.delay, raw)
	}
}

// TestSelectAndProbe_RetriesBeforeGivingUp pins the owner ruling that a
// probe which does not come back is not a verdict about the peer: it can
// be a lost packet, a relay hiccup, or a peer mid-handshake, so the
// gateway retries rather than reporting a network fault as a mesh state
// (20260812, waired-agent#624).
//
// The single candidate refuses the first round's probe and answers the
// second, so a gateway that gave up after one round fails this test.
func TestSelectAndProbe_RetriesBeforeGivingUp(t *testing.T) {
	flaky := &flakyProbeRT{failUntil: 1, ok: readyBody(0, 4)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	sel := &phase8MultiSelector{cands: []router.Candidate{remoteCandidateWithRTT("peer-A", 5)}}
	h := buildPhase8Gateway(t, sel, map[string]http.RoundTripper{"peer-A": flaky}, upstream.URL)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a transient probe failure was treated as a verdict (body=%s)",
			resp.StatusCode, raw)
	}
	// Without this the test would also pass if the first probe had
	// somehow succeeded, which is not the behaviour under test.
	if got := flaky.calls.Load(); got < 2 {
		t.Errorf("probe calls = %d, want at least 2 — the gateway did not retry", got)
	}
}

// flakyProbeRT fails the first failUntil probes and answers every one
// after that, so a test can distinguish "retried" from "gave up on the
// first round". It records the real call count rather than a bool so the
// assertion can be about how many rounds ran.
type flakyProbeRT struct {
	failUntil int32
	calls     atomic.Int32
	ok        string
}

func (f *flakyProbeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.calls.Add(1) <= f.failUntil {
		return nil, errors.New("connect refused")
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(f.ok)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

// TestSelectAndProbe_WorstCaseWaitStaysBounded is the guard on the
// retry ruling: retries multiply the failure path, and a client waiting
// on an unreachable mesh must still get its 503 in seconds. It pins the
// arithmetic rather than measuring wall clock, so it cannot flake on a
// loaded runner.
func TestSelectAndProbe_WorstCaseWaitStaysBounded(t *testing.T) {
	worst := probeAttempts*probeBudgetCeiling + (probeAttempts-1)*briefQueueDelay
	if worst > 4*time.Second {
		t.Errorf("worst-case wait before an unreachable mesh reports back = %v; "+
			"a client should not hold a request open that long for a 503", worst)
	}
	if probeAttempts < 2 {
		t.Error("probeAttempts < 2 removes the retry the owner ruling requires")
	}
	if probeBudgetFloor > probeBudgetCeiling {
		t.Error("probe budget floor exceeds its ceiling")
	}
}

// TestPhase8Integration_UnansweredProbesAreNotReportedAsCapacity pins
// the attribution split at the observability layer, which is where the
// #624 investigation started from the wrong end.
func TestPhase8Integration_UnansweredProbesAreNotReportedAsCapacity(t *testing.T) {
	if got := selectionErrorReason(router.ErrPeersDidNotAnswer); got != "peers_did_not_answer" {
		t.Errorf("reason for an unanswered mesh = %q, want peers_did_not_answer", got)
	}
	if got := selectionErrorReason(router.ErrAllPeersOverloaded); got != "all_peers_overloaded" {
		t.Errorf("reason for a full mesh = %q, want all_peers_overloaded", got)
	}
	if got := selectionStatus(router.ErrPeersDidNotAnswer); got != http.StatusServiceUnavailable {
		t.Errorf("status for an unanswered mesh = %d, want 503", got)
	}
}

// TestRespondSelectionError_PeersDidNotAnswerHasItsOwnCode keeps the
// wire code distinct from the load one on the OpenAI surface.
func TestRespondSelectionError_PeersDidNotAnswerHasItsOwnCode(t *testing.T) {
	w := httptest.NewRecorder()
	respondSelectionError(w, router.ErrPeersDidNotAnswer)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Code != "waired_peers_did_not_answer" {
		t.Errorf("code = %q, want waired_peers_did_not_answer (body=%s)", env.Error.Code, w.Body.String())
	}
}
