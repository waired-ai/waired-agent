package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// A request that names no model asks the routing mode for a NODE, and
// takes the model that node is running (waired-agent#828, owner ruling
// 2026-08-19). Until this, `waired/default` resolved to the requester's
// own model before routing ran, and the mesh was searched for that model
// alone — so with one model per agent, a pin or a peer-only fleet worked
// only when both ends happened to run the same thing.

// mixedFleet: the requester's default is qwen3-8b-instruct; every peer
// here runs the OTHER catalog model.
func mixedFleet(pinDevice string) Inputs {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("gpu-box", "qwen3:32b-q4_K_M", true, false)},
	}
	in := Inputs{
		Manifests:      []catalog.Manifest{qwen(), bigQwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		DefaultModelID: "qwen3-8b-instruct",
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	}
	if pinDevice != "" {
		in.RoutingMode = state.RoutingModePinned
		in.PinnedPeerDeviceID = pinDevice
	}
	return in
}

func TestNodeFirst_PinnedServesThePinsModel(t *testing.T) {
	sel, err := NewSelector(mixedFleet("gpu-box")).Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("a pin to a peer running another model must still serve: %v", err)
	}
	if sel.Runtime != "remote:gpu-box" {
		t.Fatalf("Runtime = %q, want remote:gpu-box", sel.Runtime)
	}
	if sel.ModelID != "qwen3-32b-instruct" {
		t.Errorf("ModelID = %q, want the pin's model, not the requester's", sel.ModelID)
	}
	if sel.EngineModel != "qwen3:32b-q4_K_M" {
		t.Errorf("EngineModel = %q — the proxied body must name what the pin loads", sel.EngineModel)
	}
}

// The pre-fix failure, kept as a tripwire: with the mesh searched for
// the requester's model only, this fleet has no candidate at all. No pin
// here — a pin would now serve it, which is the next test.
func TestNodeFirst_RequestersModelAloneWouldFindNothing(t *testing.T) {
	in := mixedFleet("")
	_, err := NewSelector(in).Select(t.Context(), Request{Model: "qwen3-8b-instruct"})
	if err == nil {
		t.Fatal("naming a model no peer serves must not silently succeed")
	}
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("err = %v, want ErrModelNotReady", err)
	}
}

func TestNodeFirst_PeerOnlyTakesWhateverTheFleetRuns(t *testing.T) {
	in := mixedFleet("")
	in.RoutingMode = state.RoutingModePeerOnly
	sel, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("peer-only must serve from the fleet's own models: %v", err)
	}
	if sel.ModelID != "qwen3-32b-instruct" {
		t.Errorf("ModelID = %q, want the peer's model", sel.ModelID)
	}
}

// Auto with nothing ready locally is the engine-less node's path.
func TestNodeFirst_AutoWithNoLocalModelReachesTheMesh(t *testing.T) {
	sel, err := NewSelector(mixedFleet("")).Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("auto with no local model must reach the mesh: %v", err)
	}
	if sel.ExecutionMode != "remote" {
		t.Fatalf("ExecutionMode = %q, want remote", sel.ExecutionMode)
	}
}

// A ready local model still wins in auto: node-first changes which peers
// are eligible, not whether this host serves its own request.
func TestNodeFirst_AutoStillPrefersAReadyLocalModel(t *testing.T) {
	in := mixedFleet("")
	in.LocalState = readyState()
	sel, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "local" || sel.ModelID != "qwen3-8b-instruct" {
		t.Fatalf("ExecutionMode=%q ModelID=%q, want local / qwen3-8b-instruct", sel.ExecutionMode, sel.ModelID)
	}
}

// Naming a model is still how you choose one. Without a pin, an
// explicitly named model is matched strictly — routing has no node to
// defer to, so substituting some other model would be a guess.
func TestNodeFirst_NamedModelIsStillMatchedStrictly(t *testing.T) {
	in := mixedFleet("")
	in.RoutingMode = state.RoutingModePeerOnly
	_, err := NewSelector(in).Select(t.Context(), Request{Model: "qwen3-8b-instruct"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("err = %v, want ErrModelNotReady — a named model must not be substituted", err)
	}
}

// The 2026-05-19 ruling's second half, revised 2026-08-19: a pin names a
// node, so it wins over the model name too.
func TestNodeFirst_PinWinsOverANamedModel(t *testing.T) {
	in := mixedFleet("gpu-box")
	in.MeshSnapshotFn = func() inferencemesh.Snapshot {
		return inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPeer("gpu-box", "qwen3:32b-q4_K_M", true, false),
			mkPeer("other", "qwen3:8b-q4_K_M", true, false),
		}}
	}
	sel, err := NewSelector(in).Select(t.Context(), Request{Model: "qwen3-8b-instruct"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:gpu-box" {
		t.Fatalf("Runtime = %q — the pin must win over the peer that has the named model", sel.Runtime)
	}
	if sel.ModelID != "qwen3-32b-instruct" {
		t.Errorf("ModelID = %q, want the pin's model", sel.ModelID)
	}
	var named bool
	for _, r := range sel.Decision.Reason {
		if strings.Contains(r, "a pin names a node") {
			named = true
		}
	}
	if !named {
		t.Errorf("the substitution must be named in the reasons; got %#v", sel.Decision.Reason)
	}
}

// The same fleet with a pin: nobody serves the named model, so there is
// no peer to soft-fall to either. The pin serves it.
func TestNodeFirst_PinWinsWhenNobodyServesTheNamedModel(t *testing.T) {
	sel, err := NewSelector(mixedFleet("gpu-box")).Select(t.Context(), Request{Model: "qwen3-8b-instruct"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:gpu-box" || sel.ModelID != "qwen3-32b-instruct" {
		t.Fatalf("Runtime=%q ModelID=%q, want remote:gpu-box / qwen3-32b-instruct", sel.Runtime, sel.ModelID)
	}
	var named bool
	for _, r := range sel.Decision.Reason {
		if strings.Contains(r, "a pin names a node") {
			named = true
		}
	}
	if !named {
		t.Errorf("the substitution must be named in the reasons; got %#v", sel.Decision.Reason)
	}
}

// A pin advertising nothing the catalog knows has no model to serve
// with, so the request does fall through to a peer that can.
func TestNodeFirst_PinRunningAnUnknownModelStillFallsThrough(t *testing.T) {
	in := mixedFleet("stranger")
	in.MeshSnapshotFn = func() inferencemesh.Snapshot {
		return inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPeer("stranger", "totally-other-model:7b", true, false),
			mkPeer("gpu-box", "qwen3:32b-q4_K_M", true, false),
		}}
	}
	sel, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:gpu-box" {
		t.Fatalf("Runtime = %q, want remote:gpu-box", sel.Runtime)
	}
}

// A tier is a promise about the serving node, so it outranks the pin:
// the filters that drop any other candidate drop the pin's too.
func TestNodeFirst_PinStillObeysTheWindowFilter(t *testing.T) {
	in := mixedFleet("gpu-box")
	in.MeshSnapshotFn = func() inferencemesh.Snapshot {
		snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPeer("gpu-box", "qwen3:32b-q4_K_M", true, false),
		}}
		snap.Peers[0].InferenceState.ContextWindow = 8192
		return snap
	}
	_, err := NewSelector(in).Select(t.Context(),
		Request{Model: "waired/default", MinContextWindow: 200000})
	if err == nil {
		t.Fatal("a pin that cannot hold the demanded window must not be selected")
	}
}

// The union want set has to resolve one engine identifier to one model,
// the same way on every host, or two nodes running the same tag would
// report different models.
func TestWantSetsFor_CollisionResolvesToTheStrongerModel(t *testing.T) {
	weak := qwen()
	weak.ModelID = "weak"
	weak.Variants[0].ParamCount = 8
	weak.Variants[0].QuantizationTier = 4
	strong := qwen()
	strong.ModelID = "strong"
	strong.Variants[0].ParamCount = 32
	strong.Variants[0].QuantizationTier = 4

	for _, order := range [][]catalog.Manifest{{weak, strong}, {strong, weak}} {
		o, _ := wantSetsFor(order)
		if got := o["qwen3:8b-q4_K_M"].manifest.ModelID; got != "strong" {
			t.Errorf("collision resolved to %q, want strong (order-independent)", got)
		}
	}
}

// The mesh branches used to borrow the local branch's sentence, so a
// pinned miss read `model is not in ready state on disk: "X"
// state="ready"` — a contradiction in one line, about a machine the
// request was never going to run on (waired-agent#828).
func TestMeshMiss_NamesTheMeshNotTheDisk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  state.RoutingMode
		model string
		want  string
	}{
		{"pinned, named model", state.RoutingModePinned, "qwen3-8b-instruct",
			`router: no mesh peer serves "qwen3-8b-instruct" (routing=pinned); local state="ready"`},
		{"peer-only, no model named", state.RoutingModePeerOnly, "waired/default",
			`router: no mesh peer is available (routing=peer-only); local state="ready"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Inputs{
				Manifests:      []catalog.Manifest{qwen()},
				LocalState:     readyState(),
				Hardware:       goodHardware(),
				Runtimes:       registryWithOllama(),
				DefaultModelID: "qwen3-8b-instruct",
				MeshSnapshotFn: func() inferencemesh.Snapshot {
					return inferencemesh.Snapshot{
						Peers: []inferencemesh.PeerView{mkPeer("up-but-idle", "unknown:7b", true, false)},
					}
				},
				RoutingMode: tc.mode,
			}
			if tc.mode == state.RoutingModePinned {
				in.PinnedPeerDeviceID = "up-but-idle"
			}
			_, err := NewSelector(in).Select(t.Context(), Request{Model: tc.model})
			if err == nil {
				t.Fatal("want an error")
			}
			if err.Error() != tc.want {
				t.Errorf("message =\n  %q\nwant\n  %q", err.Error(), tc.want)
			}
			// Every gateway mapping keys on the sentinel; only the
			// sentence changed.
			if !errors.Is(err, ErrModelNotReady) {
				t.Errorf("errors.Is(err, ErrModelNotReady) = false")
			}
		})
	}
}
