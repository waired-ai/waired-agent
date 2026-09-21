//go:build !windows

package hardware

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The sysfs reader against directory trees laid out like /sys.
//
// Not built on Windows, and not because the reader is Linux-only — it is
// untagged and runs everywhere. The FIXTURE cannot exist there: a real
// sysfs tree names devices by PCI address ("0000:10:00.0"), and ':' is not
// a legal character in a Windows path; the links it follows are symlinks,
// which Windows creates only with developer mode or elevation. The
// reader's pure parts (gfxTargetName, the PCI -> gfx table) are tested
// untagged in amd_sysfs_test.go.

// fakeAMDCard lays out what amdgpu and KFD publish for one device under
// root, the way a real /sys links it: /sys/class/drm/cardN/device is a
// symlink into /sys/devices/.../<pci address>, and the driver is a
// symlink named after the driver.
type fakeAMDCard struct {
	card       string // "card1"
	pciAddr    string // "0000:10:00.0"
	device     string // "0x13c0"
	revision   string // "0xc1"
	vramBytes  uint64
	usedBytes  uint64
	gttBytes   uint64
	gc         string // "10.3.6", "" for no IP discovery tree
	kfdNode    string // "1", "" for no KFD
	kfdGFX     int
	kfdPoolB   uint64
	driver     string // "amdgpu"
	productTag string // product_name, "" when absent
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (c fakeAMDCard) lay(t *testing.T, root string) {
	t.Helper()
	devDir := filepath.Join(root, "sys", "devices", "pci0000:00", c.pciAddr)
	writeFile(t, filepath.Join(devDir, "vendor"), "0x1002\n")
	writeFile(t, filepath.Join(devDir, "device"), c.device+"\n")
	writeFile(t, filepath.Join(devDir, "revision"), c.revision+"\n")
	writeFile(t, filepath.Join(devDir, "class"), "0x030000\n")
	writeFile(t, filepath.Join(devDir, "mem_info_vram_total"), strconv.FormatUint(c.vramBytes, 10)+"\n")
	writeFile(t, filepath.Join(devDir, "mem_info_vram_used"), strconv.FormatUint(c.usedBytes, 10)+"\n")
	writeFile(t, filepath.Join(devDir, "mem_info_gtt_total"), strconv.FormatUint(c.gttBytes, 10)+"\n")
	if c.productTag != "" {
		writeFile(t, filepath.Join(devDir, "product_name"), c.productTag+"\n")
	}
	if c.gc != "" {
		var maj, min, rev string
		parts := splitDots(c.gc)
		maj, min, rev = parts[0], parts[1], parts[2]
		gc := filepath.Join(devDir, "ip_discovery", "die", "0", "GC", "0")
		writeFile(t, filepath.Join(gc, "major"), maj+"\n")
		writeFile(t, filepath.Join(gc, "minor"), min+"\n")
		writeFile(t, filepath.Join(gc, "revision"), rev+"\n")
	}
	drvDir := filepath.Join(root, "sys", "bus", "pci", "drivers", c.driver)
	if err := os.MkdirAll(drvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drvDir, filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	cardDir := filepath.Join(root, "sys", "class", "drm", c.card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devDir, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
	if c.kfdNode != "" {
		var bus, dev, fn, domain uint64
		// "0000:10:00.0" -> location_id = bus<<8 | dev<<3 | fn
		d, _ := strconv.ParseUint(c.pciAddr[0:4], 16, 64)
		b, _ := strconv.ParseUint(c.pciAddr[5:7], 16, 64)
		dv, _ := strconv.ParseUint(c.pciAddr[8:10], 16, 64)
		f, _ := strconv.ParseUint(c.pciAddr[11:12], 16, 64)
		domain, bus, dev, fn = d, b, dv, f
		node := filepath.Join(root, "sys", "class", "kfd", "kfd", "topology", "nodes", c.kfdNode)
		writeFile(t, filepath.Join(node, "properties"),
			"cpu_cores_count 0\nsimd_count 4\n"+
				"gfx_target_version "+strconv.Itoa(c.kfdGFX)+"\n"+
				"vendor_id 4098\n"+
				"location_id "+strconv.FormatUint(bus<<8|dev<<3|fn, 10)+"\n"+
				"domain "+strconv.FormatUint(domain, 10)+"\n")
		writeFile(t, filepath.Join(node, "mem_banks", "0", "properties"),
			"heap_type 1\nsize_in_bytes "+strconv.FormatUint(c.kfdPoolB, 10)+"\nflags 0\nwidth 128\nmem_clk_max 1800\n")
	}
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

const mib = 1 << 20

// The Linux fleet host's AMD iGPU, from the values read off it on
// 2026-09-21 (Linux 7.0, a Ryzen 9 9950X): Granite Ridge 1002:13c0,
// GC 10.3.6, KFD gfx_target_version 100306, a 2 GiB carve-out, a GTT of
// 64933629952 bytes that KFD reports as the pool, location_id 4096 at
// 0000:10:00.0. libdrm's amdgpu.ids on that host has no 13C0 entry.
func rtx4000LinuxIGPU() fakeAMDCard {
	return fakeAMDCard{
		card: "card1", pciAddr: "0000:10:00.0", device: "0x13c0", revision: "0xc1",
		vramBytes: 2147483648, usedBytes: 16 * mib, gttBytes: 64933629952,
		gc: "10.3.6", kfdNode: "1", kfdGFX: 100306, kfdPoolB: 64933629952,
		driver: "amdgpu",
	}
}

func TestReadAMDSysfs_TheFleetHostsIGPU(t *testing.T) {
	root := t.TempDir()
	rtx4000LinuxIGPU().lay(t, root)
	// An NVIDIA card beside it must be ignored: vendor 0x10de.
	writeFile(t, filepath.Join(root, "sys", "class", "drm", "card0", "device", "vendor"), "0x10de\n")

	got := readAMDSysfs(root)
	if len(got) != 1 {
		t.Fatalf("readAMDSysfs = %+v, want the one AMD device", got)
	}
	g := got[0]
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"PCIID", g.PCIID, "1002:13c0"},
		{"GFXTarget", g.GFXTarget, "gfx1036"},
		{"VRAMTotalMB", g.VRAMTotalMB, 2048},
		{"VRAMFreeMB", g.VRAMFreeMB, 2032},
		{"GTTTotalMB", g.GTTTotalMB, 61925},
		{"KFDMemMB", g.KFDMemMB, 61925},
		{"Integrated", g.Integrated, true},
		{"IntegratedKnown", g.IntegratedKnown, true},
		{"Model", g.Model, "AMD GPU 1002:13c0"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A discrete card: a GC version the kernel does not mark as an APU says
// nothing about integration (the copy of the list may be older than the
// kernel), and the name comes from libdrm's table.
func TestReadAMDSysfs_ADiscreteCard(t *testing.T) {
	root := t.TempDir()
	fakeAMDCard{
		card: "card0", pciAddr: "0000:03:00.0", device: "0x744c", revision: "0xc8",
		vramBytes: 24 << 30, usedBytes: 1 << 30, gttBytes: 32 << 30,
		gc: "11.0.0", kfdNode: "2", kfdGFX: 110000, kfdPoolB: 24 << 30, driver: "amdgpu",
	}.lay(t, root)
	writeFile(t, filepath.Join(root, "usr", "share", "libdrm", "amdgpu.ids"),
		"# comment\n1.0.0\n744C,\tC8,\tAMD Radeon RX 7900 XTX\n744C,\tCC,\tAMD Radeon RX 7900 XT\n")

	got := readAMDSysfs(root)
	if len(got) != 1 {
		t.Fatalf("readAMDSysfs = %+v", got)
	}
	g := got[0]
	if g.GFXTarget != "gfx1100" || g.Model != "AMD Radeon RX 7900 XTX" || g.VRAMTotalMB != 24576 || g.VRAMFreeMB != 23552 {
		t.Errorf("got %+v", g)
	}
	if g.IntegratedKnown {
		t.Errorf("IntegratedKnown = true for a GC version not in the APU list; absence must say nothing")
	}
}

// Without KFD the target comes from the GC version through the kernel's
// own mapping, which differs from the GC digits where one ISA serves
// several revisions.
func TestReadAMDSysfs_NoKFDFallsBackToTheGCMapping(t *testing.T) {
	root := t.TempDir()
	fakeAMDCard{
		card: "card0", pciAddr: "0000:c1:00.0", device: "0x15bf", revision: "0xc4",
		vramBytes: 512 * mib, gttBytes: 16 << 30, gc: "11.0.1", driver: "amdgpu",
	}.lay(t, root)
	got := readAMDSysfs(root)
	if len(got) != 1 || got[0].GFXTarget != "gfx1103" || !got[0].IntegratedKnown || !got[0].Integrated || got[0].KFDMemMB != 0 {
		t.Fatalf("got %+v, want gfx1103 (GC 11.0.1), integrated, no KFD pool", got)
	}
}

// A device on another driver (the legacy radeon, or vfio for a
// passed-through card) is not something this host's engine can use.
func TestReadAMDSysfs_OtherDriversAreSkipped(t *testing.T) {
	root := t.TempDir()
	c := rtx4000LinuxIGPU()
	c.driver = "vfio-pci"
	c.lay(t, root)
	if got := readAMDSysfs(root); len(got) != 0 {
		t.Fatalf("readAMDSysfs = %+v, want nothing for a vfio-bound device", got)
	}
}

// Nothing at all on a system with no /sys — which is what makes the
// reader safe to call unconditionally on Windows and macOS.
func TestReadAMDSysfs_NoSysfs(t *testing.T) {
	if got := readAMDSysfs(t.TempDir()); got != nil {
		t.Fatalf("readAMDSysfs = %+v, want nil", got)
	}
}
