package hardware

import "testing"

func TestGFXTargetName(t *testing.T) {
	for v, want := range map[int]string{
		110501: "gfx1151", 110000: "gfx1100", 120001: "gfx1201", 100306: "gfx1036",
		90012: "gfx90c", 90010: "gfx90a", 90402: "gfx942", 110003: "gfx1103",
		0: "", -1: "",
	} {
		if got := gfxTargetName(v); got != want {
			t.Errorf("gfxTargetName(%d) = %q, want %q", v, got, want)
		}
	}
}

// The APU targets are derived, not listed: this pins what the derivation
// produces, so a change to either kernel table shows up here.
func TestAMDAPUTargets(t *testing.T) {
	for _, apu := range []string{"gfx902", "gfx90c", "gfx1033", "gfx1035", "gfx1036", "gfx1103", "gfx1150", "gfx1151", "gfx1152"} {
		if _, ok := amdAPUTargets[apu]; !ok {
			t.Errorf("%s missing from the APU targets", apu)
		}
	}
	for _, dgpu := range []string{"gfx1030", "gfx1100", "gfx1101", "gfx1102", "gfx1200", "gfx1201", "gfx942"} {
		if _, ok := amdAPUTargets[dgpu]; ok {
			t.Errorf("%s is a discrete target but is listed as an APU", dgpu)
		}
	}
}

func TestAMDGFXTargetForPCIID(t *testing.T) {
	for id, want := range map[string]string{
		"1002:744c": "gfx1100", // RX 7900 XTX / XT
		"1002:7550": "gfx1201", // RX 9070 XT
		"1002:7590": "gfx1200", // RX 9060 XT
		"1002:747e": "gfx1101", // RX 7800 XT
		"1002:73bf": "gfx1030", // RX 6800 XT
		"1002:1586": "gfx1151", // Strix Halo (the reference host's pair)
		"1002:13c0": "gfx1036", // Granite Ridge (the Linux fleet host's iGPU)
		"1002:150e": "gfx1150",
		"1002:ffff": "",
		"10de:2c34": "", // not AMD
		"":          "",
	} {
		if got := amdGFXTargetForPCIID(id); got != want {
			t.Errorf("amdGFXTargetForPCIID(%q) = %q, want %q", id, got, want)
		}
	}
}
