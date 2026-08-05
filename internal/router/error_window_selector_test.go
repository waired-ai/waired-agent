package router

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// TestErrorWindow_FeedsTheSelectorTieBreak closes the loop between the
// two halves of the per-peer failure signal.
//
// PRODUCT CONTRACT (waired-agent#281): outcomes recorded against a peer
// decide the same-score tie-break, and they age out.
//
// TestSelector_MeshFallback_ErrorRateTieBreak already covers the
// Selector's half against a hand-written snapshot closure, and
// error_window_test.go covers the window's half against direct Record
// calls. Neither notices that nothing in production ever calls Record —
// which is what #281 was. This one runs a real ErrorWindow into a real
// Selector, so the seam itself is exercised; the gateway half that
// supplies the Record calls is pinned by
// TestPeerOutcome_RemoteSuccessIsRecorded and its siblings in
// internal/gateway.
func TestErrorWindow_FeedsTheSelectorTieBreak(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	clk := newMockClock(time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	// peer-A is the one the deviceID tie-break would otherwise pick, so
	// a passing assertion below cannot come from the ordering alone.
	for range 8 {
		w.Record("peer-A", false)
		w.Record("peer-B", true)
	}

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalErrors:    w.Snapshot,
	})

	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-B" {
		t.Fatalf("a peer that failed every request must lose the tie-break; got %q", sel.Runtime)
	}

	// A minute of silence empties the window, and the Selector must see
	// that: the signal is evidence about recent behaviour, not a verdict
	// a peer carries until it is restarted.
	clk.advance(90 * time.Second)
	sel, err = s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select after the window aged out: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("with the window drained the deviceID tie-break decides; got %q, want remote:peer-A", sel.Runtime)
	}
}
