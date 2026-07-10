//go:build windows

package hardware

import (
	"context"
	"testing"
)

// TestAMDWindowsFallback_NoCrash verifies the registry walk does not
// panic on any developer/CI host. The test runner may or may not
// have an AMD GPU installed; the contract is "returns a slice
// (possibly nil) without panicking, every returned entry has the
// AMD vendor tag set". The presence/absence of AMD hardware is not
// asserted because CI Windows runners do not ship one. VRAMTotalMB
// may now be either 0 (registry value missing on old drivers) or
// > 0 (modern drivers populate HardwareInformation.qwMemorySize),
// so that field is no longer asserted here.
func TestAMDWindowsFallback_NoCrash(t *testing.T) {
	gpus := amdWindowsFallback(context.Background())
	for i, g := range gpus {
		if g.Vendor != "amd" {
			t.Errorf("amdWindowsFallback()[%d].Vendor = %q, want amd", i, g.Vendor)
		}
		if g.VRAMTotalMB < 0 {
			t.Errorf("amdWindowsFallback()[%d].VRAMTotalMB = %d, want >= 0", i, g.VRAMTotalMB)
		}
	}
}

// TestIsAdapterInstanceKey covers the zero-padded numeric subkey
// filter that skips "Properties" and other non-instance subkeys
// inside the display class GUID.
func TestIsAdapterInstanceKey(t *testing.T) {
	cases := map[string]bool{
		"0000":       true,
		"0001":       true,
		"0023":       true,
		"9999":       true,
		"":           false,
		"000":        false, // 3 chars
		"00000":      false, // 5 chars
		"Properties": false,
		"00A0":       false, // non-digit
		"abcd":       false,
	}
	for in, want := range cases {
		if got := isAdapterInstanceKey(in); got != want {
			t.Errorf("isAdapterInstanceKey(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestParseAdapterVRAMBytes covers the little-endian byte-buffer parse
// for HardwareInformation.qwMemorySize (8 bytes) and the legacy
// HardwareInformation.MemorySize (4 bytes, wraps at 4 GB). Real adapter
// values cross 4 GB on Strix Halo so the 8-byte path is the one that
// matters; the 4-byte path stays for older driver compatibility.
func TestParseAdapterVRAMBytes(t *testing.T) {
	t.Run("8-byte little-endian 64 GiB", func(t *testing.T) {
		// 64 GiB = 0x10_0000_0000 bytes
		buf := []byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00}
		const want = 64 * 1024
		if got := parseQWORDMemorySize(buf); got != want {
			t.Errorf("parseQWORDMemorySize(%v) = %d, want %d", buf, got, want)
		}
	})
	t.Run("8-byte wrong length returns 0", func(t *testing.T) {
		if got := parseQWORDMemorySize([]byte{0x01, 0x02, 0x03}); got != 0 {
			t.Errorf("parseQWORDMemorySize(3-byte) = %d, want 0", got)
		}
		if got := parseQWORDMemorySize(nil); got != 0 {
			t.Errorf("parseQWORDMemorySize(nil) = %d, want 0", got)
		}
	})
	t.Run("4-byte little-endian 2 GiB", func(t *testing.T) {
		// 2 GiB = 0x8000_0000 bytes
		buf := []byte{0x00, 0x00, 0x00, 0x80}
		const want = 2 * 1024
		if got := parseDWORDMemorySize(buf); got != want {
			t.Errorf("parseDWORDMemorySize(%v) = %d, want %d", buf, got, want)
		}
	})
	t.Run("4-byte wrong length returns 0", func(t *testing.T) {
		if got := parseDWORDMemorySize([]byte{0x01}); got != 0 {
			t.Errorf("parseDWORDMemorySize(1-byte) = %d, want 0", got)
		}
	})
}
