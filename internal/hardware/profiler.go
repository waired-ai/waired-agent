// Package hardware detects local machine capabilities relevant to
// running LLM runtimes: OS / arch / CPU / RAM / GPU / installed engines.
//
// GPU detection composes vendor-specific detectors (see gpu.go and
// gpu_<vendor>.go). NVIDIA via `nvidia-smi` CSV, resolved through a
// chain rather than $PATH alone and backed by NVML / the OS device
// inventory so a service account with no PATH still sees the card
// (#67). AMD via `rocm-smi` CSV with a Windows registry fallback for
// hosts where Ollama supplies its own HIP runtime and the user has not
// installed the ROCm/HIP SDK separately. Apple Metal remains future,
// addable via the same VendorDetector seam.
package hardware

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Profile is a snapshot of a machine's relevant hardware/runtime state
// at a moment in time. JSON-serialisable so it can be returned by
// /waired/v1/inference/hardware verbatim.
type Profile struct {
	OS             string  `json:"os"`
	Arch           string  `json:"arch"`
	CPU            CPUInfo `json:"cpu"`
	RAMTotalGB     int     `json:"ram_total_gb"`
	RAMAvailableGB int     `json:"ram_available_gb"`

	// RAMAvailableAtInstallGB is the persisted available-memory
	// measurement (#568), injected via WithRAMAvailableAtInstall —
	// never probed here. The name is historical: since #835 the figure
	// is the highest reading taken at a daemon start with nothing
	// resident, not the one the installer happened to take. It is what
	// HostFit projects and what the broadcast HardwareSummary carries;
	// the live RAMAvailableGB above stays on the JSON endpoint for
	// diagnostics and is deliberately NOT used in fit decisions (a
	// live figure would count a resident model against the host
	// serving it). 0 means "no measurement": hostfit answers with its
	// OSMemoryAllowanceGB constant.
	RAMAvailableAtInstallGB int `json:"ram_available_at_install_gb,omitempty"`

	// RAMAvailableAtInstallMeasuredAt dates the figure above, RFC3339Nano,
	// injected alongside it via WithRAMAvailableAtInstall (#699). Empty
	// means no claim: no measurement was persisted, or the value came
	// from the operator/CI env seam, which supplies a number and not a
	// measurement. Carried verbatim like the value — it dates the
	// reading that produced the standing figure, not this snapshot.
	RAMAvailableAtInstallMeasuredAt string `json:"ram_available_at_install_measured_at,omitempty"`

	// GPUs are the GPUs the inference engine will run on, and the only
	// ones any fit rule, budget, host key or backend plan reads.
	// UnusedGPUs are the ones that were detected but that the engine
	// leaves off by default — today, integrated GPUs other than a CUDA
	// device or a ROCm gfx1151 (engine_gpus.go, waired-agent#1484).
	// DetectedGPUs returns both.
	GPUs         []GPU            `json:"gpus"`
	UnusedGPUs   []UnusedGPU      `json:"unused_gpus,omitempty"`
	Accelerators Accelerators     `json:"accelerators"`
	Storage      StorageInfo      `json:"storage"`
	Engines      InstalledEngines `json:"engines"`
	CollectedAt  time.Time        `json:"collected_at"`
	// Errors collects non-fatal detection failures so the caller can
	// surface "RAM detection unavailable" without losing the rest of
	// the profile.
	Errors []string `json:"errors,omitempty"`

	// UnifiedMemory flags hosts where GPU and CPU share physical RAM:
	// Apple Silicon and AMD Strix Halo today. The picker uses
	// UsableVRAMMB instead of GPUs[0].VRAMTotalMB on such hosts because
	// "total RAM" overstates the budget the GPU can actually wire down
	// (the OS reserves a chunk for itself).
	UnifiedMemory bool `json:"unified_memory,omitempty"`

	// UsableVRAMMB is the GPU-addressable upper bound after OS reserve
	// is excluded. On discrete GPUs it equals GPUs[0].VRAMTotalMB. On
	// Apple Silicon it's derived from `sysctl iogpu.wired_limit_mb`
	// (fallback: 75 % of RAMTotalGB). On a Linux Strix Halo it is the
	// AMD GPU's reported VRAM total, read via rocm-smi (fallback:
	// min(75 % of RAMTotalGB, 96 GB) per BIOS UMA / Vulkan caps). On a
	// Windows Strix Halo it is the OS-visible RAM minus the OS reserve,
	// clamped to the same ceiling — see strixHaloUMA for why the
	// carve-out reading is not used there.
	UsableVRAMMB int `json:"usable_vram_mb,omitempty"`

	// CarveOutVRAMMB is GPU memory reserved at the firmware level that
	// RAMTotalGB above EXCLUDES, so a capacity rule can add the two
	// without counting the same bytes twice (hostfit.TotalMemoryMB).
	//
	// Set only on Linux, from the AMD GPU's reported VRAM total (via
	// rocm-smi, which reads sysfs mem_info_vram_total internally). It
	// stays 0 on Apple Silicon, whose figure is synthesized from RAM by
	// the iogpu.wired_limit_mb sysctl or a 75 % fallback — a view INTO
	// system RAM, which adding would double-count — and 0 on Windows,
	// where the carve-out IS read but is not memory a model may occupy
	// in addition to RAM (waired-ai/waired-agent#863; strixHaloUMA
	// carries the measurement).
	CarveOutVRAMMB int `json:"carve_out_vram_mb,omitempty"`

	// MemoryBandwidthSpecGBs is the published PEAK read bandwidth of the
	// unified pool in GB/s, looked up from the chip name by
	// UnifiedMemoryBandwidthGBs. 0 on discrete and CPU-only hosts, and on
	// any unified part not yet in that table.
	//
	// A spec figure, so an UPPER bound on decode speed — which is what
	// lets the fit rules exclude a model for being too slow instead of
	// only annotating it (#251). Populated by the per-OS UMA hook, which
	// runs last and can therefore read CPU.Model.
	MemoryBandwidthSpecGBs float64 `json:"memory_bandwidth_spec_gbs,omitempty"`
}

// HostFit projects the profile onto the shared host-fit facts
// (proto/hostfit.Host). It is the agent's ONLY adapter into the fit
// rules, and the counterpart of hostfit.FromHardwareSummary on the
// control-plane side: both repositories decide "does this model fit
// this host" with one implementation, reached through one adapter each.
//
// Before proto/hostfit existed, the control plane re-derived the rules
// from the broadcast summary and drifted — it compared system RAM with
// no VRAM term at all, so a 128 GB host with a 24 GB card was offered a
// 62 GB model as its default while this package's own picker refused it
// (waired-ai/waired#942, waired-ai/waired-agent#228).
func (p Profile) HostFit() hostfit.Host {
	h := hostfit.Host{
		RAMTotalGB: p.RAMTotalGB,
		// The persisted figure, never the live RAMAvailableGB: a live
		// reading would count a resident model against the very host
		// serving it (#568).
		RAMAvailableGB:         p.RAMAvailableAtInstallGB,
		GPUCount:               len(p.GPUs),
		UnifiedMemory:          p.UnifiedMemory,
		UsableVRAMMB:           p.UsableVRAMMB,
		MemoryBandwidthSpecGBs: p.MemoryBandwidthSpecGBs,
		CarveOutVRAMMB:         p.CarveOutVRAMMB,
	}
	if len(p.GPUs) > 0 {
		h.VRAM0MB = p.GPUs[0].VRAMTotalMB
		h.VRAMAvailable0MB = p.GPUs[0].VRAMFreeMB
		h.GPUVendor = p.GPUs[0].Vendor
	}
	// The pool rule itself lives in hostfit so this adapter and
	// FromHardwareSummary cannot drift — the same reason every other
	// field here is a projection rather than a computation.
	devs := make([]hostfit.Device, 0, len(p.GPUs))
	for _, g := range p.GPUs {
		devs = append(devs, hostfit.Device{
			Vendor:          g.Vendor,
			VRAMTotalMB:     g.VRAMTotalMB,
			VRAMAvailableMB: g.VRAMFreeMB,
		})
	}
	h.VRAMPoolMB = hostfit.OllamaVRAMPoolMB(devs)
	return h
}

// GPUSummaries is the per-device half of the adapter above: the vendor,
// model, VRAM and compute capability of each GPU, in the shared wire
// shape.
//
// hostfit.Host deliberately collapses the device list — a fit decision
// needs a budget, not an inventory — but the vLLM sizing rules do need
// the inventory: tensor parallelism requires IDENTICAL devices and fp8
// KV requires an Ada-or-newer compute capability on every one of them.
// So the shared ladder takes both, and this is the agent's door into the
// second, the way HostFit is its door into the first (waired-agent#970).
//
// signer.HardwareGPUSummary rather than a type of our own because the
// control plane already holds exactly this, decoded off the wire: giving
// the shared code a shape only one side can produce would put the
// adapter back where it was.
func (p Profile) GPUSummaries() []signer.HardwareGPUSummary {
	if len(p.GPUs) == 0 {
		return nil
	}
	out := make([]signer.HardwareGPUSummary, 0, len(p.GPUs))
	for _, g := range p.GPUs {
		out = append(out, signer.HardwareGPUSummary{
			Model:       g.Model,
			VRAMTotalMB: g.VRAMTotalMB,
			VRAMFreeMB:  g.VRAMFreeMB,
			ComputeCap:  g.ComputeCap,
			Vendor:      g.Vendor,
		})
	}
	return out
}

// EffectiveVRAMMB returns the VRAM budget the picker should compare
// against Variant.MinVRAMMB. For UMA hosts that's UsableVRAMMB; for
// discrete-GPU hosts (and any host where the UMA path hasn't filled
// UsableVRAMMB) it falls back to the first GPU's VRAMTotalMB. Returns
// 0 only on CPU-only hosts.
//
// This is the SINGLE-DEVICE budget. Ollama sizing asks
// OllamaVRAMBudgetMB instead — see there.
func (p Profile) EffectiveVRAMMB() int {
	return p.HostFit().EffectiveVRAMMB()
}

// OllamaVRAMBudgetMB returns the VRAM budget ollama sizing should use:
// the cross-device pool on a multi-GPU host, EffectiveVRAMMB otherwise.
// It is the ollama counterpart of router.VLLMVRAMBudgetMB, and the
// figure every ollama residency, context-floor and serve-tuning
// calculation compares against — selection and serving have to size
// against the same budget or a model admitted because it pools across
// two cards gets a context window sized for one (#264).
func (p Profile) OllamaVRAMBudgetMB() int {
	return p.HostFit().OllamaVRAMBudgetMB()
}

type CPUInfo struct {
	Model string `json:"model,omitempty"`
	Cores int    `json:"cores"`
}

// GPU is the per-device record. Populated for NVIDIA via nvidia-smi
// and for AMD via rocm-smi (or the Windows registry fallback). Apple
// Metal devices remain absent until a Metal detector lands.
//
// VRAMTotalMB (not GB) is the canonical capacity unit so that the
// model picker can compare against variant.estimated_weight_gb*1024
// without integer-truncation bugs at GB boundaries (e.g. a 23.9 GB
// device must reject a 24 GB-min variant). A value of 0 means
// "unknown" (e.g. AMD adapter detected via the Windows registry
// fallback where VRAM is not readable without rocm-smi or DXGI).
type GPU struct {
	Vendor      string `json:"vendor"`
	Model       string `json:"model"`
	VRAMTotalMB int    `json:"vram_total_mb"`
	// VRAMFreeMB is how much of VRAMTotalMB the driver reported free,
	// 0 when it would not say. The ollama budget is sized against this
	// where it exists, because the engine places against free memory
	// while this repo used to size against the total — so a card also
	// driving a display was valued at more than it can lend
	// (waired-agent#69, docs/decisions/20260813/1120).
	//
	// Read ONCE per process, before this agent's engine holds any
	// weights, and frozen from then on — see Profiler.freezeVRAMFree.
	VRAMFreeMB    int    `json:"vram_free_mb,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	ComputeCap    string `json:"compute_cap,omitempty"`
	UUID          string `json:"uuid,omitempty"`

	// Integrated and IntegratedKnown are the detected answer to "are
	// this device's memory and the operating system's RAM two readings
	// of one physical pool?" — see internal/hardware/integrated.go for
	// the question, the per-OS readings, and why the answer is
	// three-state (waired-agent#459).
	//
	// They are a REPORT, not a budget. Nothing here decides a budget:
	// UnifiedMemory below still gates that, because a class carries a
	// budget rule with it and knowing a part is integrated does not by
	// itself say how much of the pool it may wire down. llama.cpp keeps
	// the same two switches apart for the same reason — its CUDA
	// backend reports the device type from a live cudaDeviceProp while
	// its scheduler reads a separately gated flag.
	//
	// The one decision the report DOES feed is which devices the engine
	// uses: a device known to be integrated is moved to
	// Profile.UnusedGPUs unless the engine uses such a device by default
	// (engine_gpus.go, waired-agent#1484). A known answer is required;
	// silence keeps the device in use.
	//
	// IntegratedKnown false means NO SOURCE ANSWERED, which is not the
	// same as "discrete" and must never be read as it: on this platform
	// set the silence is the common case (an NVIDIA part on Linux, an
	// Intel Mac, a host whose render node will not open), and reading
	// silence as "discrete" is the defect waired-agent#459 was opened
	// for.
	Integrated      bool `json:"integrated,omitempty"`
	IntegratedKnown bool `json:"integrated_known,omitempty"`

	// PCIID is the accelerator's PCI vendor:device pair, lowercase hex
	// ("1002:1586"), or "" on a part that is not on a PCI bus (Apple
	// Silicon) or whose pair could not be read.
	//
	// It is the one identity of a part that is the SAME on every
	// operating system — see internal/hardware/pciid.go for why the
	// model strings are not, and for why a pair is safe in a public
	// repository where a serial would not be. The catalog's provenance
	// records carry it beside the readable host key so an importer can
	// tell two machines apart without trusting a label
	// (waired-agent#1455).
	PCIID string `json:"pci_id,omitempty"`

	// GFXTarget is an AMD device's ISA target as the ROCm stack names it
	// ("gfx1151"), or "" where no source reported one. It is the key
	// ollama's own allowlist of integrated devices is written in, which
	// is why engineUsesByDefault reads it.
	GFXTarget string `json:"gfx_target,omitempty"`

	// CUDATotalMemMB is what the CUDA driver API says the device can
	// allocate (cuDeviceTotalMem), 0 where it was not asked — which is
	// everywhere but Windows (cuda_facts.go, waired-agent#1482). On a
	// single-pool part it is the pool, which can be smaller than RAM less
	// the OS reserve, so it caps the budget.
	CUDATotalMemMB int `json:"cuda_total_mem_mb,omitempty"`

	// GTTTotalMB is the system memory an AMD device may map through its
	// GART (amdgpu's mem_info_gtt_total), 0 where not read. Reported for
	// diagnosis; the budget reads KFDMemMB, which already folds it in.
	GTTTotalMB int `json:"gtt_total_mb,omitempty"`

	// KFDMemMB is the size of the memory bank the kernel's KFD driver
	// reports for an AMD device (/sys/class/kfd/.../mem_banks/0), which is
	// the pool the ROCm stack allocates from, 0 where KFD is absent. On a
	// discrete card it is the VRAM. On an APU it is the carve-out — or,
	// on Linux 6.15 and later when the GTT is the larger of the two, the
	// GTT (amdgpu's apu_prefer_gtt) — so it is the kernel's own answer to
	// how much of the shared pool the GPU can hold (waired-agent#1485).
	KFDMemMB int `json:"kfd_mem_mb,omitempty"`
}

// GPUSummary is the minimal per-device shape suitable for inclusion in
// inference-mesh broadcasts. Drops the operator-side metadata (driver,
// UUID) that other peers can't act on, keeping the fields that drive
// Phase 7 display ("peer X: RTX 4090, 24 GB").
//
// Vendor used to be dropped here for the same reason, but it is now
// carried: the control plane decides which serving engines and catalog
// models a device may be offered during onboarding, and that answer is
// vendor-dependent (vLLM is an NVIDIA path; AMD is served through
// Ollama's ROCm/Vulkan backends, waired#290). Model is documented as
// free-form and must not be parsed for such decisions, so publishing
// the token the detectors already produce is what keeps consumers
// honest.
//
// Defined in hardware (rather than in signer) so the hardware package
// stays the single source of truth for GPU shape and so signer keeps
// zero dependencies. The agent's inference probe translates this to
// signer.HardwareGPUSummary trivially (same field set, different
// owner package).
type GPUSummary struct {
	Model       string `json:"model"`
	VRAMTotalMB int    `json:"vram_total_mb,omitempty"`
	// VRAMFreeMB mirrors GPU.VRAMFreeMB: the frozen free reading the
	// ollama budget is sized against, 0 when the driver would not say
	// (waired-agent#69).
	VRAMFreeMB int    `json:"vram_free_mb,omitempty"`
	ComputeCap string `json:"compute_cap,omitempty"`
	Vendor     string `json:"vendor,omitempty"`
}

// GPUSummary returns the per-device subset of Profile.GPUs that's
// appropriate for inclusion in NetworkMap broadcasts. Returns a
// freshly-allocated slice (caller may mutate without affecting
// Profile state); returns nil for CPU-only hosts so the JSON shape
// stays compact.
func (p Profile) GPUSummary() []GPUSummary {
	if len(p.GPUs) == 0 {
		return nil
	}
	out := make([]GPUSummary, len(p.GPUs))
	for i, g := range p.GPUs {
		out[i] = GPUSummary{
			Model:       g.Model,
			VRAMTotalMB: g.VRAMTotalMB,
			VRAMFreeMB:  g.VRAMFreeMB,
			ComputeCap:  g.ComputeCap,
			Vendor:      g.Vendor,
		}
	}
	return out
}

// Accelerators reports framework availability. CUDA flips to true
// when at least one NVIDIA GPU is detected, ROCm when at least one
// AMD GPU is detected (rocm-smi or Windows registry path). Metal
// remains future via the same VendorDetector seam.
type Accelerators struct {
	CUDA  bool `json:"cuda"`
	ROCm  bool `json:"rocm"`
	Metal bool `json:"metal"`
}

type StorageInfo struct {
	CachePath      string `json:"cache_path"`
	CacheFreeBytes int64  `json:"cache_free_bytes"`
}

type EngineInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
}

type InstalledEngines struct {
	Ollama EngineInfo `json:"ollama"`
	VLLM   EngineInfo `json:"vllm"`
}

// Profiler builds Profile values, caching the most recent successful
// snapshot for ttl. Detection functions can be swapped out via the
// With* options for testing.
type Profiler struct {
	cachePath string
	ttl       time.Duration

	nowFn           func() time.Time
	osArchFn        func() (string, string)
	cpuFn           func(context.Context) CPUInfo
	ramFn           func(context.Context) (int, int, error)
	storageFn       func(context.Context, string) (int64, error)
	engineVersionFn func(context.Context, string) (bool, string)
	gpuFn           func(context.Context) ([]GPU, Accelerators, error)
	umaFn           func(context.Context, *Profile)
	integratedFn    integratedFrom
	persistedFn     func(pciID string) (integrated, ok bool)

	ramAtInstallGB         int
	ramAtInstallMeasuredAt string

	mu       sync.Mutex
	cached   *Profile
	cachedAt time.Time

	// vramFreeFirst is the FIRST free-VRAM reading this process took, by
	// device UUID (falling back to model+index when the driver gave no
	// UUID). See freezeVRAMFree for why it is frozen rather than
	// re-read.
	vramFreeFirst map[string]int
}

// freezeVRAMFree keeps the first free-VRAM reading this process took and
// replays it on every later profile, instead of letting the figure move.
//
// The reason is the one signer.HardwareSummary.RAMAvailableGB states for
// its own quantity, in the same words: a live figure "would count a
// resident model against the very host that serves it" (waired-agent#568).
// This profile is re-sampled on a TTL, including long after the agent's
// engine has loaded weights. A free reading taken then EXCLUDES those
// weights, so the budget would shrink, the next re-tune would size
// against the smaller budget, and the reading after that would be
// smaller again — a spiral driven entirely by the host's own success at
// serving.
//
// Freezing at the first reading is what makes the number mean "what else
// on this machine holds VRAM", which is the quantity the budget wants
// (docs/decisions/20260813/1120). The daemon takes that reading while
// building its first profile, before the engine subsystem starts.
//
// Consequence, stated rather than hidden: a machine that frees VRAM
// later (the user closes a game) does not get the larger budget until
// the agent restarts. That is the same trade #568 accepted, and the
// safe direction — the stale figure is the pessimistic one.
//
// Keyed by UUID because device ORDER is not guaranteed stable across
// enumerations, and a free figure replayed onto the wrong card would be
// worse than none. A device with no UUID falls back to model+index,
// which is stable in practice for the single-vendor lists this reads.
func (p *Profiler) freezeVRAMFree(gpus []GPU) []GPU {
	key := func(g GPU, i int) string {
		if g.UUID != "" {
			return g.UUID
		}
		return fmt.Sprintf("%d/%s", i, g.Model)
	}
	if p.vramFreeFirst == nil {
		p.vramFreeFirst = make(map[string]int, len(gpus))
	}
	out := make([]GPU, len(gpus))
	copy(out, gpus)
	for i := range out {
		k := key(out[i], i)
		if seen, ok := p.vramFreeFirst[k]; ok {
			out[i].VRAMFreeMB = seen
			continue
		}
		// Only a real reading is remembered. A detector that could not
		// read free memory this time must not pin 0 forever — the next
		// profile may come from a source that can.
		if out[i].VRAMFreeMB > 0 {
			p.vramFreeFirst[k] = out[i].VRAMFreeMB
		}
	}
	return out
}

// Option mutates a freshly-constructed Profiler.
type Option func(*Profiler)

func WithTTL(ttl time.Duration) Option { return func(p *Profiler) { p.ttl = ttl } }
func WithNow(fn func() time.Time) Option {
	return func(p *Profiler) { p.nowFn = fn }
}
func WithOSArch(fn func() (string, string)) Option {
	return func(p *Profiler) { p.osArchFn = fn }
}
func WithCPU(fn func(context.Context) CPUInfo) Option {
	return func(p *Profiler) { p.cpuFn = fn }
}
func WithRAM(fn func(context.Context) (int, int, error)) Option {
	return func(p *Profiler) { p.ramFn = fn }
}

// WithRAMAvailableAtInstall injects the persisted available-memory
// figure and the timestamp that dates it (#568, #699). The profiler
// carries both verbatim into every Profile it builds — they are facts
// about a past measurement, not about this snapshot, so re-detection
// never changes them.
//
// One option for the pair rather than two, so a caller cannot supply a
// value dated by some other measurement. measuredAt may be empty on its
// own (no persisted record, or the env seam, which is an override rather
// than a measurement); a date without a value is not constructible here.
func WithRAMAvailableAtInstall(gb int, measuredAt string) Option {
	return func(p *Profiler) {
		p.ramAtInstallGB = gb
		p.ramAtInstallMeasuredAt = measuredAt
	}
}

// ProbeRAM reads total and available system RAM in whole GiB using the
// same per-OS reader the profiler uses (linux /proc/meminfo, windows
// GlobalMemoryStatusEx, darwin sysctl+vm_stat). It exists for the
// install-time measurement (#568), which needs the reading without a
// full profile.
func ProbeRAM(ctx context.Context) (totalGB, availGB int, err error) {
	return defaultRAM(ctx)
}
func WithStorage(fn func(context.Context, string) (int64, error)) Option {
	return func(p *Profiler) { p.storageFn = fn }
}
func WithEngineVersion(fn func(context.Context, string) (bool, string)) Option {
	return func(p *Profiler) { p.engineVersionFn = fn }
}
func WithGPU(fn func(context.Context) ([]GPU, Accelerators, error)) Option {
	return func(p *Profiler) { p.gpuFn = fn }
}

// WithUMA injects the UMA / usable-VRAM detector. The detector mutates
// the Profile in place (sets UnifiedMemory and UsableVRAMMB) after the
// rest of the profile has been built; it can therefore inspect
// previously-detected fields like GPUs and CPU.Model when deciding.
func WithUMA(fn func(context.Context, *Profile)) Option {
	return func(p *Profiler) { p.umaFn = fn }
}

// WithIntegratedDetector injects the per-device "is this one pool?"
// reading (waired-agent#459), so a test can drive the merge and the
// fields it lands in without the OS the real reading needs.
//
// Deliberately separate from WithUMA even though both concern the same
// hardware property. The UMA hook settles a BUDGET and must stay the one
// place that does; this one settles a REPORTED FACT and settles nothing
// else. Folding them together is how a report starts deciding things.
func WithIntegratedDetector(fn integratedFrom) Option {
	return func(p *Profiler) { p.integratedFn = fn }
}

// WithPersistedIntegration injects a reading taken earlier, under
// privileges this process does not have, keyed by the accelerator's PCI
// vendor:device pair (waired-agent#459).
//
// The daemon runs as a service user that cannot open /dev/dri/renderD*,
// so on Linux the live reading is almost always "unknown". `sudo waired
// init` can open it, takes the reading once and persists it; this is how
// it gets back. The pair is the key so that swapping the card leaves the
// old entry matching nothing, rather than describing hardware that is
// gone.
//
// A live reading still WINS where there is one: it is current, and the
// merge rule already says a source that knows overrides. The persisted
// answer is a floor, not a ceiling.
func WithPersistedIntegration(fn func(pciID string) (integrated, ok bool)) Option {
	return func(p *Profiler) { p.persistedFn = fn }
}

// NewProfiler returns a Profiler that caches results for 30s by default
// (per spec §6) and uses real OS detection. cachePath is the directory
// whose free-space we report (typically the model cache root).
func NewProfiler(cachePath string, opts ...Option) *Profiler {
	p := &Profiler{
		cachePath:       cachePath,
		ttl:             30 * time.Second,
		nowFn:           time.Now,
		osArchFn:        defaultOSArch,
		cpuFn:           defaultCPU,
		ramFn:           defaultRAM,
		storageFn:       defaultStorage,
		engineVersionFn: defaultEngineVersion,
		gpuFn:           defaultGPU,
		umaFn:           defaultUMA,
		integratedFn:    integratedFromOS,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Profile returns the current hardware snapshot, re-detecting if the
// previous snapshot is older than the TTL.
func (p *Profiler) Profile(ctx context.Context) Profile {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.nowFn()
	if p.cached != nil && now.Sub(p.cachedAt) < p.ttl {
		return *p.cached
	}

	osName, arch := p.osArchFn()
	prof := Profile{
		OS:                              osName,
		Arch:                            arch,
		CPU:                             p.cpuFn(ctx),
		GPUs:                            []GPU{},
		Storage:                         StorageInfo{CachePath: p.cachePath},
		CollectedAt:                     now,
		RAMAvailableAtInstallGB:         p.ramAtInstallGB,
		RAMAvailableAtInstallMeasuredAt: p.ramAtInstallMeasuredAt,
	}

	if p.gpuFn != nil {
		// A detector may return BOTH non-nil err and valid gpus/accel
		// (e.g. AMD registry fallback returns adapters but warns that
		// VRAM is unknown). Surface the error to Profile.Errors but
		// always propagate whatever data was returned — composeDetectors
		// guarantees the GPU slice contains every successful vendor's
		// results regardless of any single vendor's failure.
		gpus, accel, err := p.gpuFn(ctx)
		if err != nil {
			prof.Errors = append(prof.Errors, fmt.Sprintf("gpu: %v", err))
		}
		if gpus != nil {
			prof.GPUs = p.freezeVRAMFree(gpus)
		}
		prof.Accelerators = accel
	}

	total, avail, err := p.ramFn(ctx)
	if err != nil {
		prof.Errors = append(prof.Errors, fmt.Sprintf("ram: %v", err))
	} else {
		prof.RAMTotalGB = total
		prof.RAMAvailableGB = avail
	}

	free, err := p.storageFn(ctx, p.cachePath)
	if err != nil {
		prof.Errors = append(prof.Errors, fmt.Sprintf("storage: %v", err))
	} else {
		prof.Storage.CacheFreeBytes = free
	}

	if installed, ver := p.engineVersionFn(ctx, "ollama"); installed {
		prof.Engines.Ollama = EngineInfo{Installed: true, Version: ver}
	}
	if installed, ver := p.engineVersionFn(ctx, "vllm"); installed {
		prof.Engines.VLLM = EngineInfo{Installed: true, Version: ver}
	}

	// The PCI pair first, where the detector did not already have it in
	// hand (Windows reads MatchingDeviceId for its vendor filter and
	// fills it there; Linux needs a sysfs pass; Apple Silicon has no PCI
	// bus). It comes first because the persisted reading below is keyed
	// by it.
	cudaDevs, cudaOK := cudaDevicesFromOS()
	for i := range prof.GPUs {
		if prof.GPUs[i].PCIID == "" {
			prof.GPUs[i].PCIID = pciIDFromOS(&prof, i)
		}
		// NVIDIA's own figure for what the device can allocate, where
		// the driver API is reachable (Windows) and the pairing certain.
		if cudaOK {
			if d, ok := cudaFactsFor(&prof, i, cudaDevs); ok {
				prof.GPUs[i].CUDATotalMemMB = d.totalMB
			}
		}
		// An AMD part whose detector read no ISA target (Windows always;
		// Linux without KFD) is named from its device ID (amd_pci_gfx.go).
		if prof.GPUs[i].GFXTarget == "" && strings.EqualFold(prof.GPUs[i].Vendor, "amd") {
			prof.GPUs[i].GFXTarget = amdGFXTargetForPCIID(prof.GPUs[i].PCIID)
		}
	}
	// The per-device "is this one pool?" reading runs before the UMA
	// hook, so the hook could consult it — and so that the fact is
	// recorded even on the hosts where the hook declines to act on it,
	// which is most of them (waired-agent#459).
	//
	// Persisted, then the vendor axis, then the live per-OS reading —
	// oldest evidence first, so each later source may override an
	// earlier one rather than the other way round. The vendor axis sits
	// in the middle because what the VENDOR published about a part is
	// true wherever that part is plugged in, but a reading taken on THIS
	// machine is better still. It is not injectable: it is untagged and
	// pure, so a test drives it by naming a device rather than by
	// swapping it out.
	for i := range prof.GPUs {
		got := integration{
			integrated: prof.GPUs[i].Integrated,
			known:      prof.GPUs[i].IntegratedKnown,
		}
		if p.persistedFn != nil {
			if yes, ok := p.persistedFn(prof.GPUs[i].PCIID); ok {
				got = got.merge(integratedKnown(yes))
			}
		}
		got = got.merge(integratedFromVendor(&prof, i))
		if p.integratedFn != nil {
			got = got.merge(p.integratedFn(&prof, i))
		}
		prof.GPUs[i].Integrated, prof.GPUs[i].IntegratedKnown = got.integrated, got.known
	}

	// Then set aside the GPUs the engine will not run on. Here, after
	// every reading above has been taken over every detected device (the
	// persisted record needs them all), and before anything that turns
	// the list into a budget, a class or a key.
	prof.GPUs, prof.UnusedGPUs = partitionForEngine(&prof)

	// UMA detection runs after so it can inspect GPUs / RAM / CPU.Model
	// (used by the Linux Strix Halo path) without re-walking sysfs.
	if p.umaFn != nil {
		p.umaFn(ctx, &prof)
	}
	// Then the pool's peak bandwidth, which is a pure function of the
	// facts the hook just settled. Deliberately here rather than inside
	// each per-OS hook: the lookup is identical on all three, and three
	// copies of an identical rule is how the OSes drift apart
	// (CLAUDE.md §Cross-OS parity). Untagged, so it is reachable from a
	// test on any host.
	prof.MemoryBandwidthSpecGBs = unifiedBandwidthFor(&prof)

	p.cached = &prof
	p.cachedAt = now
	return prof
}

// FreeDiskBytes reports the free space (bytes available to an
// unprivileged process) on the filesystem backing path, using the same
// per-OS probe (statfs / GetDiskFreeSpaceEx) that populates
// Profile.Storage.CacheFreeBytes. It is the install-time disk pre-flight
// primitive (#517): the bundled-model download target — e.g.
// <state-dir>/runtimes/ollama/models — may not exist yet, and the
// underlying probe needs a real path, so FreeDiskBytes walks up to the
// nearest existing ancestor directory before probing (the free space of
// the enclosing filesystem is the same). Returns an error only when no
// ancestor down to the root can be stat'd.
func FreeDiskBytes(path string) (int64, error) {
	dir := nearestExistingDir(path)
	if dir == "" {
		return 0, fmt.Errorf("hardware: no existing ancestor directory for %q", path)
	}
	return defaultStorage(context.Background(), dir)
}

// nearestExistingDir walks path up toward the filesystem root, returning
// the first component that exists so the per-OS storage probe has a real
// path to query. Returns "" only when even the root cannot be stat'd.
func nearestExistingDir(path string) string {
	if path == "" {
		path = "."
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			// Reached the root; if it still doesn't stat there is nothing
			// queryable.
			return ""
		}
		path = parent
	}
}

// --- default detection implementations ---

func defaultOSArch() (string, string) {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "arm64"
	}
	return runtime.GOOS, arch
}

// defaultCPU / defaultRAM / defaultStorage live in profiler_<os>.go.
// Each OS supplies its own implementation: Linux reads /proc, Windows
// uses GlobalMemoryStatusEx + the CentralProcessor registry key, and
// non-Windows non-Linux falls back to a stub that surfaces an Error
// via Profile.Errors but doesn't block the rest of the profile.

func parseProcMeminfo(r io.Reader) (totalGB, availGB int, err error) {
	var totalKB, availKB int64
	gotTotal, gotAvail := false, false
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := s.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseMeminfoKB(line)
			gotTotal = true
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = parseMeminfoKB(line)
			gotAvail = true
		}
		if gotTotal && gotAvail {
			break
		}
	}
	if err := s.Err(); err != nil {
		return 0, 0, err
	}
	if !gotTotal {
		return 0, 0, errors.New("MemTotal not found in /proc/meminfo")
	}
	totalGB = bytesToGBRounded(uint64(totalKB) * 1024)
	// No MemAvailable line (pre-3.14 kernels, and sandboxes that
	// synthesize a partial /proc/meminfo) means "not measured", and it
	// must not be returned as the number 0 — the persisted record floors
	// a probe's answer at 1 so a truthfully exhausted host cannot read as
	// unmeasured (cmd/waired-agent/host_memory.go, the 2026-08-08 owner
	// rulings on #568), which would turn a missing line into "1 GB free"
	// and collapse hostfit's capacity to nothing. Report it the way the
	// darwin probe already reports its own parse failures: available ==
	// total, which Host.OSMemoryDeductionGB lands on the constant floor.
	if !gotAvail {
		return totalGB, totalGB, nil
	}
	availGB = bytesToGBRounded(uint64(availKB) * 1024)
	return totalGB, availGB, nil
}

// bytesToGBRounded converts a byte count to whole GiB, rounding to the
// nearest GiB instead of truncating. Hardware-reserved memory makes a
// machine's usable RAM report slightly below its marketed size — a 32 GB
// box exposes ~31.9 GiB — and flooring turned that into 31, spuriously
// failing a 32 GB fit threshold (#61). Rounding reports the honest
// marketed capacity. Unit is GiB (1<<30), matching what the catalog's
// min_ram_gb thresholds are compared against. Shared by all three OS RAM
// probes (linux /proc/meminfo, windows GlobalMemoryStatusEx, darwin
// hw.memsize) so the reported number is consistent cross-platform.
func bytesToGBRounded(b uint64) int {
	const gib = 1 << 30
	return int((b + gib/2) / gib)
}

func parseMeminfoKB(line string) int64 {
	// "MemTotal:       65856900 kB"
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[1], 10, 64)
	return n
}

// defaultEngineVersion probes an engine NAMED ON $PATH, and is only the
// fallback for Profilers built without an injected resolver — the CLI's
// one-shot hardware reads, which do not consult Profile.Engines at all.
//
// It is deliberately not the daemon's answer: waired's own engine lives
// under the state dir and is off $PATH by design, so this reports no
// version on exactly the hosts waired provisioned (#238, the #179
// predicate one layer down). The daemon injects
// engineVersionOnHost (cmd/waired-agent/engine_resolve.go) via
// WithEngineVersion, which resolves the binary the same way it resolves
// the one it spawns.
func defaultEngineVersion(ctx context.Context, binary string) (bool, string) {
	return EngineVersionAt(ctx, binary, binary)
}

// EngineVersionAt runs `<path> --version` and extracts a version
// string. A non-zero exit, or a path that cannot be executed, is
// treated as "not installed"; an installed engine whose output does not
// parse reports (true, "").
//
// engine and path are separate arguments on purpose. The parse keys off
// the ENGINE KIND, while the executable is wherever the caller resolved
// it — for a waired-managed install that is
// <state-dir>/runtimes/<engine>/bin/..., a path whose basename
// ParseEngineVersion could not switch on reliably.
func EngineVersionAt(ctx context.Context, engine, path string) (bool, string) {
	if path == "" {
		return false, ""
	}
	cctx, cancel := context.WithTimeout(ctx, engineVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").CombinedOutput()
	if err != nil {
		return false, ""
	}
	return true, ParseEngineVersion(engine, string(out))
}

// engineVersionTimeout bounds the `--version` probe. The two call sites
// this helper unified had disagreed (3 s here, 5 s in internal/setup's
// copy); 5 s wins because a timeout is indistinguishable from "not
// installed" downstream, and a cold binary on a Windows host doing a
// first-exec scan is a real way to spend three seconds.
const engineVersionTimeout = 5 * time.Second

// ParseEngineVersion isolates `<engine> --version` line parsing so it can be
// unit-tested without invoking the real binaries, and reused by callers such
// as internal/setup's Ollama detection. For ollama it keys off the
// "ollama version is " marker, which skips the "Warning: could not connect to
// a running Ollama instance" line the CLI prints when the server isn't up —
// the naive "last token of the first line" approach returned "instance" there
// and mis-flagged a perfectly good engine as unsupported.
//
// Which marker appears is decided by whether a server answers, not by the
// engine's version: with one up the CLI reports the SERVER's version as
// "ollama version is X"; with none it reports its own as "Warning: client
// version is X". Reading only the first meant a stopped engine had no
// version anywhere in the product (#826) — including the version this
// package's own callers compare against OllamaPinnedVersion. The server
// line is still preferred, so this can only fill in an answer that was
// empty.
func ParseEngineVersion(binary, output string) string {
	clientVersion := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch binary {
		case "ollama":
			// Format: "ollama version is X.Y.Z"
			const marker = "ollama version is "
			if i := strings.Index(line, marker); i >= 0 {
				return strings.TrimSpace(line[i+len(marker):])
			}
			// Format: "Warning: client version is X.Y.Z" — printed
			// instead of the above when no server is answering. Held
			// rather than returned so a server line later in the
			// output still wins.
			const clientMarker = "client version is "
			if i := strings.Index(line, clientMarker); i >= 0 && clientVersion == "" {
				clientVersion = strings.TrimSpace(line[i+len(clientMarker):])
			}
		case "vllm":
			// `vllm --version` (recent versions) prints just the
			// version string on its own line, e.g. "0.6.3.post1".
			if !strings.HasPrefix(line, "Warning") && !strings.HasPrefix(line, "WARNING") {
				return line
			}
		}
	}
	return clientVersion
}
