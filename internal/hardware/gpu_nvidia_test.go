package hardware

import "testing"

func TestParseNvidiaSMICSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []GPU
	}{
		{
			name: "single Blackwell",
			in:   "NVIDIA RTX PRO 4000 Blackwell, 24467, 595.58.03, 12.0, GPU-abc123\n",
			want: []GPU{{
				Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell",
				VRAMTotalMB: 24467, DriverVersion: "595.58.03",
				ComputeCap: "12.0", UUID: "GPU-abc123",
			}},
		},
		{
			name: "two GPUs",
			in: "NVIDIA L4, 24576, 550.54.15, 8.9, GPU-aaa\n" +
				"NVIDIA L40S, 49152, 550.54.15, 8.9, GPU-bbb\n",
			want: []GPU{
				{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 24576, DriverVersion: "550.54.15", ComputeCap: "8.9", UUID: "GPU-aaa"},
				{Vendor: "nvidia", Model: "NVIDIA L40S", VRAMTotalMB: 49152, DriverVersion: "550.54.15", ComputeCap: "8.9", UUID: "GPU-bbb"},
			},
		},
		{
			name: "trailing blank line tolerated",
			in:   "NVIDIA T4, 16384, 535.86.10, 7.5, GPU-x\n\n",
			want: []GPU{
				{Vendor: "nvidia", Model: "NVIDIA T4", VRAMTotalMB: 16384, DriverVersion: "535.86.10", ComputeCap: "7.5", UUID: "GPU-x"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNvidiaSMICSV(tc.in)
			if err != nil {
				t.Fatalf("parseNvidiaSMICSV: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("GPU[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseNvidiaSMICSV_Malformed(t *testing.T) {
	cases := []string{
		"too, few, fields\n",
		"NVIDIA T4, not-a-number, 535.86.10, 7.5, GPU-x\n",
	}
	for _, in := range cases {
		if _, err := parseNvidiaSMICSV(in); err == nil {
			t.Errorf("parseNvidiaSMICSV(%q) = nil error, want error", in)
		}
	}
}
