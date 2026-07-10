//go:build !darwin

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
	if len(m.WorkerModes) != 3 {
		t.Fatalf("want 3 mode rows, got %d", len(m.WorkerModes))
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
