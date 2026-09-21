package hardware

import "strings"

// The ISA target of an AMD part named by its PCI device ID, for the
// platforms that have no kernel to ask (waired-agent#1485).
//
// On Linux the gfx target comes from KFD, which reads it off the silicon
// (amd_sysfs.go). Windows exposes nothing equivalent without the HIP
// runtime, but it does expose the PCI pair on every adapter
// (MatchingDeviceId), and a device ID names one die. This table joins the
// two published facts that make that usable:
//
//   - pci.ids (the PCI ID database lspci reads, version 2026.09.21)
//     names the die behind each device ID: 744c is "Navi 31", 7550
//     "Navi 48", 1586 "Strix Halo";
//   - LLVM's AMDGPU processor table (and the kernel's kfd_device.c, which
//     agrees) names the ISA of each die: Navi 31 is gfx1100, Navi 48
//     gfx1201, Strix Halo gfx1151.
//
// It is consulted only where no reading supplied a target, so on Linux it
// is a fallback for a kernel without KFD. A device ID missing from it
// leaves the target empty, which is the answer every consumer already
// handles: the host key says "unknown", and the engine-GPU rule falls
// back to the CPU name for Strix Halo.
//
// Rows are dies, not products: RX 7900 XTX and RX 7900 XT share 744c and
// share gfx1100, and differ only by revision — which is why the host key
// keeps the PCI pair beside it rather than relying on this alone.
var amdGFXTargetByDevice = map[string]string{
	// Navi 21 — gfx1030
	"73a1": "gfx1030", "73a2": "gfx1030", "73a3": "gfx1030", "73a5": "gfx1030",
	"73ab": "gfx1030", "73ae": "gfx1030", "73af": "gfx1030", "73bf": "gfx1030",
	// Navi 22 — gfx1031
	"73c3": "gfx1031", "73ce": "gfx1031", "73df": "gfx1031",
	// Navi 23 — gfx1032
	"73e0": "gfx1032", "73e1": "gfx1032", "73e3": "gfx1032", "73ef": "gfx1032", "73ff": "gfx1032",
	// Navi 24 — gfx1034
	"7421": "gfx1034", "7422": "gfx1034", "7423": "gfx1034", "7424": "gfx1034", "743f": "gfx1034",
	// Navi 31 — gfx1100
	"7448": "gfx1100", "7449": "gfx1100", "744a": "gfx1100", "744b": "gfx1100",
	"744c": "gfx1100", "745e": "gfx1100",
	// Navi 32 — gfx1101
	"7460": "gfx1101", "7461": "gfx1101", "7470": "gfx1101", "747e": "gfx1101",
	// Navi 33 — gfx1102
	"73f0": "gfx1102", "7480": "gfx1102", "7481": "gfx1102", "7483": "gfx1102",
	"7487": "gfx1102", "7489": "gfx1102", "748b": "gfx1102", "7499": "gfx1102", "749f": "gfx1102",
	// Navi 44 — gfx1200
	"7590": "gfx1200",
	// Navi 48 — gfx1201
	"7550": "gfx1201", "7551": "gfx1201",

	// APUs. The host key names these by the CPU, so the target matters
	// here only for which integrated GPUs the engine uses by default.
	"1586": "gfx1151",                    // Strix Halo
	"150e": "gfx1150",                    // Strix
	"1114": "gfx1152", "1902": "gfx1152", // Krackan
	"15bf": "gfx1103", "15c8": "gfx1103", // Phoenix1, Phoenix2
	"1900": "gfx1103", "1901": "gfx1103", // Hawk Point
	"1681": "gfx1035",                                       // Rembrandt
	"164e": "gfx1036", "13c0": "gfx1036", "1506": "gfx1036", // Raphael, Granite Ridge, Mendocino
	"163f": "gfx1033",                  // Van Gogh
	"15dd": "gfx902", "15d8": "gfx902", // Raven Ridge, Picasso
	"1636": "gfx90c", "1638": "gfx90c", "164c": "gfx90c", "15e7": "gfx90c", // Renoir, Cezanne, Lucienne, Barcelo
}

// amdGFXTargetForPCIID looks an AMD PCI pair ("1002:744c") up in the
// table, or returns "".
func amdGFXTargetForPCIID(pciID string) string {
	vendor, device, ok := strings.Cut(strings.ToLower(pciID), ":")
	if !ok || vendor != "1002" {
		return ""
	}
	return amdGFXTargetByDevice[device]
}
