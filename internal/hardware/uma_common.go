package hardware

import (
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
// falls back to the 75 %-of-RAM heuristic. See waired-ai/waired-agent#868.
func strixHaloUMA(goos string, amdVRAMMB, ramTotalGB, ramAvailableAtInstallGB int) (usableVRAMMB, carveOutMB int) {
	if goos == "windows" {
		return poolMinusOSReserveMB(ramTotalGB, ramAvailableAtInstallGB, strixHaloUMACapMB), 0
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

// applyUnifiedBudget is the body of defaultUMA on every platform whose
// budget is arithmetic rather than a sysctl — Linux and Windows today.
//
// It is untagged and takes goos for the reason unifiedBudgetFor does,
// and for one more: a test that wants to exercise the REAL rule has to
// be able to run it on whatever host the test is running on. macOS
// answers this question from a sysctl and short-circuits on arm64
// before any of this is reached, so a test that called the host's own
// defaultUMA would be testing macOS on a Mac and this rule everywhere
// else — passing in both places while only ever checking one of them.
func applyUnifiedBudget(goos string, p *Profile) {
	usable, carveOut, ok := unifiedBudgetFor(goos, p)
	if !ok {
		return
	}
	p.UnifiedMemory = true
	p.UsableVRAMMB, p.CarveOutVRAMMB = usable, carveOut
}

// poolMinusOSReserveMB is the budget on a host whose GPU-addressable
// memory IS system RAM: everything the operating system reports, less
// what the operating system itself keeps.
//
// The deduction comes from hostfit rather than being re-derived here so
// that the figure the budget is sized with is the same one the capacity
// gate will subtract — the reason strixHaloUMA's Windows branch already
// took it from there (waired-agent#568).
//
// capMB is an architectural ceiling on what the GPU may address, in MB;
// 0 means there is none. It is a property of the platform, not of the
// machine: Strix Halo on Windows has one (the BIOS UMA ceiling) and the
// NVIDIA single-pool parts do not, because CUDA there addresses the
// whole pool. It is NOT a place to put a per-machine budget constant —
// the owner ruling of 2026-09-20 refused those
// (docs/decisions/20260920/2100-memory-estimate-stays-failure-made-safe.md:
// "予算を切り詰めるのではなくて、例外処理を充実させる").
//
// Returns 0 when RAM is unknown or the OS would take all of it. 0 means
// "no budget to publish", and every caller treats it that way.
func poolMinusOSReserveMB(ramTotalGB, ramAvailableAtInstallGB, capMB int) int {
	if ramTotalGB <= 0 {
		return 0
	}
	deduction := hostfit.Host{
		RAMTotalGB:     ramTotalGB,
		RAMAvailableGB: ramAvailableAtInstallGB,
	}.OSMemoryDeductionGB()
	usable := (ramTotalGB - deduction) * 1024
	if usable <= 0 {
		return 0
	}
	if capMB > 0 {
		return min(usable, capMB)
	}
	return usable
}

// firstAMDVRAMMB is the first AMD device's reported VRAM total, or 0.
func firstAMDVRAMMB(p *Profile) int {
	for _, g := range p.GPUs {
		if strings.EqualFold(g.Vendor, "amd") && g.VRAMTotalMB > 0 {
			return g.VRAMTotalMB
		}
	}
	return 0
}

// unifiedBudgetFor is the whole of the UMA POLICY for the two operating
// systems that share one: given a profile the detectors have already
// filled in, does this host have one pool, and if so what may the GPU
// wire down? ok=false leaves UnifiedMemory alone, which is today's
// behaviour for every host no rule below covers.
//
// # Why this is untagged, and takes goos
//
// Linux and Windows answered this in two build-tagged copies of nearly
// the same function, and the copies had already drifted — Windows flips
// the flag off the CPU string alone while Linux additionally requires a
// real AMD VRAM reading. Adding a third vendor to two copies is how that
// drift compounds, so the rule moves here and the platform difference
// becomes an argument, which is the shape CLAUDE.md §Cross-OS parity
// asks for and the one strixHaloUMA already uses. macOS keeps its own
// hook: its budget comes from a sysctl, not from arithmetic.
//
// # The rules are per (OS, vendor), not per machine
//
// That distinction is the point of waired-agent#1455. The table below
// has one row per way a pool is bounded, and a machine nobody has heard
// of flows through whichever row its vendor uses without anyone editing
// anything:
//
//	NVIDIA, any OS   the pool IS system RAM, so the budget is RAM less
//	                 the OS reserve, capped by the pool CUDA itself
//	                 reports where it can be asked (Windows,
//	                 cuDeviceTotalMem, #1482) and uncapped where it
//	                 cannot, and the carve-out is 0 — a GB10
//	                 reports no separate framebuffer at all, and an N1X
//	                 reports one that is a slice of RAM rather than an
//	                 addition to it.
//	AMD, Windows     RAM less the OS reserve, clamped to the BIOS UMA
//	                 ceiling; carve-out 0 (waired-agent#863, decision
//	                 20260820/0005).
//	AMD, Linux       the carve-out reading is the budget AND is
//	                 additive; its absence falls back to 75 % of RAM.
//	                 Unmeasured and deliberately unchanged (#868).
//
// # Why NVIDIA is asked first
//
// It is asked off a DETECTED fact (GPU.Integrated, which reached the
// profile through the asymmetric merge in integrated.go) while the AMD
// rows are asked off a family name. A detected fact should not lose to a
// string match. In practice the two cannot both fire — a host's GPUs[0]
// has one vendor — so the order documents the precedence rather than
// resolving a live conflict.
//
// # What this does NOT generalise
//
// Linux AMD APUs other than Strix Halo now REPORT integrated (the
// amdgpu FUSION bit reaches the profile through `sudo waired init`), and
// this function still does not act on it. That is not an oversight: the
// Linux AMD budget rule is the carve-out reading, which nothing in this
// repository has measured on any APU but the reference host, and a
// budget published without a measurement behind it is the failure mode
// #1453 and decision 20260920/2100 are about. Widening it is a
// measurement task, not a design one.
func unifiedBudgetFor(goos string, p *Profile) (usableVRAMMB, carveOutMB int, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	if nvidiaUnifiedHost(p) {
		// The pool CUDA reports, where it was asked (Windows), caps the
		// budget: on the RTX Spark N1X it is 45.4 GiB against 54.2 GiB of
		// RAM, so RAM less the reserve would promise ~10 % more than the
		// GPU can allocate (#1482). 0 means not asked, and no cap.
		return poolMinusOSReserveMB(p.RAMTotalGB, p.RAMAvailableAtInstallGB, p.GPUs[0].CUDATotalMemMB), 0, true
	}
	if !IsStrixHaloAPU(p.CPU.Model) {
		return 0, 0, false
	}
	amdVRAMMB := firstAMDVRAMMB(p)
	if goos != "windows" && amdVRAMMB == 0 {
		// Linux classifies a Strix Halo only on a real reading. Without
		// rocm-smi the iGPU is invisible there, and the budget branch
		// below would answer from the 75 % heuristic for a host whose
		// GPU was never enumerated at all.
		return 0, 0, false
	}
	u, c := strixHaloUMA(goos, amdVRAMMB, p.RAMTotalGB, p.RAMAvailableAtInstallGB)
	return u, c, true
}
