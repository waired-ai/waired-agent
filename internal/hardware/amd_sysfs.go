package hardware

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Reading AMD GPUs from what the Linux kernel publishes, without ROCm
// (waired-agent#1485).
//
// WHY NOT rocm-smi. It ships with the ROCm SDK, which most machines do
// not have — ollama carries its own HIP runtime — so the profiler used to
// see no AMD GPU at all on a typical Linux host (docs/knowledges/
// 20260805/1610 §1). Everything this reader needs is instead in files the
// amdgpu and KFD drivers create mode 0444, so the daemon, which runs as
// an unprivileged service user, can read them too:
//
//	/sys/class/drm/cardN/device/{vendor,device,revision,class}
//	/sys/class/drm/cardN/device/mem_info_vram_total   the carve-out on an APU
//	/sys/class/drm/cardN/device/mem_info_vram_used
//	/sys/class/drm/cardN/device/mem_info_gtt_total
//	/sys/class/drm/cardN/device/ip_discovery/die/0/GC/0/{major,minor,revision}
//	/sys/class/kfd/kfd/topology/nodes/N/properties    gfx_target_version, location_id
//	/sys/class/kfd/kfd/topology/nodes/N/mem_banks/0/properties  size_in_bytes
//
// (amdgpu_vram_mgr.c and amdgpu_gtt_mgr.c declare the mem_info files
// S_IRUGO; amdgpu_discovery.c the GC files __ATTR_RO; kfd_priv.h sets
// KFD_SYSFS_FILE_MODE to 0444.)
//
// The function takes the filesystem root so a test can hand it a
// directory tree built from a real machine's values. On a system with no
// /sys/class/drm it finds nothing, which is also why it needs no build
// tag: on Windows and macOS the paths simply are not there.

// sysfsRoot is where the sysfs readers look. A variable only so a test of
// a detector can point it at a fake tree.
var sysfsRoot = "/"

// readAMDSysfs lists every GPU bound to the amdgpu driver.
func readAMDSysfs(root string) []GPU {
	cards, _ := filepath.Glob(filepath.Join(root, "sys", "class", "drm", "card[0-9]*"))
	kfd := readKFDNodes(root)
	names := loadAMDGPUIDs(root)
	var out []GPU
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue // card0-DP-1 and friends are connectors
		}
		dev := filepath.Join(card, "device")
		if readTrim(filepath.Join(dev, "vendor")) != "0x1002" {
			continue
		}
		if !strings.HasPrefix(readTrim(filepath.Join(dev, "class")), "0x03") {
			continue // not a display controller
		}
		if drv, err := os.Readlink(filepath.Join(dev, "driver")); err != nil || filepath.Base(drv) != "amdgpu" {
			continue
		}
		deviceID := strings.TrimPrefix(readTrim(filepath.Join(dev, "device")), "0x")
		revision := strings.TrimPrefix(readTrim(filepath.Join(dev, "revision")), "0x")
		g := GPU{
			Vendor: "amd",
			PCIID:  PCIIDFromSysfs("0x1002", "0x"+deviceID),
		}
		total := readUint(filepath.Join(dev, "mem_info_vram_total"))
		g.VRAMTotalMB = int(total / (1 << 20))
		if used := readUint(filepath.Join(dev, "mem_info_vram_used")); total > 0 && used <= total {
			g.VRAMFreeMB = int((total - used) / (1 << 20))
		}
		g.GTTTotalMB = int(readUint(filepath.Join(dev, "mem_info_gtt_total")) / (1 << 20))

		gc, gcOK := readGCVersion(dev)
		addr := pciAddressOf(dev)
		if n, ok := kfd[addr]; ok {
			g.GFXTarget = gfxTargetName(n.gfxTargetVersion)
			g.KFDMemMB = n.memMB
		}
		if g.GFXTarget == "" && gcOK {
			g.GFXTarget = gfxTargetName(gfxTargetVersionForGC[gc])
		}
		// Integration, only ever in the direction the kernel itself
		// decides by: amdgpu marks exactly these GC versions AMD_IS_APU,
		// and that flag is what AMDGPU_IDS_FLAGS_FUSION reports. A GC
		// version NOT in the copy says nothing — the copy may be older
		// than the kernel — so it stays unknown for the render-node
		// reading or the persisted one to settle.
		if gcOK {
			if _, apu := apuGCVersions[gc]; apu {
				g.Integrated, g.IntegratedKnown = true, true
			}
		}
		g.Model = amdModelName(dev, deviceID, revision, names)
		out = append(out, g)
	}
	return out
}

// apuGCVersions are the GC IP versions amdgpu marks AMD_IS_APU, copied
// from amdgpu_discovery.c (Linux v7.0, the switch on GC_HWIP that sets
// adev->flags |= AMD_IS_APU). v6.17 had the same list without 11.5.4.
//
// Absent on purpose: the datacenter APU that shares GC 9.4.3 with a
// discrete part. The kernel tells those apart by package type
// (SMUIO 13.0.3 / 13.0.11), which no sysfs file here exposes.
var apuGCVersions = map[string]struct{}{
	"9.1.0": {}, "9.2.2": {}, "9.3.0": {},
	"10.1.3": {}, "10.1.4": {},
	"10.3.1": {}, "10.3.3": {}, "10.3.6": {}, "10.3.7": {},
	"11.0.1": {}, "11.0.4": {},
	"11.5.0": {}, "11.5.1": {}, "11.5.2": {}, "11.5.3": {}, "11.5.4": {},
}

// gfxTargetVersionForGC maps a GC IP version to the ISA target KFD would
// report for it, copied from kfd_device.c (Linux v7.0,
// kgd2kfd_probe). Used only where KFD itself is absent. The two differ
// where one ISA serves several silicon revisions — GC 11.0.3 is gfx1101,
// and GC 10.3.6 and 10.3.7 are both gfx1036.
var gfxTargetVersionForGC = map[string]int{
	"9.0.1": 90000, "9.1.0": 90002, "9.2.2": 90002, "9.2.1": 90004,
	"9.3.0": 90012, "9.4.0": 90006, "9.4.1": 90008, "9.4.2": 90010,
	"9.4.3": 90402, "9.4.4": 90402, "9.5.0": 90500,
	"10.1.10": 100100, "10.1.2": 100101, "10.1.1": 100102,
	"10.1.3": 100103, "10.1.4": 100103,
	"10.3.0": 100300, "10.3.2": 100301, "10.3.1": 100303, "10.3.4": 100302,
	"10.3.5": 100304, "10.3.3": 100305, "10.3.6": 100306, "10.3.7": 100306,
	"11.0.0": 110000, "11.0.1": 110003, "11.0.4": 110003, "11.0.2": 110002,
	"11.0.3": 110001,
	"11.5.0": 110500, "11.5.1": 110501, "11.5.2": 110502, "11.5.3": 110503,
	"11.5.4": 110504,
	"12.0.0": 120000, "12.0.1": 120001, "12.1.0": 120500,
}

// gfxTargetName renders KFD's gfx_target_version the way the ROCm stack
// and ollama spell it: major*10000 + minor*100 + stepping, with the
// stepping as one hex digit (90012 is gfx90c, 110501 is gfx1151).
func gfxTargetName(v int) string {
	if v <= 0 {
		return ""
	}
	major, minor, step := v/10000, (v/100)%100, v%100
	if minor > 9 || step > 15 {
		return ""
	}
	return fmt.Sprintf("gfx%d%d%x", major, minor, step)
}

// kfdNode is what one KFD topology node says about a GPU.
type kfdNode struct {
	gfxTargetVersion int
	memMB            int
}

// readKFDNodes indexes the GPU nodes of the KFD topology by PCI address
// ("0000:10:00.0"). CPU nodes carry gfx_target_version 0 and are skipped.
func readKFDNodes(root string) map[string]kfdNode {
	nodes, _ := filepath.Glob(filepath.Join(root, "sys", "class", "kfd", "kfd", "topology", "nodes", "*"))
	out := map[string]kfdNode{}
	for _, n := range nodes {
		props := readProperties(filepath.Join(n, "properties"))
		gfx := int(props["gfx_target_version"])
		if gfx == 0 {
			continue
		}
		loc := props["location_id"]
		addr := fmt.Sprintf("%04x:%02x:%02x.%x", props["domain"], loc>>8, (loc>>3)&0x1f, loc&0x7)
		mem := readProperties(filepath.Join(n, "mem_banks", "0", "properties"))
		out[addr] = kfdNode{gfxTargetVersion: gfx, memMB: int(mem["size_in_bytes"] / (1 << 20))}
	}
	return out
}

// readProperties parses a KFD "name value" properties file.
func readProperties(path string) map[string]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// readGCVersion reads the graphics IP version from amdgpu's IP discovery
// tree ("11.5.1"). Present since Linux 5.18 on parts that have a
// discovery table.
func readGCVersion(dev string) (string, bool) {
	base := filepath.Join(dev, "ip_discovery", "die", "0", "GC", "0")
	major, minor, rev := readTrim(filepath.Join(base, "major")), readTrim(filepath.Join(base, "minor")), readTrim(filepath.Join(base, "revision"))
	if major == "" || minor == "" || rev == "" {
		return "", false
	}
	return major + "." + minor + "." + rev, true
}

// pciAddressOf is the device's PCI address, the last element of where
// its sysfs link points ("0000:10:00.0").
func pciAddressOf(dev string) string {
	target, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.Base(target))
}

// amdModelName names the device: the board's own product name where the
// driver exposes one, else libdrm's amdgpu.ids entry for the device and
// revision, else the PCI pair.
func amdModelName(dev, deviceID, revision string, names map[string]string) string {
	if p := readTrim(filepath.Join(dev, "product_name")); p != "" {
		return p
	}
	if n, ok := names[strings.ToUpper(deviceID)+","+strings.ToUpper(revision)]; ok {
		return n
	}
	return "AMD GPU " + PCIIDFromSysfs("0x1002", "0x"+deviceID)
}

// loadAMDGPUIDs reads libdrm's amdgpu.ids — "DEVICE,\tREVISION,\tNAME",
// hex — which is the table Mesa and every libdrm client name AMD parts
// from. It is a naming table only: entries are corrected over time
// (150E,C4 moved from "890M" to "880M"), so nothing but the display name
// is taken from it.
func loadAMDGPUIDs(root string) map[string]string {
	for _, p := range []string{
		"usr/share/libdrm/amdgpu.ids",
		"usr/local/share/libdrm/amdgpu.ids",
		"opt/amdgpu/share/libdrm/amdgpu.ids",
	} {
		f, err := os.Open(filepath.Join(root, p))
		if err != nil {
			continue
		}
		out := map[string]string{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ",", 3)
			if len(parts) != 3 {
				continue
			}
			key := strings.ToUpper(strings.TrimSpace(parts[0])) + "," + strings.ToUpper(strings.TrimSpace(parts[1]))
			out[key] = strings.TrimSpace(parts[2])
		}
		f.Close()
		return out
	}
	return nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(path string) uint64 {
	n, err := strconv.ParseUint(readTrim(path), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
