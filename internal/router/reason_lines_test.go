package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// The reason list `waired infer --explain` prints is the only account a
// user gets of why a request went where it went, so a line in it is a
// claim of fact. Three of them were not (waired-agent#854, #888).

// PIN: product contract —
// docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md §5
// already words the mesh MISS the same way ("local state=..." after the
// routing note); waired-agent#854 is the ruling that the SUCCESS path
// must not contradict it.
//
// RoutingModeLocalOnly never reaches a mesh branch — it returns before
// tryMeshFallbackK — so its row records today's routing rather than a
// promise. It is here so that a future branch reaching this function
// with local-only produces something readable instead of "".
func TestLocalBypassReason(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       state.RoutingMode
		servingOff bool
		localState string
		want       string
	}{{
		name: "pinned names the node, not the local state",
		mode: state.RoutingModePinned, localState: catalog.ModelStateReady,
		want: `routing=pinned: a pin names a node, so this host's own engine is not consulted; local state for "m" is "ready"`,
	}, {
		name: "pinned with the model absent still reports the real state",
		mode: state.RoutingModePinned, localState: catalog.ModelStateNotPresent,
		want: `routing=pinned: a pin names a node, so this host's own engine is not consulted; local state for "m" is "not_present"`,
	}, {
		name: "peer-only says the operator opted this host out",
		mode: state.RoutingModePeerOnly, localState: catalog.ModelStateReady,
		want: `routing=peer-only: this host is set not to serve; local state for "m" is "ready"`,
	}, {
		name: "peer-preferred says local was second choice, not unavailable",
		mode: state.RoutingModePeerPreferred, localState: catalog.ModelStateReady,
		want: `routing=peer-preferred: a mesh peer is tried before this host's own engine; local state for "m" is "ready"`,
	}, {
		name: "auto reached the mesh because local had no candidate",
		mode: state.RoutingModeAuto, localState: catalog.ModelStateNotPresent,
		want: `local state for "m" is "not_present", so this host has no candidate; trying the mesh`,
	}, {
		name: "the empty mode is auto",
		mode: "", localState: "downloading",
		want: `local state for "m" is "downloading", so this host has no candidate; trying the mesh`,
	}, {
		name: "local-only takes the auto wording (unreachable today)",
		mode: state.RoutingModeLocalOnly, localState: catalog.ModelStateReady,
		want: `local state for "m" is "ready", so this host has no candidate; trying the mesh`,
	}, {
		name: "serving off adds nothing — the line above already said it",
		mode: state.RoutingModeAuto, servingOff: true, localState: catalog.ModelStateReady,
		want: "",
	}, {
		name: "serving off wins over the mode",
		mode: state.RoutingModePinned, servingOff: true, localState: catalog.ModelStateReady,
		want: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := localBypassReason(tc.mode, tc.servingOff, "m", tc.localState)
			if got != tc.want {
				t.Errorf("localBypassReason\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

func TestWithReason(t *testing.T) {
	base := []string{"a", "b"}
	if got := withReason(base, ""); len(got) != 2 {
		t.Fatalf("empty reason must not grow the slice: %v", got)
	}
	// The peer-preferred branch keeps appending to its own slice after a
	// mesh attempt returns nothing, so the two must not share a backing
	// array.
	grown := withReason(base[:1], "mesh")
	base[1] = "mutated-by-the-local-path"
	if grown[1] != "mesh" {
		t.Errorf("withReason aliased the caller's array: grown = %v", grown)
	}
}

// PIN: product contract — waired-agent#888. The name is what
// `waired peers list` shows and the identifier is what its DEVICE-ID
// column shows; `waired worker set --pin` accepts either, so a reason a
// reader carries to --pin must carry both.
func TestPeerLabel(t *testing.T) {
	for _, tc := range []struct {
		name, displayName, displayID, want string
	}{
		{"a named machine carries both", "linux-gpu", "dev_abc", `"linux-gpu" (dev_abc)`},
		{"an unnamed peer is just its id", "", "dev_abc", `"dev_abc"`},
		{"a pseudonym IS the id, so it is not repeated", "guest-a7f3", "guest-a7f3", `"guest-a7f3"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerLabel(tc.displayName, tc.displayID); got != tc.want {
				t.Errorf("peerLabel(%q, %q) = %q, want %q", tc.displayName, tc.displayID, got, tc.want)
			}
		})
	}
}

// The measured rc10 defect, replayed. On a host pinned to a peer, with
// the model READY on local disk, `waired infer --explain` printed
// `local state for "qwen3-8b-instruct" is not ready`. It was emitted
// unconditionally by makeMeshCandidate, so it was false whenever local
// was in fact ready — and a pin never consults local readiness at all.
//
// PIN: product contract — waired-agent#854 (measured on hardware
// 2026-08-20) + docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md.
func TestSelectK_PinnedRemoteDoesNotClaimTheModelIsNotReadyLocally(t *testing.T) {
	// Both ends run the SAME model, so the line names the right model
	// and is still false — which is the half the issue understated.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("gpu-box", "qwen3:8b-q4_K_M", true, false)},
	}
	in := Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         readyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		DefaultModelID:     "qwen3-8b-instruct",
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "gpu-box",
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
	}

	// The defect reproduced with the model named as well, so the fix is
	// checked on both entry paths.
	for _, model := range []string{"waired/default", "qwen3-8b-instruct"} {
		t.Run(model, func(t *testing.T) {
			sel, err := NewSelector(in).Select(t.Context(), Request{Model: model})
			if err != nil {
				t.Fatalf("Select(%q): %v", model, err)
			}
			if sel.ExecutionMode != "remote" {
				t.Fatalf("ExecutionMode = %q — the pin must still win over a ready local model", sel.ExecutionMode)
			}
			joined := strings.Join(sel.Decision.Reason, "\n")
			if strings.Contains(joined, "is not ready") {
				t.Errorf("the trace still claims the model is not ready locally, and it IS ready:\n%s", joined)
			}
			if !strings.Contains(joined, `routing=pinned: a pin names a node`) {
				t.Errorf("the trace does not say why local was bypassed:\n%s", joined)
			}
			if !strings.Contains(joined, `local state for "qwen3-8b-instruct" is "ready"`) {
				t.Errorf("the trace does not report the real local state:\n%s", joined)
			}
		})
	}
}

// PIN: product contract — waired-agent#888, and
// docs-site/src/content/docs/troubleshooting/slow-or-wrong.md, which already
// tells the user that --explain "names the computer that served it".
func TestSelectK_ReasonsNameThePeerTheWayPeersListDoes(t *testing.T) {
	peer := mkPeer("dev_abc123", "qwen3:8b-q4_K_M", true, false)
	peer.DeviceName = "linux-gpu"
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{peer}}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "qwen3-8b-instruct"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	joined := strings.Join(sel.Decision.Reason, "\n")
	if !strings.Contains(joined, `peer "linux-gpu" (dev_abc123)`) {
		t.Errorf("the reason names the peer by identifier alone:\n%s", joined)
	}
}

// The capability line reported the REQUESTER's manifest, unconditionally
// — including for the every-day request that requires nothing, where no
// filtering happened at all.
//
// PIN: record of today's behaviour. Nothing ratifies dropping the line;
// what is ratified is that it must not describe a model that did not
// answer (waired-agent#854).
func TestSelectK_CapabilityLineOnlyWhenSomethingWasRequired(t *testing.T) {
	newSel := func() *Selector {
		return NewSelector(Inputs{
			Manifests:  []catalog.Manifest{qwen()},
			LocalState: readyState(),
			Hardware:   goodHardware(),
			Runtimes:   registryWithOllama(),
		})
	}
	bare, err := newSel().Select(t.Context(), Request{Model: "qwen3-8b-instruct"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if j := strings.Join(bare.Decision.Reason, "\n"); strings.Contains(j, "capability filter") {
		t.Errorf("a request that required nothing reports a filter that did not run:\n%s", j)
	}

	asked, err := newSel().Select(t.Context(), Request{
		Model:        "qwen3-8b-instruct",
		Requirements: Requirements{NeedJSONMode: true},
	})
	if err != nil {
		t.Fatalf("Select with a requirement: %v", err)
	}
	j := strings.Join(asked.Decision.Reason, "\n")
	if !strings.Contains(j, `capability filter passed for "qwen3-8b-instruct"`) {
		t.Errorf("a request that required json_mode does not report the filter, or does not say for what:\n%s", j)
	}
}
