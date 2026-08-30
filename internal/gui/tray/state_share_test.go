package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// waired#1297. The app shows and sets one sharing answer — whether this
// computer lends itself out at all — and reports what the console has it
// shared with. These pin the owner ruling recorded on that issue.

func sharingSnapshot(sh *management.ShareStateResponse) Snapshot {
	return Snapshot{
		Health:    HealthOnline,
		Identity:  &management.IdentityView{Enrolled: true, AccountEmail: "a@b"},
		Status:    &management.Status{Phase: "active"},
		Inference: &management.InferenceStatus{SubsystemState: "ready", DesiredState: "enabled"},
		Sharing:   sh,
	}
}

func TestUpdate_Sharing_On(t *testing.T) {
	got := Update(sharingSnapshot(&management.ShareStateResponse{
		State:        string(state.SharingOn),
		DesiredState: string(state.SharingOn),
		MeshShare:    string(state.MeshShareOn),
	}))
	if got.ShareToggleAction != "Stop sharing this computer" {
		t.Errorf("ShareToggleAction=%q, want Stop sharing this computer", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "Sharing: enabled" {
		t.Errorf("ShareStateLabel=%q, want Sharing: enabled", got.ShareStateLabel)
	}
}

func TestUpdate_Sharing_Off(t *testing.T) {
	got := Update(sharingSnapshot(&management.ShareStateResponse{
		State:        string(state.SharingOff),
		DesiredState: string(state.SharingOff),
		MeshShare:    string(state.MeshShareOn),
	}))
	if got.ShareToggleAction != "Share this computer" {
		t.Errorf("ShareToggleAction=%q, want Share this computer", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "Sharing: disabled" {
		t.Errorf("ShareStateLabel=%q, want Sharing: disabled", got.ShareStateLabel)
	}
}

// The state line reports the OUTCOME, not the switch. A computer that is
// lending itself out but has been taken out of every distribution serves
// nobody, and "enabled" there would describe the switch — sending its
// owner looking for a fault on the machine instead of at the console.
func TestUpdate_Sharing_OnButOfferedToNobody(t *testing.T) {
	got := Update(sharingSnapshot(&management.ShareStateResponse{
		State:       string(state.SharingOn),
		MeshShare:   string(state.MeshShareOff),
		PublicShare: string(state.SharingOff),
	}))
	if got.ShareStateLabel != "Sharing: nobody, set in the console" {
		t.Errorf("ShareStateLabel=%q", got.ShareStateLabel)
	}
	// The switch still offers to stop: it is the operator's, and it is
	// what a console setting cannot reach.
	if got.ShareToggleAction != "Stop sharing this computer" {
		t.Errorf("ShareToggleAction=%q", got.ShareToggleAction)
	}
}

// Out of the mesh but still public is not "nobody" — the computer is
// serving guests, and saying otherwise would be a claim about live work.
func TestUpdate_Sharing_OutOfMeshButPublic(t *testing.T) {
	got := Update(sharingSnapshot(&management.ShareStateResponse{
		State:       string(state.SharingOn),
		MeshShare:   string(state.MeshShareOff),
		PublicShare: string(state.SharingOn),
	}))
	if got.ShareStateLabel != "Sharing: enabled" {
		t.Errorf("ShareStateLabel=%q, want Sharing: enabled", got.ShareStateLabel)
	}
}

// Daemon predates the route: the snapshot field stays nil and the row
// stays hidden, so the menu does not bait clicks on an endpoint that
// does not exist.
func TestUpdate_SharingHiddenWhenDaemonDoesntSupportIt(t *testing.T) {
	got := Update(sharingSnapshot(nil))
	if got.ShareToggleAction != "" || got.ShareStateLabel != "" {
		t.Errorf("expected the sharing row hidden, got %q / %q", got.ShareToggleAction, got.ShareStateLabel)
	}
	// The engine rows are unaffected — they answer a different question.
	if got.InferenceToggleAction == "" {
		t.Error("the inference toggle disappeared with the sharing row")
	}
}

// The session latch (#316): the app suspends on Quit and lifts it on its
// next start, so this is normally invisible. Seeing it means the lift did
// not land, and the row must offer the action that clears it rather than
// one that would appear to do nothing.
func TestUpdate_SharingSuspended(t *testing.T) {
	got := Update(sharingSnapshot(&management.ShareStateResponse{
		State:        string(state.SharingOff),
		DesiredState: string(state.SharingOn),
		Suspended:    true,
	}))
	if got.ShareToggleAction != "Share this computer" {
		t.Errorf("ShareToggleAction=%q, want Share this computer", got.ShareToggleAction)
	}
	if got.ShareStateLabel != "Sharing: paused" {
		t.Errorf("ShareStateLabel=%q, want Sharing: paused", got.ShareStateLabel)
	}
}

// Sharing is a decision about this computer, not about its engine: a
// machine whose engine is soft-disabled can still be lending itself out,
// and a machine with no engine at all can still be told to stop. The row
// used to disappear with the engine because it lived inside the
// inference projection.
func TestUpdate_SharingVisibleWithoutAnEngine(t *testing.T) {
	snap := sharingSnapshot(&management.ShareStateResponse{
		State:     string(state.SharingOn),
		MeshShare: string(state.MeshShareOn),
	})
	snap.Inference = &management.InferenceStatus{SubsystemState: "no_engine", DesiredState: "enabled"}
	got := Update(snap)
	if got.ShareToggleAction != "Stop sharing this computer" {
		t.Errorf("ShareToggleAction=%q on a computer with no engine", got.ShareToggleAction)
	}
}

// Only on Connected / Disconnected, the same gate the inference group
// has: the operator should not reach a sharing decision while the
// network state itself is unknown.
func TestUpdate_SharingHiddenWhenNotConnectedOrDisconnected(t *testing.T) {
	snap := sharingSnapshot(&management.ShareStateResponse{
		State:     string(state.SharingOn),
		MeshShare: string(state.MeshShareOn),
	})
	snap.Identity = nil
	got := Update(snap)
	if got.ShareToggleAction != "" || got.ShareStateLabel != "" {
		t.Errorf("sharing rendered while signed out: %q / %q", got.ShareToggleAction, got.ShareStateLabel)
	}
}
