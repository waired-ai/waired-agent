package tray

import (
	"strings"
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

// TestWorkerSummaryLabel_DownPinStatesTheConsequence pins the PRODUCT
// CONTRACT from waired-agent#325: the root row must say what a down pin
// MEANS, not just that it is down. The old " (unavailable)" suffix was
// technically true and actively misleading — the operator saw it while this
// machine's GPU quietly answered every Claude turn. "not served here" is the
// phrasing that holds on every surface: general inference 503s, and a Claude
// turn on the auto route leaves for the Anthropic API. Neither runs on the
// pinned worker.
func TestWorkerSummaryLabel_DownPinStatesTheConsequence(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"ok", "linux-gpu (pinned)"},
		{"unavailable", "linux-gpu (pinned) — unavailable, requests are not served here"},
		{"absent", "linux-gpu (pinned) — absent, requests are not served here"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			got := workerSummaryLabel(management.WorkerResponse{
				Mode:               state.RoutingModePinned,
				PinnedPeerDeviceID: "dev_lin",
				PinnedPeerName:     "linux-gpu",
				PinnedPeerStatus:   tc.status,
			})
			if got != tc.want {
				t.Errorf("workerSummaryLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// A pin with no friendly name still has to render — the device id is all the
// tray has.
func TestWorkerSummaryLabel_FallsBackToDeviceID(t *testing.T) {
	got := workerSummaryLabel(management.WorkerResponse{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: "dev_lin",
		PinnedPeerStatus:   "unavailable",
	})
	if !strings.HasPrefix(got, "dev_lin (pinned)") {
		t.Errorf("workerSummaryLabel = %q, want it to start with the device id", got)
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

// TestApplyWorker_SilentPeerStaysSelectable pins the display half of
// waired#729. PRODUCT CONTRACT.
//
// docs-site defines "(unavailable)" as "your pin survives, it just
// cannot serve until that machine ..." — so the label may only appear
// when the peer genuinely cannot serve. A disco-silent peer CAN: the
// Selector routes to it and the /healthz probe decides. Marking it
// unavailable would tell the user their working machine is unusable
// and grey out the row that works, which is the exact complaint #729
// opened with, moved up into the UI.
// waired#1064: the pin row names the model in the catalog's namespace,
// so the same model reads the same on every row regardless of the peer's
// engine — and therefore its OS. PRODUCT CONTRACT.
func TestApplyWorker_PinRowNamesTheCatalogModel(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:   "dev_mac",
			DeviceName: "mac-studio",
			InferenceState: &signer.InferenceState{
				Reachable:      true,
				Type:           signer.InferenceTypeOllama,
				Models:         []string{"hf.co/unsloth/Qwen3-Coder-Next-GGUF:Q4_K_M"},
				ActiveModel:    "qwen3-coder-next-80b-a3b-instruct",
				SubsystemState: signer.SubsystemStateReady,
			},
		}},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("got %d entries", len(m.WorkerPinEntries))
	}
	if got := m.WorkerPinEntries[0].Label; got != "mac-studio (qwen3-coder-next-80b-a3b-instruct)" {
		t.Errorf("pin label = %q", got)
	}
}

// The case waired#1064 exists for: narrowPublishedModels withdrew the
// engine tag because the model is still downloading, so before this the
// row could only say "(unavailable)" — the same thing it says for a dead
// engine. PRODUCT CONTRACT.
func TestApplyWorker_PinRowSaysWhyAPeerIsNotServing(t *testing.T) {
	row := func(subState string) string {
		t.Helper()
		mesh := &inferencemesh.Snapshot{
			Peers: []inferencemesh.PeerView{{
				DeviceID:   "dev_busy",
				DeviceName: "win-desktop",
				InferenceState: &signer.InferenceState{
					Reachable:      true,
					Type:           signer.InferenceTypeOllama,
					ActiveModel:    "qwen3-8b-instruct",
					SubsystemState: subState,
				},
			}},
		}
		m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
		if len(m.WorkerPinEntries) != 1 {
			t.Fatalf("got %d entries", len(m.WorkerPinEntries))
		}
		if m.WorkerPinEntries[0].Available {
			t.Error("a peer with no advertised tag must not be Available")
		}
		return m.WorkerPinEntries[0].Label
	}
	if got := row(signer.SubsystemStateLoading); got != "win-desktop (qwen3-8b-instruct — downloading)" {
		t.Errorf("downloading row = %q", got)
	}
	if got := row(signer.SubsystemStatePullFailed); got != "win-desktop (qwen3-8b-instruct — pull failed)" {
		t.Errorf("failed-download row = %q", got)
	}
	if got := row(signer.SubsystemStateEngineFailed); got != "win-desktop (qwen3-8b-instruct — engine failed)" {
		t.Errorf("dead-engine row = %q", got)
	}
}

// A peer running an agent that predates the fields renders exactly as it
// did before: the engine tag, and the published "(unavailable)" for every
// reason it cannot give. PRODUCT CONTRACT — a mixed fleet must not
// regress while it upgrades.
func TestApplyWorker_PinRowUnchangedForAnOlderPeer(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID:   "dev_old_ok",
				DeviceName: "old-serving",
				InferenceState: &signer.InferenceState{
					Reachable: true,
					Type:      signer.InferenceTypeOllama,
					Models:    []string{"qwen3:8b-q4_K_M"},
				},
			},
			{
				DeviceID:   "dev_old_down",
				DeviceName: "old-down",
				InferenceState: &signer.InferenceState{
					Reachable: false,
					Type:      signer.InferenceTypeOllama,
				},
			},
		},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if len(m.WorkerPinEntries) != 2 {
		t.Fatalf("got %d entries", len(m.WorkerPinEntries))
	}
	if got := m.WorkerPinEntries[0].Label; got != "old-serving (qwen3:8b-q4_K_M)" {
		t.Errorf("serving row = %q", got)
	}
	if got := m.WorkerPinEntries[1].Label; got != "old-down (unavailable)" {
		t.Errorf("down row = %q", got)
	}
}

// waired#1064: the summary row answers the same question the pin rows do.
func TestWorkerSummaryLabel_NamesTheModelAndTheReason(t *testing.T) {
	ok := workerSummaryLabel(management.WorkerResponse{
		Mode:             state.RoutingModePinned,
		PinnedPeerName:   "linux-gpu",
		PinnedPeerStatus: "ok",
		PinnedPeerModel:  "qwen3-8b-instruct",
	})
	if ok != "linux-gpu (pinned) — qwen3-8b-instruct" {
		t.Errorf("ok summary = %q", ok)
	}
	down := workerSummaryLabel(management.WorkerResponse{
		Mode:                state.RoutingModePinned,
		PinnedPeerName:      "linux-gpu",
		PinnedPeerStatus:    "unavailable",
		PinnedPeerModel:     "qwen3-8b-instruct",
		PinnedPeerCondition: signer.SubsystemStateLoading,
	})
	// waired-agent#325's consequence clause survives verbatim; only the
	// reason in front of it got specific.
	if down != "linux-gpu (pinned) — downloading, requests are not served here" {
		t.Errorf("down summary = %q", down)
	}
	// A peer that gave no reason keeps the published wording exactly.
	older := workerSummaryLabel(management.WorkerResponse{
		Mode:             state.RoutingModePinned,
		PinnedPeerName:   "linux-gpu",
		PinnedPeerStatus: "unavailable",
	})
	if older != "linux-gpu (pinned) — unavailable, requests are not served here" {
		t.Errorf("older-peer summary = %q", older)
	}
}

func TestApplyWorker_SilentPeerStaysSelectable(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:   "dev_silent",
			DeviceName: "peer-silent",
			Silent:     true,
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Models:    []string{"qwen3:8b"},
			},
		}},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("silent peer should appear, got %d entries", len(m.WorkerPinEntries))
	}
	pe := m.WorkerPinEntries[0]
	if !pe.Available {
		t.Errorf("silent peer must stay Available — routing will use it: %+v", pe)
	}
	if strings.Contains(pe.Label, "unavailable") {
		t.Errorf("silent label = %q, must not carry the (unavailable) suffix", pe.Label)
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

// The public-share fixtures. Synthetic and unmistakable: this repository
// is public, and a leak has to show up as a substring hit.
const (
	trayForeignDeviceID = "dev_foreign00000001"
	trayForeignAlias    = "guest-a7f3"
)

// PRODUCT CONTRACT (waired-agent#739 + public share spec §8.5). A menu
// row is a surface a public machine's real device id may not reach. The
// pin rows come from the mesh snapshot, so the grant is in hand here.
func TestApplyWorker_PinRowNamesAPublicMachineByItsPseudonym(t *testing.T) {
	mesh := &inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			// No DeviceName: the fallback is what used to render the real id.
			DeviceID: trayForeignDeviceID,
			Grant:    &signer.PeerGrant{Kind: "public", Role: "provider", Pseudonym: trayForeignAlias},
			InferenceState: &signer.InferenceState{
				Reachable: true, Type: signer.InferenceTypeOllama, Models: []string{"qwen3:8b"},
			},
		}},
	}
	m := Update(baseSnapshotWithWorker(&management.WorkerResponse{Mode: state.RoutingModeAuto}, mesh))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("pin entries = %d, want 1", len(m.WorkerPinEntries))
	}
	pe := m.WorkerPinEntries[0]
	if strings.Contains(pe.Label, trayForeignDeviceID) {
		t.Errorf("the public machine's real device id reached the menu: %q", pe.Label)
	}
	if !strings.Contains(pe.Label, trayForeignAlias) {
		t.Errorf("label = %q, want it to name the pseudonym", pe.Label)
	}
	// The row's DeviceID stays the real one: the tray posts it back to
	// set the pin and matches it against the snapshot to mark the
	// selection. Display and key are different answers.
	if pe.DeviceID != trayForeignDeviceID {
		t.Errorf("row DeviceID = %q, want the real id — it is the pin the daemon stores", pe.DeviceID)
	}
}

// The absent row is the one that fires most often: resolvePinStatus
// returns no name once the peer leaves the snapshot, and the label used
// to fall back to the raw pin.
func TestApplyWorker_AbsentPinRowNamesAPublicMachineByItsPseudonym(t *testing.T) {
	w := &management.WorkerResponse{
		Mode:                state.RoutingModePinned,
		PinnedPeerDeviceID:  trayForeignDeviceID,
		PinnedPeerDisplayID: trayForeignAlias,
		PinnedPeerStatus:    "absent",
	}
	m := Update(baseSnapshotWithWorker(w, &inferencemesh.Snapshot{}))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("pin entries = %d, want the absent row", len(m.WorkerPinEntries))
	}
	if got := m.WorkerPinEntries[0].Label; got != trayForeignAlias+" (absent)" {
		t.Errorf("absent label = %q, want %q", got, trayForeignAlias+" (absent)")
	}
	if strings.Contains(m.WorkerActiveLabel, trayForeignDeviceID) {
		t.Errorf("the summary row leaks the real device id: %q", m.WorkerActiveLabel)
	}
	if !strings.Contains(m.WorkerActiveLabel, trayForeignAlias) {
		t.Errorf("summary row = %q, want it to name the pseudonym", m.WorkerActiveLabel)
	}
}

// Own-network rows read exactly as they did before: the display
// identifier a daemon reports for one of your own machines IS its
// DeviceID, so nothing in the menu changes.
func TestApplyWorker_OwnPinRowsUnchanged(t *testing.T) {
	w := &management.WorkerResponse{
		Mode:                state.RoutingModePinned,
		PinnedPeerDeviceID:  "dev_gone",
		PinnedPeerDisplayID: "dev_gone",
		PinnedPeerName:      "ghost",
		PinnedPeerStatus:    "absent",
	}
	m := Update(baseSnapshotWithWorker(w, &inferencemesh.Snapshot{}))
	if len(m.WorkerPinEntries) != 1 {
		t.Fatalf("pin entries = %d, want the absent row", len(m.WorkerPinEntries))
	}
	if got := m.WorkerPinEntries[0].Label; got != "ghost (absent)" {
		t.Errorf("absent label = %q, want %q", got, "ghost (absent)")
	}
}
