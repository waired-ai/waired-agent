package hardware

import "strings"

// Intel GPUs by PCI device ID, copied from the kernel (waired-agent#1483).
//
// include/drm/intel/pciids.h (Linux v7.0) lists every device ID i915 and
// xe bind, grouped by platform, and drivers/gpu/drm/xe/xe_pci.c marks the
// discrete platforms (.is_dgfx = 1 through DGFX_FEATURES): DG1, DG2
// (Alchemist, including the ATS-M datacenter parts), BMG (Battlemage),
// PVC and CRI. Every other platform is integrated. The table below holds
// BOTH sides from Gen9 (Skylake) on, so that an ID in neither — a part
// newer than the copy — is unknown rather than guessed: an unknown Intel
// GPU is set aside only if its memory cannot be read (engine_gpus.go),
// and on Windows the carve-out arithmetic can still recognise an
// integrated one.
//
// The platform names are the kernel's own (INTEL_<PLATFORM>_IDS), and
// they are what a discrete Intel card's host key carries — the Intel
// counterpart of NVIDIA's compute capability and AMD's ISA target: a
// B580 is "bmg", an A770 "dg2".
//
// Regenerate from a newer header with the same grouping when new silicon
// ships; #1486 carries the cadence.

// intelPart is what the kernel's table says about one device ID.
type intelPart struct {
	platform string
	discrete bool
}

// intelParts maps a lowercase PCI device ID to its platform.
var intelParts = map[string]intelPart{
	// dg1
	"4905": {"dg1", true}, "4906": {"dg1", true}, "4907": {"dg1", true}, "4908": {"dg1", true},
	"4909": {"dg1", true},
	// dg2
	"5690": {"dg2", true}, "5691": {"dg2", true}, "5692": {"dg2", true}, "5693": {"dg2", true},
	"5694": {"dg2", true}, "5695": {"dg2", true}, "5696": {"dg2", true}, "5697": {"dg2", true},
	"56a0": {"dg2", true}, "56a1": {"dg2", true}, "56a2": {"dg2", true}, "56a3": {"dg2", true},
	"56a4": {"dg2", true}, "56a5": {"dg2", true}, "56a6": {"dg2", true}, "56b0": {"dg2", true},
	"56b1": {"dg2", true}, "56b2": {"dg2", true}, "56b3": {"dg2", true}, "56ba": {"dg2", true},
	"56bb": {"dg2", true}, "56bc": {"dg2", true}, "56bd": {"dg2", true}, "56be": {"dg2", true},
	"56bf": {"dg2", true},
	// atsm
	"56c0": {"atsm", true}, "56c1": {"atsm", true}, "56c2": {"atsm", true},
	// pvc
	"0b69": {"pvc", true}, "0b6e": {"pvc", true}, "0bd4": {"pvc", true}, "0bd5": {"pvc", true},
	"0bd6": {"pvc", true}, "0bd7": {"pvc", true}, "0bd8": {"pvc", true}, "0bd9": {"pvc", true},
	"0bda": {"pvc", true}, "0bdb": {"pvc", true}, "0be0": {"pvc", true}, "0be1": {"pvc", true},
	"0be5": {"pvc", true},
	// bmg
	"e202": {"bmg", true}, "e209": {"bmg", true}, "e20b": {"bmg", true}, "e20c": {"bmg", true},
	"e20d": {"bmg", true}, "e210": {"bmg", true}, "e211": {"bmg", true}, "e212": {"bmg", true},
	"e216": {"bmg", true}, "e220": {"bmg", true}, "e221": {"bmg", true}, "e222": {"bmg", true},
	"e223": {"bmg", true},
	// cri
	"674c": {"cri", true},

	// skl
	"1902": {"skl", false}, "1906": {"skl", false}, "190a": {"skl", false}, "190b": {"skl", false},
	"190e": {"skl", false}, "1912": {"skl", false}, "1913": {"skl", false}, "1915": {"skl", false},
	"1916": {"skl", false}, "1917": {"skl", false}, "191a": {"skl", false}, "191b": {"skl", false},
	"191d": {"skl", false}, "191e": {"skl", false}, "1921": {"skl", false}, "1923": {"skl", false},
	"1926": {"skl", false}, "1927": {"skl", false}, "192a": {"skl", false}, "192b": {"skl", false},
	"192d": {"skl", false}, "1932": {"skl", false}, "193a": {"skl", false}, "193b": {"skl", false},
	"193d": {"skl", false},
	// bxt
	"0a84": {"bxt", false}, "1a84": {"bxt", false}, "1a85": {"bxt", false}, "5a84": {"bxt", false},
	"5a85": {"bxt", false},
	// glk
	"3184": {"glk", false}, "3185": {"glk", false},
	// kbl
	"5902": {"kbl", false}, "5906": {"kbl", false}, "5908": {"kbl", false}, "590a": {"kbl", false},
	"590b": {"kbl", false}, "590e": {"kbl", false}, "5912": {"kbl", false}, "5913": {"kbl", false},
	"5915": {"kbl", false}, "5916": {"kbl", false}, "5917": {"kbl", false}, "591a": {"kbl", false},
	"591b": {"kbl", false}, "591c": {"kbl", false}, "591d": {"kbl", false}, "591e": {"kbl", false},
	"5921": {"kbl", false}, "5923": {"kbl", false}, "5926": {"kbl", false}, "5927": {"kbl", false},
	"593b": {"kbl", false}, "87c0": {"kbl", false},
	// cfl
	"3e90": {"cfl", false}, "3e91": {"cfl", false}, "3e92": {"cfl", false}, "3e93": {"cfl", false},
	"3e94": {"cfl", false}, "3e96": {"cfl", false}, "3e98": {"cfl", false}, "3e99": {"cfl", false},
	"3e9a": {"cfl", false}, "3e9b": {"cfl", false}, "3e9c": {"cfl", false}, "3ea5": {"cfl", false},
	"3ea6": {"cfl", false}, "3ea7": {"cfl", false}, "3ea8": {"cfl", false}, "3ea9": {"cfl", false},
	"87ca": {"cfl", false},
	// whl
	"3ea0": {"whl", false}, "3ea1": {"whl", false}, "3ea2": {"whl", false}, "3ea3": {"whl", false},
	"3ea4": {"whl", false},
	// cnl
	"5a40": {"cnl", false}, "5a41": {"cnl", false}, "5a42": {"cnl", false}, "5a44": {"cnl", false},
	"5a49": {"cnl", false}, "5a4a": {"cnl", false}, "5a4c": {"cnl", false}, "5a50": {"cnl", false},
	"5a51": {"cnl", false}, "5a52": {"cnl", false}, "5a54": {"cnl", false}, "5a59": {"cnl", false},
	"5a5a": {"cnl", false}, "5a5c": {"cnl", false},
	// icl
	"8a50": {"icl", false}, "8a51": {"icl", false}, "8a52": {"icl", false}, "8a53": {"icl", false},
	"8a54": {"icl", false}, "8a56": {"icl", false}, "8a57": {"icl", false}, "8a58": {"icl", false},
	"8a59": {"icl", false}, "8a5a": {"icl", false}, "8a5b": {"icl", false}, "8a5c": {"icl", false},
	"8a5d": {"icl", false}, "8a70": {"icl", false}, "8a71": {"icl", false},
	// ehl
	"4541": {"ehl", false}, "4551": {"ehl", false}, "4555": {"ehl", false}, "4557": {"ehl", false},
	"4570": {"ehl", false}, "4571": {"ehl", false},
	// jsl
	"4e51": {"jsl", false}, "4e55": {"jsl", false}, "4e57": {"jsl", false}, "4e61": {"jsl", false},
	"4e71": {"jsl", false},
	// tgl
	"9a40": {"tgl", false}, "9a49": {"tgl", false}, "9a59": {"tgl", false}, "9a60": {"tgl", false},
	"9a68": {"tgl", false}, "9a70": {"tgl", false}, "9a78": {"tgl", false}, "9ac0": {"tgl", false},
	"9ac9": {"tgl", false}, "9ad9": {"tgl", false}, "9af8": {"tgl", false},
	// rkl
	"4c80": {"rkl", false}, "4c8a": {"rkl", false}, "4c8b": {"rkl", false}, "4c8c": {"rkl", false},
	"4c90": {"rkl", false}, "4c9a": {"rkl", false},
	// adls
	"4680": {"adls", false}, "4682": {"adls", false}, "4688": {"adls", false}, "468a": {"adls", false},
	"468b": {"adls", false}, "4690": {"adls", false}, "4692": {"adls", false}, "4693": {"adls", false},
	// adlp
	"4626": {"adlp", false}, "4628": {"adlp", false}, "462a": {"adlp", false}, "46a0": {"adlp", false},
	"46a1": {"adlp", false}, "46a2": {"adlp", false}, "46a3": {"adlp", false}, "46a6": {"adlp", false},
	"46a8": {"adlp", false}, "46aa": {"adlp", false}, "46b0": {"adlp", false}, "46b1": {"adlp", false},
	"46b2": {"adlp", false}, "46b3": {"adlp", false}, "46c0": {"adlp", false}, "46c1": {"adlp", false},
	"46c2": {"adlp", false}, "46c3": {"adlp", false},
	// adln
	"46d0": {"adln", false}, "46d1": {"adln", false}, "46d2": {"adln", false}, "46d3": {"adln", false},
	"46d4": {"adln", false},
	// rpls
	"a780": {"rpls", false}, "a781": {"rpls", false}, "a782": {"rpls", false}, "a783": {"rpls", false},
	"a788": {"rpls", false}, "a789": {"rpls", false}, "a78a": {"rpls", false}, "a78b": {"rpls", false},
	// rplu
	"a721": {"rplu", false}, "a7a1": {"rplu", false}, "a7a9": {"rplu", false}, "a7ac": {"rplu", false},
	"a7ad": {"rplu", false},
	// rplp
	"a720": {"rplp", false}, "a7a0": {"rplp", false}, "a7a8": {"rplp", false}, "a7aa": {"rplp", false},
	"a7ab": {"rplp", false},
	// mtl
	"7d40": {"mtl", false}, "7d45": {"mtl", false}, "7d55": {"mtl", false}, "7d60": {"mtl", false},
	"7dd5": {"mtl", false},
	// arl
	"7d41": {"arl", false}, "7d51": {"arl", false}, "7d67": {"arl", false}, "7dd1": {"arl", false},
	"b640": {"arl", false},
	// lnl
	"6420": {"lnl", false}, "64a0": {"lnl", false}, "64b0": {"lnl", false},
	// ptl
	"b080": {"ptl", false}, "b081": {"ptl", false}, "b082": {"ptl", false}, "b083": {"ptl", false},
	"b084": {"ptl", false}, "b085": {"ptl", false}, "b086": {"ptl", false}, "b087": {"ptl", false},
	"b08f": {"ptl", false}, "b090": {"ptl", false}, "b0a0": {"ptl", false}, "b0b0": {"ptl", false},
	// wcl
	"fd80": {"wcl", false}, "fd81": {"wcl", false},
	// nvls
	"d740": {"nvls", false}, "d741": {"nvls", false}, "d742": {"nvls", false}, "d743": {"nvls", false},
	"d744": {"nvls", false}, "d745": {"nvls", false},
}

// intelPartFor looks an Intel PCI pair ("8086:e20b") up in the table.
func intelPartFor(pciID string) (intelPart, bool) {
	vendor, device, ok := strings.Cut(strings.ToLower(pciID), ":")
	if !ok || vendor != "8086" {
		return intelPart{}, false
	}
	p, ok := intelParts[device]
	return p, ok
}
