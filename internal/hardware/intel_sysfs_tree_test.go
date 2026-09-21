//go:build !windows

package hardware

import (
	"os"
	"path/filepath"
	"testing"
)

// The Intel sysfs reader against a directory tree laid out like /sys.
// Not built on Windows for the reason amd_sysfs_tree_test.go gives: the
// fixture's PCI-address paths and symlinks cannot exist there.

// layIntelCard writes what i915/xe publish for one device under root.
func layIntelCard(t *testing.T, root, card, pciAddr, device, driver string) {
	t.Helper()
	devDir := filepath.Join(root, "sys", "devices", "pci0000:00", pciAddr)
	writeFile(t, filepath.Join(devDir, "vendor"), "0x8086\n")
	writeFile(t, filepath.Join(devDir, "device"), device+"\n")
	writeFile(t, filepath.Join(devDir, "class"), "0x030000\n")
	drvDir := filepath.Join(root, "sys", "bus", "pci", "drivers", driver)
	if err := os.MkdirAll(drvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drvDir, filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	cardDir := filepath.Join(root, "sys", "class", "drm", card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devDir, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
}

// A Lunar Lake laptop and a desktop with a Battlemage card, as sysfs
// shows them. The memory reader is asked only about a card that is not
// known to be integrated.
func TestReadIntelSysfs(t *testing.T) {
	root := t.TempDir()
	layIntelCard(t, root, "card0", "0000:00:02.0", "0x64a0", "xe") // Lunar Lake, Arc 140V
	layIntelCard(t, root, "card1", "0000:03:00.0", "0xe20b", "xe") // Arc B580
	layIntelCard(t, root, "card2", "0000:04:00.0", "0x56a0", "vfio-pci")
	var asked []string
	vram := func(addr, driver string) (int, bool) {
		asked = append(asked, addr+"/"+driver)
		if addr == "0000:03:00.0" {
			return 12288, true
		}
		return 0, false
	}
	got := readIntelSysfs(root, vram)
	if len(got) != 2 {
		t.Fatalf("readIntelSysfs = %+v, want the two xe devices (vfio-bound skipped)", got)
	}
	byID := map[string]GPU{}
	for _, g := range got {
		applyIntelPart(&g)
		byID[g.PCIID] = g
	}
	if g := byID["8086:64a0"]; !g.IntegratedKnown || !g.Integrated || g.VRAMTotalMB != 0 {
		t.Errorf("Lunar Lake = %+v, want known integrated with no memory of its own", g)
	}
	if g := byID["8086:e20b"]; !g.IntegratedKnown || g.Integrated || g.VRAMTotalMB != 12288 {
		t.Errorf("B580 = %+v, want known discrete with 12288 MB", g)
	}
	if len(asked) != 1 || asked[0] != "0000:03:00.0/xe" {
		t.Errorf("memory reader asked %v, want only the discrete card", asked)
	}
}
