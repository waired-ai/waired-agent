package hardware

import "strings"

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

// strixHaloUsableVRAMMB computes the GPU-addressable memory budget for a
// Strix Halo UMA host, shared by the Linux and Windows profilers.
//
// When the driver/sysfs reports the BIOS carve-out size (amdVRAMMB > 0)
// that value — clamped to the BIOS UMA ceiling — is authoritative. This
// is the carve-out fix: on a box that fixes, say, 96 GB to the iGPU at
// the BIOS level, the OS-visible system RAM (ramTotalGB) is only the
// *leftover* (~31 GB), so a 75 %-of-RAM heuristic would wrongly clamp
// the budget to ~24 GB and hide most of the 96 GB pool. We must trust
// the carve-out reading and NOT min it against the heuristic.
//
// Only when no carve-out reading is available (amdVRAMMB == 0) do we
// fall back to the 75 %-of-RAM heuristic. That path is correct on a
// truly-unified host where ramTotalGB reports the whole shared pool
// (e.g. a registry walk that failed to surface qwMemorySize). Both
// branches are clamped to the ceiling.
func strixHaloUsableVRAMMB(amdVRAMMB, ramTotalGB int) int {
	if amdVRAMMB > 0 {
		return minNonZero(amdVRAMMB, strixHaloUMACapMB)
	}
	heuristicMB := int(float64(ramTotalGB) * 0.75 * 1024)
	return minNonZero(heuristicMB, strixHaloUMACapMB)
}
