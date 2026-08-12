package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// TestSelectK_CandidateCarriesMeasuredRTT is the router half of the
// waired-agent#624 fix. The gateway sizes each readiness-probe round from
// how far away the peers are, and the Selector is the only layer holding
// that measurement — so it has to reach the Candidate.
//
// Record of today's behaviour: the wire shape of Candidate.RTTMS is not a
// published contract, only an internal hand-off between the two layers.
func TestSelectK_CandidateCarriesMeasuredRTT(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-measured", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-unmeasured", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		// Only one of the two peers has ever produced a matched pong,
		// which is what a relay-only peer also looks like.
		LocalRTT: func() map[string]uint32 { return map[string]uint32{"peer-measured": 52} },
	})

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	got := map[string]uint32{}
	for _, c := range cands {
		got[c.PeerID] = c.RTTMS
	}
	if got["peer-measured"] != 52 {
		t.Errorf("measured peer RTTMS = %d, want 52", got["peer-measured"])
	}
	// Not 0: a consumer scaling a wait by RTT must be able to tell "one
	// millisecond away" from "never measured", and 0 would read as the
	// closest possible peer.
	if got["peer-unmeasured"] != RTTUnknown {
		t.Errorf("unmeasured peer RTTMS = %d, want RTTUnknown (%d)", got["peer-unmeasured"], uint32(RTTUnknown))
	}
}

// TestSelectK_ReasonsRenderUnmeasuredRTT is the display half of the same
// fact (waired-agent#714). The sentinel reached `waired infer --explain`
// as 4294967295 — 2^32-1 presented as a measurement.
//
// The two halves are asserted in one test on purpose: the fix is "render
// it differently", and the failure mode it must not become is "store it
// differently". A future change that zeroes RTTMS to tidy the display
// would satisfy the string assertion and break the budget the value
// feeds (gateway.probeBudgetFor gives RTTUnknown the ceiling), so the
// value assertion is repeated here rather than left to the test above.
func TestSelectK_ReasonsRenderUnmeasuredRTT(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-measured", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-unmeasured", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalRTT:       func() map[string]uint32 { return map[string]uint32{"peer-measured": 52} },
	})

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}

	want := map[string]string{
		"peer-measured":   "rtt_ms=52,",
		"peer-unmeasured": "rtt_ms=unmeasured,",
	}
	for _, c := range cands {
		reasons := strings.Join(c.Decision.Reason, "\n")
		if w, ok := want[c.PeerID]; ok && !strings.Contains(reasons, w) {
			t.Errorf("peer %q reasons do not contain %q:\n%s", c.PeerID, w, reasons)
		}
		if strings.Contains(reasons, "4294967295") {
			t.Errorf("peer %q reasons print the raw sentinel:\n%s", c.PeerID, reasons)
		}
		// The value the probe budget reads is untouched by the display fix.
		if c.PeerID == "peer-unmeasured" && c.RTTMS != RTTUnknown {
			t.Errorf("unmeasured peer RTTMS = %d, want RTTUnknown — the sentinel must survive rendering", c.RTTMS)
		}
	}
}
