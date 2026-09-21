package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The peer rows inside "This device" report what each of your other computers
// is running, which is what the docs have always said they do. They read the
// inference mesh; the hardware rendering they replaced could not tell a
// computer serving a 35B model from one with inference switched off
// (waired-agent#1032).

func TestFormatPeerRowLabel(t *testing.T) {
	tests := []struct {
		name string
		peer inferencemesh.PeerView
		want string
	}{
		{
			name: "serving names the model",
			peer: inferencemesh.PeerView{
				DeviceID: "dev_b", DeviceName: "strix-halo-win",
				InferenceState: &signer.InferenceState{
					Reachable: true, Models: []string{"qwen3.6:35b-a3b-q4_K_M"}, ActiveModel: "qwen3.6-35b-a3b",
				},
			},
			want: "● strix-halo-win — qwen3.6-35b-a3b",
		},
		{
			// ActiveModel is the catalog id every host agrees on; the
			// engine tag spells the same weights differently per engine,
			// so it is only the fallback (inferencemesh.PeerModel).
			name: "the engine tag is the fallback for a model name",
			peer: inferencemesh.PeerView{
				DeviceID: "dev_b", DeviceName: "strix-halo-win",
				InferenceState: &signer.InferenceState{Reachable: true, Models: []string{"qwen3.6:35b-a3b-q4_K_M"}},
			},
			want: "● strix-halo-win — qwen3.6:35b-a3b-q4_K_M",
		},
		{
			name: "a peer that never reported an engine says so",
			peer: inferencemesh.PeerView{DeviceID: "dev_c", DeviceName: "rtx4070-laptop"},
			want: "○ rtx4070-laptop — no engine",
		},
		{
			name: "a peer's own reason beats the viewer's coarser reading",
			peer: inferencemesh.PeerView{
				DeviceID: "dev_c", DeviceName: "rtx4070-laptop",
				InferenceState: &signer.InferenceState{SubsystemState: signer.SubsystemStatePullFailed},
			},
			want: "○ rtx4070-laptop — pull failed",
		},
		{
			// A stale peer's last-known model is a claim about the past;
			// rendering it beside a live-looking row would state it as
			// the present (inferencemesh.ConditionHasFreshModel).
			name: "a stale peer's model is withheld",
			peer: inferencemesh.PeerView{
				DeviceID: "dev_b", DeviceName: "strix-halo-win", Stale: true,
				InferenceState: &signer.InferenceState{
					Reachable: true, Models: []string{"qwen3.6:35b"}, ActiveModel: "qwen3.6-35b-a3b",
				},
			},
			want: "○ strix-halo-win — unavailable",
		},
		{
			// Public share spec §8.5: a stranger's device identifier must
			// never reach a menu row. The grant pseudonym is the only
			// name this peer has here.
			name: "a public-share peer is named by its pseudonym",
			peer: inferencemesh.PeerView{
				DeviceID: "dev_secret", DeviceName: "someones-laptop",
				Grant:          &signer.PeerGrant{ID: "grant-1", Pseudonym: "guest-7"},
				InferenceState: &signer.InferenceState{Reachable: true, Models: []string{"qwen3.6:35b"}},
			},
			want: "● guest-7 — qwen3.6:35b",
		},
		{
			// No pseudonym to show: naming it any other way would be the
			// leak itself, so the row takes the public-machine phrase.
			name: "a grant peer with no pseudonym is never named by its device id",
			peer: inferencemesh.PeerView{
				DeviceID: "dev_secret", DeviceName: "someones-laptop",
				Grant:          &signer.PeerGrant{ID: "grant-1"},
				InferenceState: &signer.InferenceState{Reachable: true, Models: []string{"qwen3.6:35b"}},
			},
			want: "● public machine (grant 00d1) — qwen3.6:35b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPeerRowLabel(tc.peer)
			if got != tc.want {
				t.Errorf("formatPeerRowLabel = %q, want %q", got, tc.want)
			}
			// The §8.5 half of the two grant cases, stated as itself so
			// it survives a change to how the pseudonym is rendered.
			if tc.peer.Grant != nil {
				for _, secret := range []string{tc.peer.DeviceID, tc.peer.DeviceName} {
					if secret != "" && strings.Contains(got, secret) {
						t.Errorf("row %q leaks a grant peer's own identifier %q", got, secret)
					}
				}
			}
		})
	}
}

func TestUpdate_PeerRows_FromMesh(t *testing.T) {
	snap := statusSnapshot()
	snap.Mesh = &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		{DeviceID: "dev_b", DeviceName: "strix-halo-win", InferenceState: &signer.InferenceState{
			Reachable: true, Models: []string{"q"}, ActiveModel: "qwen3.6-35b-a3b",
		}},
		{DeviceID: "dev_c", DeviceName: "rtx4070-laptop"},
	}}
	got := Update(snap)
	if !got.ShowPeerRows {
		t.Fatal("ShowPeerRows should be true when the mesh has peers")
	}
	if got.PeerRowsParent != "Peers (2)" {
		t.Errorf("PeerRowsParent = %q", got.PeerRowsParent)
	}
	want := []string{"● strix-halo-win — qwen3.6-35b-a3b", "○ rtx4070-laptop — no engine"}
	if len(got.PeerRowEntries) != len(want) {
		t.Fatalf("rows: want %d, got %d (%+v)", len(want), len(got.PeerRowEntries), got.PeerRowEntries)
	}
	for i := range want {
		if got.PeerRowEntries[i].Label != want[i] {
			t.Errorf("row %d = %q, want %q", i, got.PeerRowEntries[i].Label, want[i])
		}
	}
}

// A daemon predating /inference/mesh leaves Snapshot.Mesh nil, and the menu
// against it must say exactly what it always said.
func TestUpdate_PeerRows_FallBackToHardwareOnAnOldDaemon(t *testing.T) {
	snap := statusSnapshot()
	snap.Status = &management.Status{Phase: "active", PeerCount: 1, Peers: []management.PeerStatus{{
		DeviceID: "dev_b", DeviceName: "rtx4090-desktop", DisplayID: "dev_b",
		Hardware: &management.PeerHardware{GPUModel: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24576},
	}}}
	got := Update(snap)
	if !got.ShowPeerRows {
		t.Fatal("ShowPeerRows should be true on the hardware fallback")
	}
	if len(got.PeerRowEntries) != 1 || got.PeerRowEntries[0].Label != "rtx4090-desktop — RTX 4090 (24 GB)" {
		t.Errorf("hardware fallback row = %+v", got.PeerRowEntries)
	}
}

// A mesh that reports no peers renders no rows — and does not fall through to
// the hardware path, which would render a different set from a different
// lens on the same machine.
func TestUpdate_PeerRows_EmptyMeshDoesNotFallBack(t *testing.T) {
	snap := statusSnapshot()
	snap.Status = &management.Status{Phase: "active", PeerCount: 1, Peers: []management.PeerStatus{{
		DeviceID: "dev_b", DeviceName: "rtx4090-desktop",
		Hardware: &management.PeerHardware{GPUModel: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24576},
	}}}
	snap.Mesh = &inferencemesh.Snapshot{}
	got := Update(snap)
	if got.ShowPeerRows || len(got.PeerRowEntries) != 0 {
		t.Errorf("an empty mesh should render no peer rows: %+v", got.PeerRowEntries)
	}
}
