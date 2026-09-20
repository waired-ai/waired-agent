//go:build linux

package hardware

import (
	"os"
	"path/filepath"
	"strings"
)

// pciIDFromOS reads prof.GPUs[i]'s PCI vendor:device pair from sysfs.
//
// Both files are world-readable, which is the point: this is the half of
// the hardware's identity the product can have without the render-node
// privilege the amdgpu ioctl needs.
//
// The GPU entries come from nvidia-smi and rocm-smi row order and carry
// no bus address, so a device can only be matched back to sysfs by
// vendor. Where a vendor has exactly one DRM device the match is
// certain; where it has more than one it is a guess, and a guessed
// identity in a provenance record is worse than none, so the answer is
// "" — the same shape integratedFromOS takes for the same reason.
func pciIDFromOS(prof *Profile, i int) string {
	if i < 0 || i >= len(prof.GPUs) {
		return ""
	}
	want := pciVendorWordFor(prof.GPUs[i].Vendor)
	if want == "" {
		return ""
	}
	var found string
	for _, e := range drmDeviceDirs() {
		vb, err := os.ReadFile(filepath.Join(e, "vendor"))
		if err != nil || strings.TrimSpace(string(vb)) != want {
			continue
		}
		db, err := os.ReadFile(filepath.Join(e, "device"))
		if err != nil {
			continue
		}
		id := PCIIDFromSysfs(string(vb), string(db))
		if id == "" {
			continue
		}
		if found != "" && found != id {
			// Two devices of one vendor, and nothing here says which
			// entry is which.
			return ""
		}
		found = id
	}
	return found
}

// drmDeviceDirs lists the sysfs device directory of each DRM card.
//
// card* rather than renderD*: a display-only device has no render node,
// and this read is about identity rather than about submitting work.
func drmDeviceDirs() []string {
	entries, err := filepath.Glob("/sys/class/drm/card[0-9]*")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(filepath.Base(e), "-") {
			// card0-DP-1 and friends are connectors, not devices.
			continue
		}
		out = append(out, filepath.Join(e, "device"))
	}
	return out
}

// pciVendorWordFor maps this repo's lowercase vendor token to the word
// sysfs prints. Apple is absent on purpose: Apple Silicon has no PCI
// bus, and its GPU is named by the chip instead.
func pciVendorWordFor(vendor string) string {
	switch strings.ToLower(strings.TrimSpace(vendor)) {
	case "nvidia":
		return "0x10de"
	case "amd":
		return "0x1002"
	case "intel":
		return "0x8086"
	default:
		return ""
	}
}
