//go:build windows

// Windows display-adapter inventory, shared by the vendor detectors.
//
// The registry class below is the OS's own list of installed graphics
// adapters: it answers "what card is in this machine" without a vendor
// CLI, without $PATH, and from a LocalSystem service — which is exactly
// what the PATH-only probes could not do (#67, and #179's bug class).
// It is a FALLBACK, never the first answer: vendor tooling and the
// driver libraries report live devices, while this key can outlive the
// card it describes.

package hardware

import (
	"encoding/binary"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// displayClassGUID is the Windows device class GUID for display
// adapters. Every installed graphics adapter instance lives under
// HKLM\SYSTEM\CurrentControlSet\Control\Class\{this GUID}\NNNN
// where NNNN is a 4-digit zero-padded instance index.
const displayClassGUID = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

// windowsDisplayAdapters walks the display-adapters registry class and
// returns one GPU per adapter whose MatchingDeviceId carries pciVendorID
// (e.g. "VEN_1002"), tagged with vendor. VRAMTotalMB is populated from
// the driver-supplied HardwareInformation values (see readAdapterVRAMMB)
// and is 0 when the driver publishes none.
//
// Errors at the registry layer are swallowed: this is a best-effort
// fallback and failure here is functionally identical to "no adapter
// from this vendor". Callers decide whether an incomplete answer (VRAM
// unknown) deserves a soft warning.
func windowsDisplayAdapters(pciVendorID, vendor string) []GPU {
	parent, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		displayClassGUID,
		registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS,
	)
	if err != nil {
		return nil
	}
	defer parent.Close()

	names, err := parent.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}

	var out []GPU
	for _, name := range names {
		if !isAdapterInstanceKey(name) {
			continue
		}
		gpu, ok := readDisplayAdapter(parent, name, pciVendorID, vendor)
		if !ok {
			continue
		}
		out = append(out, gpu)
	}
	return out
}

// isAdapterInstanceKey returns true iff name looks like a device
// instance subkey (4-digit zero-padded numeric). The display-class
// key also contains "Properties" and similar non-instance subkeys
// that must be skipped.
func isAdapterInstanceKey(name string) bool {
	if len(name) != 4 {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readDisplayAdapter opens the named adapter subkey and returns a GPU
// record if the adapter's PCI vendor ID matches. The second return is
// false when the subkey cannot be opened or belongs to another vendor.
func readDisplayAdapter(parent registry.Key, name, pciVendorID, vendor string) (GPU, bool) {
	k, err := registry.OpenKey(parent, name, registry.QUERY_VALUE)
	if err != nil {
		return GPU{}, false
	}
	defer k.Close()

	matching, _, _ := k.GetStringValue("MatchingDeviceId")
	if !strings.Contains(strings.ToUpper(trimNul(matching)), pciVendorID) {
		return GPU{}, false
	}

	desc, _, _ := k.GetStringValue("DriverDesc")
	driverVer, _, _ := k.GetStringValue("DriverVersion")
	return GPU{
		Vendor:        vendor,
		Model:         trimNul(desc),
		DriverVersion: trimNul(driverVer),
		VRAMTotalMB:   readAdapterVRAMMB(k),
	}, true
}

// readAdapterVRAMMB reads the adapter's total VRAM in MB from the
// driver-supplied HardwareInformation values. Tries the 64-bit
// qwMemorySize first (accurate above the 4 GiB DWORD ceiling) and
// falls back to the legacy 32-bit MemorySize. Returns 0 when neither
// is readable, leaving VRAMTotalMB at its zero value so callers can
// surface a soft warning.
func readAdapterVRAMMB(k registry.Key) int {
	if buf, _, err := k.GetBinaryValue("HardwareInformation.qwMemorySize"); err == nil {
		if mb := parseQWORDMemorySize(buf); mb > 0 {
			return mb
		}
	}
	// Some driver builds publish the value as REG_QWORD (read via
	// GetIntegerValue) rather than REG_BINARY. Try that shape too.
	if n, _, err := k.GetIntegerValue("HardwareInformation.qwMemorySize"); err == nil && n > 0 {
		return int(n / (1024 * 1024))
	}
	if buf, _, err := k.GetBinaryValue("HardwareInformation.MemorySize"); err == nil {
		if mb := parseDWORDMemorySize(buf); mb > 0 {
			return mb
		}
	}
	if n, _, err := k.GetIntegerValue("HardwareInformation.MemorySize"); err == nil && n > 0 {
		return int(n / (1024 * 1024))
	}
	return 0
}

// parseQWORDMemorySize decodes an 8-byte little-endian REG_BINARY value
// (the shape modern drivers use for HardwareInformation.qwMemorySize)
// and returns the size in MB. Returns 0 when the buffer is not exactly
// 8 bytes long.
func parseQWORDMemorySize(buf []byte) int {
	if len(buf) != 8 {
		return 0
	}
	n := binary.LittleEndian.Uint64(buf)
	return int(n / (1024 * 1024))
}

// parseDWORDMemorySize decodes a 4-byte little-endian REG_BINARY value
// (the legacy shape from pre-Adrenalin AMD drivers for
// HardwareInformation.MemorySize) and returns the size in MB. Returns
// 0 when the buffer is not exactly 4 bytes long. The value wraps at
// 4 GiB so this path is unreliable for modern dGPUs and Strix Halo
// UMA budgets; the QWORD form is preferred whenever available.
func parseDWORDMemorySize(buf []byte) int {
	if len(buf) != 4 {
		return 0
	}
	n := binary.LittleEndian.Uint32(buf)
	return int(n / (1024 * 1024))
}
