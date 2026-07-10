package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func enrolledIdentity() *management.IdentityView {
	return &management.IdentityView{
		Enrolled:     true,
		AccountEmail: "alice@example.com",
		DeviceName:   "alice-laptop",
		OverlayIP:    "100.96.0.10",
	}
}

func TestUpdate_PeerHardware_HiddenWhenNoPeers(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: 0,
		},
	})
	if got.ShowPeerHardware {
		t.Errorf("submenu surfaced with no peers; PeerHardwareEntries=%+v", got.PeerHardwareEntries)
	}
}

func TestUpdate_PeerHardware_HiddenWhenAllPeersHardwareNil(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: 2,
			Peers: []management.PeerStatus{
				{DeviceID: "dev_a", DeviceName: "alice"},
				{DeviceID: "dev_b", DeviceName: "bob"},
			},
		},
	})
	if got.ShowPeerHardware {
		t.Errorf("submenu surfaced despite all peers Hardware-less; rows=%+v", got.PeerHardwareEntries)
	}
}

func TestUpdate_PeerHardware_SinglePeerWithGPU(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: 1,
			Peers: []management.PeerStatus{
				{
					DeviceID:   "dev_b",
					DeviceName: "bob-desktop",
					Hardware: &management.PeerHardware{
						GPUModel:    "NVIDIA GeForce RTX 4090",
						VRAMTotalMB: 24576,
					},
				},
			},
		},
	})
	if !got.ShowPeerHardware {
		t.Fatalf("submenu hidden despite peer having Hardware")
	}
	if got.PeerHardwareParent != "Peers (1)" {
		t.Errorf("parent label = %q, want %q", got.PeerHardwareParent, "Peers (1)")
	}
	if len(got.PeerHardwareEntries) != 1 {
		t.Fatalf("entries count = %d, want 1", len(got.PeerHardwareEntries))
	}
	want := "bob-desktop — RTX 4090 (24 GB)"
	if got.PeerHardwareEntries[0].Label != want {
		t.Errorf("row label = %q, want %q", got.PeerHardwareEntries[0].Label, want)
	}
}

func TestUpdate_PeerHardware_MixedGPUAndCPUOnlyAndUnknown(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: 3,
			Peers: []management.PeerStatus{
				{
					DeviceID:   "dev_b",
					DeviceName: "bob",
					Hardware: &management.PeerHardware{
						GPUModel:    "NVIDIA GeForce RTX 4070",
						VRAMTotalMB: 12288,
					},
				},
				{
					DeviceID:   "dev_c",
					DeviceName: "carol-server",
					Hardware: &management.PeerHardware{
						RAMTotalGB: 64,
					},
				},
				{
					DeviceID:   "dev_d",
					DeviceName: "dave-old",
					// nil Hardware: predates Phase 7 push but the submenu
					// is still visible because at least one peer has it.
				},
			},
		},
	})
	if !got.ShowPeerHardware {
		t.Fatalf("submenu hidden despite mixed peer hardware")
	}
	wantRows := []string{
		"bob — RTX 4070 (12 GB)",
		"carol-server — CPU only (64 GB RAM)",
		"dave-old — (hardware unknown)",
	}
	if len(got.PeerHardwareEntries) != len(wantRows) {
		t.Fatalf("row count = %d, want %d (rows=%+v)",
			len(got.PeerHardwareEntries), len(wantRows), got.PeerHardwareEntries)
	}
	for i, w := range wantRows {
		if got.PeerHardwareEntries[i].Label != w {
			t.Errorf("row %d = %q, want %q", i, got.PeerHardwareEntries[i].Label, w)
		}
	}
}

func TestUpdate_PeerHardware_FallsBackToDeviceIDWhenNoName(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: 1,
			Peers: []management.PeerStatus{
				{
					DeviceID: "dev_anonymous",
					Hardware: &management.PeerHardware{
						GPUModel:    "AMD Radeon RX 7900 XTX",
						VRAMTotalMB: 24576,
					},
				},
			},
		},
	})
	if len(got.PeerHardwareEntries) != 1 {
		t.Fatalf("entries count = %d, want 1", len(got.PeerHardwareEntries))
	}
	want := "dev_anonymous — AMD Radeon RX 7900 XTX (24 GB)"
	if got.PeerHardwareEntries[0].Label != want {
		t.Errorf("row label = %q, want %q", got.PeerHardwareEntries[0].Label, want)
	}
}

func TestUpdate_PeerHardware_GPUWithoutVRAMRendersGPUOnly(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: 1,
			Peers: []management.PeerStatus{
				{
					DeviceID:   "dev_b",
					DeviceName: "bob",
					Hardware: &management.PeerHardware{
						GPUModel: "NVIDIA GeForce RTX 4090",
					},
				},
			},
		},
	})
	want := "bob — RTX 4090"
	if got.PeerHardwareEntries[0].Label != want {
		t.Errorf("row label = %q, want %q", got.PeerHardwareEntries[0].Label, want)
	}
}

func TestUpdate_PeerHardware_OverflowCappedAt16(t *testing.T) {
	peers := make([]management.PeerStatus, 0, MaxPeerHardwareRows+3)
	peers = append(peers, management.PeerStatus{
		DeviceID:   "dev_first",
		DeviceName: "first",
		Hardware: &management.PeerHardware{
			GPUModel: "RTX 4090", VRAMTotalMB: 24576,
		},
	})
	// Add 18 more with no Hardware — only the first peer's Hardware
	// is needed to flip ShowPeerHardware.
	for range MaxPeerHardwareRows + 2 {
		peers = append(peers, management.PeerStatus{
			DeviceID: "dev_extra",
		})
	}
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: enrolledIdentity(),
		Status: &management.Status{
			PeerCount: len(peers),
			Peers:     peers,
		},
	})
	if !got.ShowPeerHardware {
		t.Fatalf("submenu hidden in overflow test")
	}
	if len(got.PeerHardwareEntries) != MaxPeerHardwareRows {
		t.Errorf("entries count = %d, want %d (cap)",
			len(got.PeerHardwareEntries), MaxPeerHardwareRows)
	}
	wantOverflow := len(peers) - MaxPeerHardwareRows
	if got.PeerHardwareOverflow != wantOverflow {
		t.Errorf("PeerHardwareOverflow = %d, want %d",
			got.PeerHardwareOverflow, wantOverflow)
	}
}

func TestVRAMMBToGB_RoundsToNearest(t *testing.T) {
	cases := []struct {
		mb int
		gb int
	}{
		{24576, 24}, // RTX 4090 advertised
		{23900, 23}, // RTX 4090 after driver reserve
		{12288, 12}, // RTX 4070 advertised
		{11264, 11}, // RTX 4070 after driver reserve
		{8192, 8},
		{0, 0},
		{511, 0}, // < 0.5 GB → 0
		{512, 1}, // == 0.5 GB → 1
	}
	for _, c := range cases {
		if got := vramMBToGB(c.mb); got != c.gb {
			t.Errorf("vramMBToGB(%d) = %d, want %d", c.mb, got, c.gb)
		}
	}
}

func TestShortGPUModel_StripsNvidiaPrefix(t *testing.T) {
	cases := map[string]string{
		"NVIDIA GeForce RTX 4090": "RTX 4090",
		"NVIDIA GeForce RTX 4070": "RTX 4070",
		"AMD Radeon RX 7900 XTX":  "AMD Radeon RX 7900 XTX",
		"Apple M3 Max":            "Apple M3 Max",
		"":                        "",
	}
	for in, want := range cases {
		if got := shortGPUModel(in); got != want {
			t.Errorf("shortGPUModel(%q) = %q, want %q", in, got, want)
		}
	}
}
