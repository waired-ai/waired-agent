//go:build linux

package hardware

import (
	"os"
	"path/filepath"
	"testing"
)

// A real /proc/driver/nvidia/gpus/<bus-id>/information, tab-padded the
// way the kernel module writes it.
const procNvidiaInformation = `Model: 			 NVIDIA GeForce RTX 3060 Ti
IRQ:   			 74
GPU UUID: 		 GPU-16d2f1c0-1234-5678-9abc-def012345678
Video BIOS: 		 94.06.25.00.6c
Bus Type: 		 PCIe
DMA Size: 		 47 bits
DMA Mask: 		 0x7fffffffffff
Bus Location: 		 0000:01:00.0
Device Minor: 		 0
`

func TestParseNvidiaProcInformation(t *testing.T) {
	got := parseNvidiaProcInformation(procNvidiaInformation)
	want := GPU{
		Vendor: "nvidia",
		Model:  "NVIDIA GeForce RTX 3060 Ti",
		UUID:   "GPU-16d2f1c0-1234-5678-9abc-def012345678",
	}
	if got != want {
		t.Errorf("parseNvidiaProcInformation = %+v, want %+v", got, want)
	}
	// Pinned as a record of the file's shape, not as a wish: the kernel
	// publishes no VRAM figure here, which is why the caller warns that
	// the budget is unknown rather than reporting a 0 GB card.
	if got.VRAMTotalMB != 0 {
		t.Errorf("VRAMTotalMB = %d, want 0 (procfs carries no capacity)", got.VRAMTotalMB)
	}
}

func TestNvidiaProcDevices(t *testing.T) {
	root := t.TempDir()
	for _, bus := range []string{"0000:01:00.0", "0000:02:00.0"} {
		dir := filepath.Join(root, bus)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "information"), []byte(procNvidiaInformation), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// A directory with no `information` file must be skipped, not fail
	// the whole walk.
	if err := os.MkdirAll(filepath.Join(root, "0000:03:00.0"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := nvidiaProcDevices(root)
	if len(got) != 2 {
		t.Fatalf("nvidiaProcDevices = %+v, want 2 devices", got)
	}
	for i, g := range got {
		if g.Vendor != "nvidia" || g.Model == "" {
			t.Errorf("device[%d] = %+v, want a vendor-tagged, named device", i, g)
		}
	}

	if got := nvidiaProcDevices(filepath.Join(root, "does-not-exist")); got != nil {
		t.Errorf("nvidiaProcDevices(missing root) = %+v, want nil", got)
	}
}

func TestNvidiaPCIAdapterPresent(t *testing.T) {
	write := func(t *testing.T, dir, vendor, class string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
			t.Fatalf("write vendor: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "class"), []byte(class+"\n"), 0o644); err != nil {
			t.Fatalf("write class: %v", err)
		}
	}

	t.Run("VGA controller counts", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "0000:01:00.0"), "0x10de", "0x030000")
		if !nvidiaPCIAdapterPresent(root) {
			t.Error("nvidiaPCIAdapterPresent = false, want true")
		}
	})

	t.Run("headless 3D controller counts", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "0000:01:00.0"), "0x10de", "0x030200")
		if !nvidiaPCIAdapterPresent(root) {
			t.Error("nvidiaPCIAdapterPresent = false, want true")
		}
	})

	t.Run("an NVIDIA audio function alone does not", func(t *testing.T) {
		root := t.TempDir()
		// Every NVIDIA card exposes an HDMI audio function (class 0x0403)
		// on a neighbouring PCI slot; matching it would report a GPU on a
		// host whose card was removed but whose audio device lingers.
		write(t, filepath.Join(root, "0000:01:00.1"), "0x10de", "0x040300")
		if nvidiaPCIAdapterPresent(root) {
			t.Error("nvidiaPCIAdapterPresent = true for an audio function")
		}
	})

	t.Run("another vendor's display adapter does not", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "0000:01:00.0"), "0x1002", "0x030000")
		if nvidiaPCIAdapterPresent(root) {
			t.Error("nvidiaPCIAdapterPresent = true for an AMD adapter")
		}
	})

	t.Run("missing sysfs is not an adapter", func(t *testing.T) {
		if nvidiaPCIAdapterPresent(filepath.Join(t.TempDir(), "absent")) {
			t.Error("nvidiaPCIAdapterPresent = true with no sysfs")
		}
	})
}

// The real host's inventories must not panic or misreport, whatever this
// machine is. Contract: a returned device is vendor-tagged, and claiming
// devices implies claiming an adapter.
func TestNvidiaFallback_RealHostNoCrash(t *testing.T) {
	got := nvidiaFallback(t.Context())
	for i, g := range got.GPUs {
		if g.Vendor != "nvidia" {
			t.Errorf("GPUs[%d].Vendor = %q, want nvidia", i, g.Vendor)
		}
	}
	if len(got.GPUs) > 0 && !got.AdapterSeen {
		t.Error("devices reported without AdapterSeen")
	}
	if (len(got.GPUs) > 0 || got.AdapterSeen) && got.Source == "" {
		t.Error("a positive result carries no Source for the diagnostic")
	}
}
