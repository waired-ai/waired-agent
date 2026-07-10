//go:build darwin

package hardware

import "testing"

func TestParseSPDisplaysGPUName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Apple M4 Ultra (sppci_model)",
			in: `{
				"SPDisplaysDataType": [
					{ "sppci_model": "Apple M4 Ultra", "_name": "M4 Ultra GPU" }
				]
			}`,
			want: "Apple M4 Ultra",
		},
		{
			name: "fallback to _name when sppci_model missing",
			in: `{
				"SPDisplaysDataType": [
					{ "_name": "Apple M2 Max GPU" }
				]
			}`,
			want: "Apple M2 Max GPU",
		},
		{
			name: "empty array yields empty result",
			in:   `{"SPDisplaysDataType": []}`,
			want: "",
		},
		{
			name: "malformed JSON yields empty result",
			in:   `not json`,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseSPDisplaysGPUName([]byte(c.in)); got != c.want {
				t.Errorf("parseSPDisplaysGPUName = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseSPHardwareChip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Apple Silicon chip_type",
			in: `{
				"SPHardwareDataType": [
					{ "chip_type": "Apple M4", "machine_model": "Mac16,10" }
				]
			}`,
			want: "Apple M4",
		},
		{
			name: "Intel cpu_type fallback",
			in: `{
				"SPHardwareDataType": [
					{ "cpu_type": "Intel Core i7", "machine_model": "Macmini8,1" }
				]
			}`,
			want: "Intel Core i7",
		},
		{
			name: "chip_type wins over cpu_type",
			in: `{
				"SPHardwareDataType": [
					{ "chip_type": "Apple M3 Max", "cpu_type": "ignored" }
				]
			}`,
			want: "Apple M3 Max",
		},
		{
			name: "no recognised key yields empty",
			in:   `{"SPHardwareDataType": [{ "machine_model": "Mac16,10" }]}`,
			want: "",
		},
		{
			name: "malformed JSON yields empty",
			in:   `not json`,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseSPHardwareChip([]byte(c.in)); got != c.want {
				t.Errorf("parseSPHardwareChip = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseVMStatAvailableBytes(t *testing.T) {
	// A realistic vm_stat sample (16 KiB pages). Available =
	// free(14921) + inactive(423000) + speculative(25227) +
	// purgeable(2141) = 465289 pages × 16384 = 7,623,495,680 bytes.
	const sample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    14921.
Pages active:                                 429524.
Pages inactive:                               423000.
Pages speculative:                             25227.
Pages throttled:                                   0.
Pages wired down:                              84981.
Pages purgeable:                                2141.
"Translation faults":                      123456789.
`
	const wantAvail = uint64(465289) * 16384

	got, err := parseVMStatAvailableBytes([]byte(sample))
	if err != nil {
		t.Fatalf("parseVMStatAvailableBytes returned error: %v", err)
	}
	if got != wantAvail {
		t.Errorf("parseVMStatAvailableBytes = %d, want %d", got, wantAvail)
	}

	t.Run("missing page-size header errors", func(t *testing.T) {
		if _, err := parseVMStatAvailableBytes([]byte("Pages free: 100.\n")); err == nil {
			t.Error("expected error for missing page-size header, got nil")
		}
	})
	t.Run("missing Pages free errors", func(t *testing.T) {
		in := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages active: 100.\n"
		if _, err := parseVMStatAvailableBytes([]byte(in)); err == nil {
			t.Error("expected error for missing 'Pages free' line, got nil")
		}
	})
	t.Run("absent optional classes count as zero", func(t *testing.T) {
		// Only free present; inactive/speculative/purgeable absent.
		in := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 10.\n"
		got, err := parseVMStatAvailableBytes([]byte(in))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != uint64(10)*4096 {
			t.Errorf("got %d, want %d", got, uint64(10)*4096)
		}
	})
}
