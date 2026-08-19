package hardware

import (
	"regexp"
	"strings"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// IsStrixHaloAPU recognises AMD's Ryzen AI Max series (Strix Halo) via
// the human-readable CPU model string supplied by /proc/cpuinfo on
// Linux or the CentralProcessor registry key on Windows. Match is
// case-insensitive and substring-based so future revs ("Ryzen AI Max
// 395+", "Ryzen AI Max+ PRO 395") all hit. Other AMD APUs (Phoenix,
// Hawk Point) have much smaller iGPUs and don't change picker
// decisions, so they intentionally do not match.
//
// Shared across profiler_linux.go and profiler_windows.go — both
// reach for the same model substring even though they read it via
// different OS interfaces. Exported because the Ollama backend
// selector (internal/runtime) keys the Strix Halo GPU-backend decision
// off the CPU model: on Linux the iGPU is invisible to the profiler
// unless rocm-smi is installed, so the CPU string is the only reliable
// Strix Halo signal (#290).
func IsStrixHaloAPU(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "ryzen ai max")
}

// amdMobileiGPURe matches a numbered AMD mobile-APU iGPU token — three
// digits followed by "m" (Radeon 610M/660M/680M/740M/760M/780M/860M/
// 880M/890M …). Exactly three digits, so it does NOT match a discrete
// mobile card ("Radeon RX 7600M", four digits) or Strix Halo's Radeon
// 8060S (no trailing "m").
var amdMobileiGPURe = regexp.MustCompile(`\b\d{3}m\b`)

// IsAMDMobileAPU recognises a non-Strix AMD APU that carries a *numbered*
// mobile iGPU (Radeon 780M/760M/880M …) from the CPU model string, the
// same way IsStrixHaloAPU keys off the CPU model. It is a last-resort
// GPU-backend signal for the Ollama selector (internal/runtime): on Linux
// such an iGPU is invisible to the profiler without rocm-smi, so the host
// reports no GPU (PrimaryGPUVendor == "") and would fall to CPU even
// though the iGPU is worth engaging via Vulkan (#68).
//
// The match requires BOTH the "radeon" marker and a three-digit "…M"
// token, so a vestigial desktop iGPU reported as bare "Radeon Graphics"
// (~2 CU, frequently slower than the CPU) deliberately does NOT match,
// and neither does Strix Halo's "Radeon 8060S" (already handled upstream
// by IsStrixHaloAPU). A detected AMD iGPU takes the internal/runtime
// case "amd" path instead; this only fires when nothing was detected.
func IsAMDMobileAPU(modelName string) bool {
	m := strings.ToLower(modelName)
	return strings.Contains(m, "radeon") && amdMobileiGPURe.MatchString(m)
}

// minNonZero returns the smallest positive value among the inputs, or
// 0 when every input is non-positive. Used by the UMA heuristics to
// combine a detected VRAM amount, a 75 %-of-RAM heuristic, and a known
// driver / BIOS / Vulkan ceiling without nested if-statements.
func minNonZero(values ...int) int {
	out := 0
	for _, v := range values {
		if v <= 0 {
			continue
		}
		if out == 0 || v < out {
			out = v
		}
	}
	return out
}

// strixHaloUMACapMB is the BIOS UMA ceiling shipped on current Strix
// Halo platforms (Ryzen AI Max series). It clamps both the carve-out
// reading and the heuristic fallback; raise it as future BIOS revisions
// allow larger GPU-side allocations.
const strixHaloUMACapMB = 96 * 1024

// strixHaloUMA computes the GPU-addressable memory budget and the
// additive firmware carve-out for a Strix Halo UMA host. It takes goos
// because the two operating systems that reach it answer differently,
// and routing that through one untagged function keeps both answers in
// one table test rather than in two build-tagged files.
//
// It returns BOTH figures because the branch it took is itself a fact
// downstream needs. carveOutMB is non-zero only where the budget is
// memory the OS excluded from ramTotalGB AND a model may occupy it in
// addition, which is the sum hostfit.TotalMemoryMB forms. Where the
// budget is a slice OF ramTotalGB the carve-out is 0, because adding it
// would count the same bytes twice. One function returns the pair so
// the two profilers cannot each decide half of it and disagree.
//
// # Windows: the carve-out is subtracted, not added
//
// Measured on a Ryzen AI Max+ 395 by changing only the AMD Variable
// Graphics Memory size and re-running the same load
// (waired-ai/waired-agent#863). With a 96 GB carve-out the OS saw
// 31.65 GB and a 76.3 GB model failed to load after 27.9 minutes; with
// a 512 MB carve-out the OS saw 127.15 GB and the same model loaded in
// 15.0 s at 26.32 tok/s. Every Windows video allocation is pageable, so
// the video memory manager charges a system-memory backing store commit
// at allocation time ("Every graphics allocation in the WDDM model has
// a backing store … a committed memory buffer", Microsoft). The weights
// reached the carve-out AND 74.8 GB of commit was charged against the
// 31.65 GB the OS still had; it could not be resident, the page file
// took it, and the allocation was then evicted from the carve-out too.
//
// So on Windows the memory a model can occupy is the OS-visible RAM
// minus the OS's own reserve, whatever the carve-out size — the same
// quantity hostfit.TotalMemoryMB forms, which is why the deduction is
// taken from hostfit rather than re-derived here. The carve-out reading
// is therefore neither the budget nor an addend, and this function
// returns 0 for it. The registry figure survives on GPUs[0].VRAMTotalMB
// for diagnostics.
//
// This is a record of what those two configurations measured, not a
// platform contract: only the one host was measured, and the mechanism
// above is documented for WDDM, not for amdgpu.
//
// # Linux: unchanged
//
// amdgpu reaches system memory through GTT, which reserves nothing
// permanently, and AMD's own guidance is a small BIOS carve-out plus a
// large GTT limit rather than the Windows arrangement. Nothing here
// reads GTT and no Linux Strix Halo was measured, so the Linux answer
// is left exactly as it was: a carve-out reading — clamped to the BIOS
// UMA ceiling — is the budget and is additive, and only its absence
// falls back to the 75 %-of-RAM heuristic. See waired-ai/waired-agent#864.
func strixHaloUMA(goos string, amdVRAMMB, ramTotalGB, ramAvailableAtInstallGB int) (usableVRAMMB, carveOutMB int) {
	if goos == "windows" {
		if ramTotalGB <= 0 {
			return 0, 0
		}
		deduction := hostfit.Host{
			RAMTotalGB:     ramTotalGB,
			RAMAvailableGB: ramAvailableAtInstallGB,
		}.OSMemoryDeductionGB()
		usable := (ramTotalGB - deduction) * 1024
		if usable <= 0 {
			return 0, 0
		}
		return min(usable, strixHaloUMACapMB), 0
	}
	if amdVRAMMB > 0 {
		c := minNonZero(amdVRAMMB, strixHaloUMACapMB)
		return c, c
	}
	// minNonZero treats 0 as "not a candidate", so without this guard a
	// failed RAM probe would return the ceiling — a host that measured
	// nothing would publish the largest budget this code can express.
	if ramTotalGB <= 0 {
		return 0, 0
	}
	heuristicMB := int(float64(ramTotalGB) * 0.75 * 1024)
	return minNonZero(heuristicMB, strixHaloUMACapMB), 0
}
