package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// A round in which peers answered and none could take the request ends in
// ErrAllPeersOverloaded, whose sentence says every peer is at capacity.
// That is only true when they were full. A peer that is running its
// benchmark, has its engine down or is paused answered too, and was
// reported as full (waired-agent#1369).

func notReadyStatus(mod func(*router.HealthStatus)) router.ProbeResult {
	s := router.HealthStatus{EngineReady: true, ShareEnabled: true, CapacityTotal: 1}
	mod(&s)
	return router.ProbeResult{Outcome: router.ProbeOK, Status: s}
}

func TestNotReadyMeshError(t *testing.T) {
	full := notReadyStatus(func(s *router.HealthStatus) { s.CapacityUsed = 1 })
	measuring := notReadyStatus(func(s *router.HealthStatus) { s.Measuring = true })
	engineDown := notReadyStatus(func(s *router.HealthStatus) { s.EngineReady = false })
	cands := []router.Candidate{
		{PeerID: "dev_a", PeerDisplayID: "pub-node-a", ExecutionMode: "remote"},
		{PeerID: "dev_b", PeerDisplayID: "pub-node-b", ExecutionMode: "remote"},
	}

	for _, tc := range []struct {
		name    string
		results []router.ProbeResult
		// wantBare: the sentinel itself, because every answering peer was full.
		wantBare bool
		wantIn   []string
	}{
		{name: "every peer was full", results: []router.ProbeResult{full, full}, wantBare: true},
		{name: "full, and the other never answered", results: []router.ProbeResult{full, {Outcome: router.ProbeTransportError}}, wantBare: true},
		{name: "the only answering peer is running its benchmark",
			results: []router.ProbeResult{measuring, {Outcome: router.ProbeTransportError}},
			wantIn:  []string{`(tried "pub-node-a": running its benchmark (takes a few minutes); "pub-node-b": no answer)`}},
		{name: "one full, one with its engine down",
			results: []router.ProbeResult{full, engineDown},
			wantIn:  []string{`router: no matching mesh peer is ready (tried "pub-node-a": at capacity; "pub-node-b": engine not ready)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := notReadyMeshError(probedSelection{cands: cands, probeResults: tc.results})
			if !errors.Is(err, router.ErrAllPeersOverloaded) {
				t.Fatalf("err = %v, want it to match ErrAllPeersOverloaded so the status and code stay", err)
			}
			if errors.Is(err, router.ErrPeersDidNotAnswer) {
				t.Errorf("err also matches ErrPeersDidNotAnswer: %v", err)
			}
			if tc.wantBare {
				if err != router.ErrAllPeersOverloaded {
					t.Errorf("err = %v, want the bare sentinel", err)
				}
				return
			}
			msg := err.Error()
			if strings.Contains(msg, router.ErrAllPeersOverloaded.Error()) {
				t.Errorf("message still makes the mesh-wide capacity claim: %q", msg)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q: %q", want, msg)
				}
			}
			for _, real := range []string{"dev_a", "dev_b"} {
				if strings.Contains(msg, real) {
					t.Errorf("real device identifier %q in the client-visible message: %q", real, msg)
				}
			}
		})
	}
}

// TestPhase8Integration_MeasuringPeerIsNotReportedAsFull drives the whole
// handler: the only candidate answers its probe with measuring:true. The
// status and code stay those of the capacity case (the brief queue waits
// both out the same way), and the sentence says what the peer is doing.
func TestPhase8Integration_MeasuringPeerIsNotReportedAsFull(t *testing.T) {
	measuring := &stubRT{status: 200, body: `{"engine_ready":true,"model_id":"qwen3:8b-q4_K_M","capacity_total":1,"capacity_used":0,"paused":false,"share_enabled":true,"measuring":true}`}
	sel := &phase8MultiSelector{
		cands: []router.Candidate{phase8RemoteCandidate("peer-A")},
	}
	h := buildPhase8Gateway(t, sel, map[string]http.RoundTripper{"peer-A": measuring}, "")

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Error.Code != "waired_all_peers_overloaded" {
		t.Errorf("error.code = %q, want waired_all_peers_overloaded (body=%s)", env.Error.Code, raw)
	}
	if strings.Contains(env.Error.Message, router.ErrAllPeersOverloaded.Error()) {
		t.Errorf("message = %q, still says every peer is at capacity", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "running its benchmark (takes a few minutes)") {
		t.Errorf("message = %q, want it to say the peer is running its benchmark", env.Error.Message)
	}
}
