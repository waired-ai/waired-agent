package hardware

import (
	"strings"
	"testing"
)

// TestParseROCmSMICSV_SingleCard parses a realistic ROCm 6.x
// `rocm-smi --showproductname --showmeminfo vram --showdriverversion --csv`
// row.
func TestParseROCmSMICSV_SingleCard(t *testing.T) {
	in := strings.Join([]string{
		"device,Card series,Card model,Card vendor,Card SKU,VRAM Total Memory (B),VRAM Total Used Memory (B),Driver version",
		"card0,1002,7900 XTX,Advanced Micro Devices Inc. [AMD/ATI],D7160100,25753026560,16777216,6.7.0",
		"",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("parser err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	g := got[0]
	if g.Vendor != "amd" {
		t.Errorf("Vendor = %q, want amd", g.Vendor)
	}
	if g.Model != "7900 XTX" {
		t.Errorf("Model = %q, want '7900 XTX'", g.Model)
	}
	if g.DriverVersion != "6.7.0" {
		t.Errorf("DriverVersion = %q, want 6.7.0", g.DriverVersion)
	}
	if g.UUID != "card0" {
		t.Errorf("UUID = %q, want card0", g.UUID)
	}
	// 25_753_026_560 / 1024 / 1024 = 24560 MiB (a 24 GiB card).
	if g.VRAMTotalMB != 24560 {
		t.Errorf("VRAMTotalMB = %d, want 24560", g.VRAMTotalMB)
	}
	// Free is DERIVED from the used column in the same row:
	// 16_777_216 B = 16 MiB used, so 24560 - 16 = 24544 MiB free.
	//
	// The point of this assertion is that no rocm-smi flag had to
	// change to get it. --showmeminfo vram was already emitting "VRAM
	// Total Used Memory (B)" — the header above is the same one this
	// test carried before waired-agent#1056 — and the parser read past
	// it, so TightestGPUFreeMB abstained on every AMD host and the
	// post-load fit check (waired-agent#1038) ran with one measure
	// instead of two.
	if g.VRAMFreeMB != 24544 {
		t.Errorf("VRAMFreeMB = %d, want 24544", g.VRAMFreeMB)
	}
}

// TestParseROCmSMICSV_UsedColumnIsSoft pins the asymmetry copied from
// gpu_nvidia.go's memory.free handling (waired-agent#69): the TOTAL is
// load-bearing and a bad one fails the row, but an unreadable USED
// figure costs only the derived free value. Losing the device over it
// would be worse than reporting it with no free reading, which every
// consumer already handles as "no evidence".
func TestParseROCmSMICSV_UsedColumnIsSoft(t *testing.T) {
	in := strings.Join([]string{
		"device,Card model,VRAM Total Memory (B),VRAM Total Used Memory (B)",
		"card0,7900 XTX,25753026560,[N/A]",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("an unreadable used figure must not fail the row: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].VRAMTotalMB != 24560 {
		t.Errorf("VRAMTotalMB = %d, want 24560", got[0].VRAMTotalMB)
	}
	if got[0].VRAMFreeMB != 0 {
		t.Errorf("VRAMFreeMB = %d, want 0 (no evidence)", got[0].VRAMFreeMB)
	}
}

// TestParseROCmSMICSV_UsedExceedingTotalIsNotNegativeFree records
// today's behaviour for a row that cannot be true. Free stays 0 —
// "no evidence" — rather than a negative number a caller would compare
// against a floor.
func TestParseROCmSMICSV_UsedExceedingTotalIsNotNegativeFree(t *testing.T) {
	in := strings.Join([]string{
		"device,VRAM Total Memory (B),VRAM Total Used Memory (B)",
		"card0,1048576,25753026560",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("parser err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].VRAMFreeMB != 0 {
		t.Errorf("VRAMFreeMB = %d, want 0", got[0].VRAMFreeMB)
	}
}

// TestParseROCmSMICSV_TwoCards verifies a multi-GPU row layout.
func TestParseROCmSMICSV_TwoCards(t *testing.T) {
	in := strings.Join([]string{
		"device,Card model,VRAM Total Memory (B),Driver version",
		"card0,7900 XTX,25753026560,6.7.0",
		"card1,7800 XT,17179869184,6.7.0",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("parser err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Model != "7900 XTX" || got[1].Model != "7800 XT" {
		t.Errorf("Models = %q, %q", got[0].Model, got[1].Model)
	}
	// 17_179_869_184 / 1024 / 1024 = 16384 MiB.
	if got[1].VRAMTotalMB != 16384 {
		t.Errorf("got[1].VRAMTotalMB = %d, want 16384", got[1].VRAMTotalMB)
	}
	// A rocm-smi build that reports a total and no used column: free
	// stays 0 on both rows, which TightestGPUFreeMB reads as "no
	// evidence" rather than "the cards are full".
	for i, g := range got {
		if g.VRAMFreeMB != 0 {
			t.Errorf("got[%d].VRAMFreeMB = %d, want 0", i, g.VRAMFreeMB)
		}
	}
}

// TestParseROCmSMICSV_HeaderOrderTolerance verifies the parser keys
// by header name, not column position. rocm-smi's column order
// varies across ROCm versions, so positional parsing (which works
// for nvidia-smi because we pin columns via --query-gpu=) is unsafe.
func TestParseROCmSMICSV_HeaderOrderTolerance(t *testing.T) {
	in := strings.Join([]string{
		"VRAM Total Memory (B),Driver version,device,Card model",
		"25753026560,6.7.0,card0,7900 XTX",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("parser err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	g := got[0]
	if g.Vendor != "amd" || g.Model != "7900 XTX" || g.DriverVersion != "6.7.0" || g.UUID != "card0" || g.VRAMTotalMB != 24560 {
		t.Errorf("GPU = %+v", g)
	}
}

// TestParseROCmSMICSV_MissingVRAMColumn covers `rocm-smi` invocations
// that omit `--showmeminfo vram`. Model/Driver still populate;
// VRAMTotalMB stays 0 (= "unknown" to the model picker) and no
// parser error is raised.
func TestParseROCmSMICSV_MissingVRAMColumn(t *testing.T) {
	in := strings.Join([]string{
		"device,Card model,Driver version",
		"card0,7900 XTX,6.7.0",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("parser err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].VRAMTotalMB != 0 {
		t.Errorf("missing VRAM column should yield VRAMTotalMB=0, got %d", got[0].VRAMTotalMB)
	}
	if got[0].VRAMFreeMB != 0 {
		t.Errorf("missing VRAM column should yield VRAMFreeMB=0, got %d", got[0].VRAMFreeMB)
	}
	if got[0].Model != "7900 XTX" {
		t.Errorf("Model = %q, want '7900 XTX'", got[0].Model)
	}
}

// TestParseROCmSMICSV_Malformed covers parser failure modes.
func TestParseROCmSMICSV_Malformed(t *testing.T) {
	cases := map[string]string{
		"non-numeric VRAM bytes": "device,VRAM Total Memory (B)\ncard0,not-a-number\n",
		"column count mismatch":  "device,Card model,VRAM Total Memory (B),Driver version\ncard0,7900 XTX,25753026560\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseROCmSMICSV(in); err == nil {
				t.Errorf("expected error parsing %q", in)
			}
		})
	}
}

// TestParseROCmSMICSV_FallbackModelColumns verifies the parser
// prefers Card model, falls back to Card series, Marketing Name,
// then Card SKU when newer rocm-smi versions omit one of them.
func TestParseROCmSMICSV_FallbackModelColumns(t *testing.T) {
	in := strings.Join([]string{
		"device,Card series,VRAM Total Memory (B)",
		"card0,Radeon Pro W7900,51539607552",
	}, "\n")
	got, err := parseROCmSMICSV(in)
	if err != nil {
		t.Fatalf("parser err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Model != "Radeon Pro W7900" {
		t.Errorf("expected Card series fallback for Model, got %q", got[0].Model)
	}
}

// TestParseROCmSMICSV_Empty handles the degenerate case of header
// only / completely empty output. No error, no rows.
func TestParseROCmSMICSV_Empty(t *testing.T) {
	cases := []string{
		"",
		"\n",
		"device,Card model,VRAM Total Memory (B)\n",
	}
	for _, in := range cases {
		got, err := parseROCmSMICSV(in)
		if err != nil {
			t.Errorf("parseROCmSMICSV(%q) err = %v, want nil", in, err)
		}
		if len(got) != 0 {
			t.Errorf("parseROCmSMICSV(%q) len = %d, want 0", in, len(got))
		}
	}
}
