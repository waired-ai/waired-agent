package hardware

import "testing"

// The inputs are strings read off this fleet's own machines on
// 2026-09-20, not constructed ones: the Windows rows come from the
// display-adapter registry on the Strix Halo reference host, and the
// sysfs rows from the Linux host that carries a discrete NVIDIA card
// beside an AMD iGPU.
//
// The point of the pair is that BOTH operating systems name one part the
// same way, so the AMD rows below are the check that matters: the
// registry and sysfs disagree about the model string for that part and
// agree about this.
func TestPCIIDFromWindowsMatchingID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`PCI\VEN_1002&DEV_1586&SUBSYS_801D2014&REV_C1`, "1002:1586"},
		{`PCI\VEN_10DE&DEV_2C34&SUBSYS_00000000&REV_A1`, "10de:2c34"},
		{`PCI\VEN_8086&DEV_7D55`, "8086:7d55"},
		{`pci\ven_1002&dev_1586`, "1002:1586"},
		// A Remote Display Adapter carries no pair at all; the reference
		// host reports two of them beside the real GPU.
		{`RdpIdd_IndirectDisplay`, ""},
		{"", ""},
		{`PCI\VEN_10DE`, ""},
		{`PCI\VEN_10D&DEV_2C34`, ""},
	} {
		if got := PCIIDFromWindowsMatchingID(tc.in); got != tc.want {
			t.Errorf("PCIIDFromWindowsMatchingID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPCIIDFromSysfs(t *testing.T) {
	for _, tc := range []struct{ vendor, device, want string }{
		{"0x10de\n", "0x2c34\n", "10de:2c34"},
		{"0x1002\n", "0x13c0\n", "1002:13c0"},
		{"0x1002", "0x1586", "1002:1586"},
		{"0X10DE\n", "0X2C34\n", "10de:2c34"},
		// A truncated read must not produce a plausible-looking pair.
		{"0x10d\n", "0x2c34\n", ""},
		{"0x10de\n", "0x2c3\n", ""},
		{"", "0x2c34", ""},
		{"10de", "2c34", ""},
	} {
		if got := PCIIDFromSysfs(tc.vendor, tc.device); got != tc.want {
			t.Errorf("PCIIDFromSysfs(%q, %q) = %q, want %q",
				tc.vendor, tc.device, got, tc.want)
		}
	}
}

// The pair must distinguish the two NVIDIA parts the readable key folds
// together. This is the gap the key's own doc admits to and the reason
// the pair rides beside it (waired-agent#1455).
func TestPCIIDSeparatesPartsTheChipSlugFolds(t *testing.T) {
	const cap = "12.0"
	rtxPro4000 := ChipSlug(GPU{Vendor: "nvidia",
		Model: "NVIDIA RTX PRO 4000 Blackwell", ComputeCap: cap}, "")
	rtx5090 := ChipSlug(GPU{Vendor: "nvidia",
		Model: "NVIDIA GeForce RTX 5090", ComputeCap: cap}, "")
	if rtxPro4000 != rtx5090 {
		t.Fatalf("precondition: both parts report compute capability %s, "+
			"so the chip slug should fold them together", cap)
	}
	a := PCIIDFromSysfs("0x10de", "0x2c34") // RTX PRO 4000 Blackwell
	b := PCIIDFromSysfs("0x10de", "0x2b85") // an RTX 50-series part
	if a == "" || b == "" {
		t.Fatalf("PCI pairs did not parse: %q, %q", a, b)
	}
	if a == b {
		t.Errorf("PCI pairs collide (%q); the pair is what the key relies on "+
			"to tell these apart", a)
	}
}
