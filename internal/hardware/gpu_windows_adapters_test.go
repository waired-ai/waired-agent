//go:build windows

package hardware

import "testing"

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

// The shared walk must survive any host: a Windows CI runner has no
// discrete GPU at all, and a developer box may have either vendor. The
// contract is "no panic, and every entry carries the vendor tag the
// caller asked for".
func TestWindowsDisplayAdapters_NoCrash(t *testing.T) {
	for _, tc := range []struct{ id, vendor string }{
		{nvidiaPCIVendorID, "nvidia"},
		{amdPCIVendorID, "amd"},
		{"VEN_0000", "nobody"}, // matches nothing; must return empty, not panic
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			for i, g := range windowsDisplayAdapters(tc.id, tc.vendor) {
				if g.Vendor != tc.vendor {
					t.Errorf("adapter[%d].Vendor = %q, want %q", i, g.Vendor, tc.vendor)
				}
				if g.VRAMTotalMB < 0 {
					t.Errorf("adapter[%d].VRAMTotalMB = %d, want >= 0", i, g.VRAMTotalMB)
				}
			}
		})
	}
}
