package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// TestSelectK_ReasonsNameTheFrameTheFiguresCameFrom is waired-agent#713.
//
// PRODUCT CONTRACT (ratifying source: waired-agent#713, "the `--explain`
// reasons string does not record the provenance of the number it prints,
// so the run cannot be re-diagnosed after the fact"): a mesh candidate's
// reasons report how old the network-map frame its figures were read off
// was. The exact spelling and position of the field are a record of
// today's behaviour, not a contract.
//
// The figures are taken from the rc8 hardware run that filed the issue
// (capacities 1 and 3 at map_age_ms 46608, against a 90000 ms staleness
// threshold) because they are realistic, NOT because they were in
// conflict there: re-checking the captured snapshots showed the 1 and
// the 3 belonged to two different peers and both were correct. What the
// run really lacked is what this test pins — a row of figures with no
// statement of how old the frame behind them was.
func TestSelectK_ReasonsNameTheFrameTheFiguresCameFrom(t *testing.T) {
	snap := inferencemesh.Snapshot{
		MapReceivedAt: "2026-08-12T12:18:03.123456789Z",
		MapAgeMS:      46608,
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-a", "qwen3:8b-q4_K_M", 1),
			mkPeerWithCap("peer-b", "qwen3:8b-q4_K_M", 3),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("len(cands) = %d, want 2", len(cands))
	}

	// Both peers advertise a different capacity in the SAME frame, and
	// both rows report the same frame age: the age qualifies the figures,
	// it is not a property of the peer.
	wantCap := map[string]string{"peer-a": "cap=1,", "peer-b": "cap=3,"}
	for _, c := range cands {
		reasons := strings.Join(c.Decision.Reason, "\n")
		if !strings.Contains(reasons, "map_age_ms=46608") {
			t.Errorf("peer %q reasons do not name the frame age:\n%s", c.PeerID, reasons)
		}
		if w := wantCap[c.PeerID]; w != "" && !strings.Contains(reasons, w) {
			t.Errorf("peer %q reasons do not contain %q:\n%s", c.PeerID, w, reasons)
		}
		// The point of the field is that the two are readable together:
		// a capacity is only interpretable next to the age of the frame
		// it was read off.
		if !strings.Contains(reasons, "cap=") || !strings.Contains(reasons, "map_age_ms=") {
			t.Errorf("peer %q must report capacity and frame age in one line:\n%s", c.PeerID, reasons)
		}
	}
}

// TestSelectK_FrameAgeIsReadPerSelect pins that the reported age comes
// from the snapshot this Select saw, not from one captured earlier.
//
// Record of today's behaviour. It matters because the whole value of the
// field is that two readings taken minutes apart can be compared: an age
// frozen at Selector construction would be worse than no field at all,
// since it would look like evidence.
func TestSelectK_FrameAgeIsReadPerSelect(t *testing.T) {
	snap := inferencemesh.Snapshot{
		MapAgeMS: 1200,
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-a", "qwen3:8b-q4_K_M", 2),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})

	first, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 1)
	if err != nil {
		t.Fatalf("SelectK (first): %v", err)
	}
	if got := strings.Join(first[0].Decision.Reason, "\n"); !strings.Contains(got, "map_age_ms=1200") {
		t.Errorf("first Select does not report the frame age it saw:\n%s", got)
	}

	// A newer frame arrived between the two calls.
	snap.MapAgeMS = 37
	second, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 1)
	if err != nil {
		t.Fatalf("SelectK (second): %v", err)
	}
	got := strings.Join(second[0].Decision.Reason, "\n")
	if !strings.Contains(got, "map_age_ms=37") {
		t.Errorf("second Select does not report the newer frame age:\n%s", got)
	}
	if strings.Contains(got, "map_age_ms=1200") {
		t.Errorf("second Select still reports the first frame's age:\n%s", got)
	}
}

// TestSelectK_LocalReasonsHaveNoFrameAge keeps the field where it means
// something. A local selection reads no network-map frame, so reporting
// an age for it would attach a provenance the figure does not have —
// which is the failure #713 is about, pointed the other way.
//
// Record of today's behaviour.
func TestSelectK_LocalReasonsHaveNoFrameAge(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
		// Wired, and deliberately fresh: the local path must not report a
		// frame age even when a snapshot is available to read one from.
		MeshSnapshotFn: func() inferencemesh.Snapshot {
			return inferencemesh.Snapshot{MapAgeMS: 500}
		},
	})

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 1 || cands[0].ExecutionMode != "local" {
		t.Fatalf("want exactly 1 local candidate, got %d", len(cands))
	}
	if got := strings.Join(cands[0].Decision.Reason, "\n"); strings.Contains(got, "map_age_ms=") {
		t.Errorf("local candidate reports a network-map frame age:\n%s", got)
	}
}
