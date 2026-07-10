//go:build !darwin

package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
)

// meshConnectedSnapshot returns a Snapshot that resolves to a
// Connected/Disconnected menu (enrolled + healthy), with the given mesh.
func meshConnectedSnapshot(mesh *inferencemesh.Snapshot) Snapshot {
	return Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status:   &management.Status{},
		Mesh:     mesh,
	}
}

func TestUpdate_MeshReachable_HiddenWhenMeshNil(t *testing.T) {
	got := Update(meshConnectedSnapshot(nil))
	if got.MeshReachableLabel != "" {
		t.Errorf("MeshReachableLabel = %q, want empty (daemon predates mesh API)", got.MeshReachableLabel)
	}
}

func TestUpdate_MeshReachable_ReachablePeer(t *testing.T) {
	got := Update(meshConnectedSnapshot(&inferencemesh.Snapshot{Reachable: true}))
	if got.MeshReachableLabel != "Mesh: peer engine reachable" {
		t.Errorf("MeshReachableLabel = %q, want reachable label", got.MeshReachableLabel)
	}
}

func TestUpdate_MeshReachable_NoReachablePeer(t *testing.T) {
	// Peers present but none reachable (stale / engine down) → Reachable
	// stays false and the row reports that explicitly.
	got := Update(meshConnectedSnapshot(&inferencemesh.Snapshot{
		Reachable: false,
		Peers:     []inferencemesh.PeerView{{DeviceID: "dev_b", DeviceName: "bob", Stale: true}},
	}))
	if got.MeshReachableLabel != "Mesh: no reachable peer engine" {
		t.Errorf("MeshReachableLabel = %q, want no-reachable label", got.MeshReachableLabel)
	}
}

func TestUpdate_MeshReachable_HiddenWhenNotConnected(t *testing.T) {
	// Daemon down: even with a mesh snapshot present, the indicator stays
	// hidden because the menu never reaches Connected/Disconnected.
	got := Update(Snapshot{Health: HealthOffline, Mesh: &inferencemesh.Snapshot{Reachable: true}})
	if got.MeshReachableLabel != "" {
		t.Errorf("MeshReachableLabel = %q, want empty while disconnected", got.MeshReachableLabel)
	}
}
