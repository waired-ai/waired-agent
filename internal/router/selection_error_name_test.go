package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestSelectK_CapacityErrorNamesTheRowTheClientPicked is waired-agent#1366's
// wording half.
//
// Measured with OpenCode on the peers-only row: a subagent failed with
// `router: every matching mesh peer is at capacity: "qwen3.5-4b"`, the
// requester's own default model, which no peer on that row runs. A row names
// a computer rather than a model, so the router resolves the alias to this
// host's default and the error used to name that. Record of today's
// behaviour for the catalog-id case; the row case follows the row the client
// picked.
func TestSelectK_CapacityErrorNamesTheRowTheClientPicked(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	release, _ := tracker.Acquire("peer-A", 1)
	defer release()

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
		RoutingMode:    state.RoutingModePeerOnly,
	})

	_, err := s.SelectK(t.Context(), Request{Model: DefaultModelAlias, NodeDirective: "waired/peer"}, 3)
	if !errors.Is(err, ErrAllPeersOverloaded) {
		t.Fatalf("err = %v, want ErrAllPeersOverloaded", err)
	}
	if !strings.Contains(err.Error(), `"waired/peer"`) {
		t.Errorf("err = %q, want it to name the row the client picked", err.Error())
	}

	_, err = s.SelectK(t.Context(), Request{Model: qwen().ModelID}, 3)
	if !strings.Contains(err.Error(), `"`+qwen().ModelID+`"`) {
		t.Errorf("err = %q, want the catalog id when no row was picked", err.Error())
	}
}
