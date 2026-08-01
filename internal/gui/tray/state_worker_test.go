package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func baseSnapshotWithWorker(worker *management.WorkerResponse, mesh *inferencemesh.Snapshot) Snapshot {
	return Snapshot{
		Health: HealthOnline,
		Identity: &management.IdentityView{
			Enrolled:     true,
			DeviceName:   "dev",
			NetworkName:  "net",
			AccountEmail: "alice@example.com",
		},
		Status: &management.Status{Phase: "active"},
		Inference: &management.InferenceStatus{
			SubsystemState: "ready",
			Worker:         worker,
		},
		Mesh: mesh,
	}
}

func TestApplyWorker_HiddenWhenWorkerNil(t *testing.T) {
	snap := baseSnapshotWithWorker(nil, nil)
	m := Update(snap)
	if m.ShowWorker {
		t.Errorf("ShowWorker must stay false when Worker is nil")
	}
}

func TestApplyWorker_AutoModeSelected(t *testing.T) {
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, nil)
	m := Update(snap)
	if !m.ShowWorker {
		t.Fatal("ShowWorker should be true when Worker present")
	}
	if m.WorkerActiveLabel != "Worker: auto" {
		t.Errorf("active label = %q, want 'Worker: auto'", m.WorkerActiveLabel)
	}
	if len(m.WorkerModes) != workerModeSlots {
		t.Fatalf("want %d mode rows, got %d", workerModeSlots, len(m.WorkerModes))
	}
	if !m.WorkerModes[0].Selected {
		t.Errorf("auto row should be Selected: %+v", m.WorkerModes[0])
	}
	if m.WorkerShowClearPin {
		t.Errorf("WorkerShowClearPin must be false outside pinned mode")
	}
}

func TestApplyWorker_LocalOnlySelected(t *testing.T) {
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeLocalOnly}, nil)
	m := Update(snap)
	if m.WorkerActiveLabel != "Worker: local only" {
		t.Errorf("active label = %q", m.WorkerActiveLabel)
	}
	if !m.WorkerModes[1].Selected {
		t.Errorf("local-only row should be Selected: %+v", m.WorkerModes)
	}
}

func TestApplyWorker_PeerPreferredSelected(t *testing.T) {
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModePeerPreferred}, nil)
	m := Update(snap)
	if m.WorkerActiveLabel != "Worker: peer preferred" {
		t.Errorf("active label = %q", m.WorkerActiveLabel)
	}
	if !m.WorkerModes[2].Selected {
		t.Errorf("peer-preferred row should be Selected: %+v", m.WorkerModes)
	}
}

func TestApplyWorker_PinnedActiveLabel(t *testing.T) {
	w := &management.WorkerResponse{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: "dev_lin",
		PinnedPeerName:     "linux-gpu",
		PinnedPeerStatus:   "ok",
	}
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:   "dev_lin",
			DeviceName: "linux-gpu",
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Models:    []string{"qwen3:8b-q4_K_M"},
			},
		}},
	}
	m := Update(baseSnapshotWithWorker(w, mesh))
	if m.WorkerActiveLabel != "Worker: linux-gpu (pinned)" {
		t.Errorf("active label = %q", m.WorkerActiveLabel)
	}
	if !m.WorkerShowClearPin {
		t.Errorf("WorkerShowClearPin should be true when pinned")
	}
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("want 1 pin entry, got %d", len(m.WorkerPinEntries))
	}
	pe := m.WorkerPinEntries[0]
	if !pe.Selected {
		t.Errorf("pinned peer should be Selected")
	}
	if !pe.Available {
		t.Errorf("pinned peer should be Available")
	}
	if pe.Label != "linux-gpu (qwen3:8b-q4_K_M)" {
		t.Errorf("label = %q", pe.Label)
	}
}

func TestApplyWorker_FiltersOutNonInferenceCandidates(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID: "dev_capable", DeviceName: "linux-gpu",
				InferenceState: &signer.InferenceState{
					Reachable: true, Type: signer.InferenceTypeOllama, Models: []string{"qwen3:8b"},
				},
			},
			{DeviceID: "dev_no_engine", DeviceName: "win-laptop"}, // InferenceState=nil → filtered
		},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if len(m.WorkerPinEntries) != 1 {
		t.Errorf("non-inference peer should be filtered: got %d entries (%+v)", len(m.WorkerPinEntries), m.WorkerPinEntries)
	}
	if m.WorkerPinEntries[0].DeviceID != "dev_capable" {
		t.Errorf("wrong entry survived: %+v", m.WorkerPinEntries)
	}
}

func TestApplyWorker_StalePeerShownAsUnavailable(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:   "dev_stale",
			DeviceName: "peer-stale",
			Stale:      true,
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Models:    []string{"qwen3:8b"},
			},
		}},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("stale peer should still appear (unavailable), got %d entries", len(m.WorkerPinEntries))
	}
	pe := m.WorkerPinEntries[0]
	if pe.Available {
		t.Errorf("stale peer should not be Available: %+v", pe)
	}
	if pe.Label != "peer-stale (unavailable)" {
		t.Errorf("stale label = %q", pe.Label)
	}
}

func TestApplyWorker_PinAbsentAppendsAbsentRow(t *testing.T) {
	// Pin set but peer fell out of the mesh snapshot.
	w := &management.WorkerResponse{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: "dev_gone",
		PinnedPeerName:     "ghost",
		PinnedPeerStatus:   "absent",
	}
	mesh := &inferencemesh.Snapshot{} // empty
	m := Update(baseSnapshotWithWorker(w, mesh))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("absent pin row should be appended, got %d", len(m.WorkerPinEntries))
	}
	pe := m.WorkerPinEntries[0]
	if pe.Available {
		t.Errorf("absent pin must not be Available")
	}
	if pe.Label != "ghost (absent)" {
		t.Errorf("absent label = %q", pe.Label)
	}
	if !pe.Selected {
		t.Errorf("absent pin should still be marked Selected so the operator sees their choice")
	}
}

func TestApplyWorker_HiddenWhileConnecting(t *testing.T) {
	// Mid-transition phase → worker submenu should NOT render. Mirrors
	// the catalog submenu gating.
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, nil)
	snap.Status.Phase = "starting"
	m := Update(snap)
	if m.ShowWorker {
		t.Errorf("worker submenu should stay hidden while connecting")
	}
}

func TestApplyWorker_HiddenWhenDaemonDown(t *testing.T) {
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, nil)
	snap.Health = HealthOffline
	m := Update(snap)
	if m.ShowWorker {
		t.Errorf("daemon-down must collapse worker submenu")
	}
}

// TestApplyWorker_PeerOnlySelected covers the mode added in #327: the
// mirror image of Local only, for an operator who wants this computer
// left alone. Row order is a product contract — the renderer maps rows
// to pre-allocated slots positionally, so a reordering silently moves
// what a click does.
func TestApplyWorker_PeerOnlySelected(t *testing.T) {
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModePeerOnly}, nil)
	m := Update(snap)
	if m.WorkerActiveLabel != "Worker: peer only" {
		t.Errorf("active label = %q, want 'Worker: peer only'", m.WorkerActiveLabel)
	}
	if !m.WorkerModes[3].Selected {
		t.Errorf("peer-only row should be Selected: %+v", m.WorkerModes)
	}
	if m.WorkerModes[3].Label != "Peer only" {
		t.Errorf("peer-only row label = %q, want 'Peer only'", m.WorkerModes[3].Label)
	}
	if m.WorkerModes[3].Mode != state.RoutingModePeerOnly {
		t.Errorf("peer-only row Mode = %q, want %q", m.WorkerModes[3].Mode, state.RoutingModePeerOnly)
	}
	for i, r := range m.WorkerModes[:3] {
		if r.Selected {
			t.Errorf("row %d (%s) must not be selected in peer-only mode", i, r.Label)
		}
	}
}

// TestApplyWorkerModeSlotsMatchPreallocation is the guard the
// workerModeSlots comment promises: the renderer only owns that many
// menu items, so a fifth mode row appended to applyWorker would be
// invisible and unclickable rather than obviously broken.
func TestApplyWorkerModeSlotsMatchPreallocation(t *testing.T) {
	snap := baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, nil)
	m := Update(snap)
	if len(m.WorkerModes) != workerModeSlots {
		t.Fatalf("applyWorker emits %d mode rows but the tray pre-allocates %d slots",
			len(m.WorkerModes), workerModeSlots)
	}
}

// TestApplyWorker_RoutingMenuHeaders pins the two disabled section
// labels (#327). They are the whole separation between "Waired picks
// for you" and "always this one computer" — the systray Windows backend
// renders no third nesting level, so the pins cannot move one submenu
// deeper as the review first suggested.
func TestApplyWorker_RoutingMenuHeaders(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:   "dev_lin",
			DeviceName: "linux-gpu",
			InferenceState: &signer.InferenceState{
				Type: signer.InferenceTypeOllama, Reachable: true, Models: []string{"qwen3:8b"},
			},
		}},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if m.WorkerParentLabel != "Inference routing" {
		t.Errorf("parent label = %q, want 'Inference routing'", m.WorkerParentLabel)
	}
	if m.WorkerModesHeader != "Choose automatically" {
		t.Errorf("modes header = %q", m.WorkerModesHeader)
	}
	if m.WorkerPinsHeader != "Pin to one peer" {
		t.Errorf("pins header = %q", m.WorkerPinsHeader)
	}
	if !m.ShowRoutingMenu {
		t.Error("ShowRoutingMenu must be true when the worker rows are present")
	}
}

// TestApplyWorker_PinsHeaderHiddenWithoutPins: a header labels the group
// under it, so with no inference-capable peer there is nothing to label.
func TestApplyWorker_PinsHeaderHiddenWithoutPins(t *testing.T) {
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, nil))
	if m.WorkerPinsHeader != "" {
		t.Errorf("pins header = %q, want empty when no peer can be pinned", m.WorkerPinsHeader)
	}
	if m.WorkerModesHeader == "" {
		t.Error("modes header must still show — the mode rows are always there")
	}
}

// TestRoutingMenuShownForMeshOnlyDaemon: the routing parent also hosts
// the mesh-reachable row, so a daemon that exposes the mesh endpoint but
// not the worker API must still get a parent — otherwise that row has
// nowhere to render. Records today's contract for old daemons.
func TestRoutingMenuShownForMeshOnlyDaemon(t *testing.T) {
	snap := baseSnapshotWithWorker(nil, &inferencemesh.Snapshot{})
	m := Update(snap)
	if m.ShowWorker {
		t.Fatal("ShowWorker must stay false without a WorkerResponse")
	}
	if m.MeshReachableLabel == "" {
		t.Fatal("mesh row should be populated from a non-nil mesh snapshot")
	}
	if !m.ShowRoutingMenu {
		t.Error("ShowRoutingMenu must be true when only the mesh row has content")
	}
}

// TestRoutingMenuHiddenForPreFeatureDaemon: neither endpoint present ⇒
// no parent at all, so a daemon predating both renders the old menu.
func TestRoutingMenuHiddenForPreFeatureDaemon(t *testing.T) {
	m := Update(baseSnapshotWithWorker(nil, nil))
	if m.ShowRoutingMenu {
		t.Error("ShowRoutingMenu must stay false when neither worker nor mesh is present")
	}
}
