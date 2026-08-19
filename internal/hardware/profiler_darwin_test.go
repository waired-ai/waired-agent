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
