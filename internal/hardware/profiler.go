// Package hardware detects local machine capabilities relevant to
// running LLM runtimes: OS / arch / CPU / RAM / GPU / installed engines.
//
// GPU detection composes vendor-specific detectors (see gpu.go and
// gpu_<vendor>.go). NVIDIA via `nvidia-smi` CSV. AMD via `rocm-smi`
// CSV with a Windows registry fallback for hosts where Ollama
// supplies its own HIP runtime and the user has not installed the
// ROCm/HIP SDK separately. Apple Metal remains future, addable via
// the same VendorDetector seam.
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
)

// Profile is a snapshot of a machine's relevant hardware/runtime state
// at a moment in time. JSON-serialisable so it can be returned by
// /waired/v1/inference/hardware verbatim.
type Profile struct {
	OS             string           `json:"os"`
	Arch           string           `json:"arch"`
	CPU            CPUInfo          `json:"cpu"`
	RAMTotalGB     int              `json:"ram_total_gb"`
	RAMAvailableGB int              `json:"ram_available_gb"`
	GPUs           []GPU            `json:"gpus"`
	Accelerators   Accelerators     `json:"accelerators"`
	Storage        StorageInfo      `json:"storage"`
	Engines        InstalledEngines `json:"engines"`
	CollectedAt    time.Time        `json:"collected_at"`
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
	// (fallback: 75 % of RAMTotalGB). On Strix Halo it comes from
	// `/sys/class/drm/card*/device/mem_info_vram_total` (fallback:
	// min(75 % of RAMTotalGB, 96 GB) per BIOS UMA / Vulkan caps).
	UsableVRAMMB int `json:"usable_vram_mb,omitempty"`
}

// EffectiveVRAMMB returns the VRAM budget the picker should compare
// against Variant.MinVRAMMB. For UMA hosts that's UsableVRAMMB; for
// discrete-GPU hosts (and any host where the UMA path hasn't filled
// UsableVRAMMB) it falls back to the first GPU's VRAMTotalMB. Returns
// 0 only on CPU-only hosts.
func (p Profile) EffectiveVRAMMB() int {
	if p.UnifiedMemory && p.UsableVRAMMB > 0 {
		return p.UsableVRAMMB
	}
	if len(p.GPUs) > 0 {
		return p.GPUs[0].VRAMTotalMB
	}
	return 0
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
	Vendor        string `json:"vendor"`
	Model         string `json:"model"`
	VRAMTotalMB   int    `json:"vram_total_mb"`
	DriverVersion string `json:"driver_version,omitempty"`
	ComputeCap    string `json:"compute_cap,omitempty"`
	UUID          string `json:"uuid,omitempty"`
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
	ComputeCap  string `json:"compute_cap,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
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

	mu       sync.Mutex
	cached   *Profile
	cachedAt time.Time
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
		OS:          osName,
		Arch:        arch,
		CPU:         p.cpuFn(ctx),
		GPUs:        []GPU{},
		Storage:     StorageInfo{CachePath: p.cachePath},
		CollectedAt: now,
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
			prof.GPUs = gpus
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

	// UMA detection runs last so it can inspect GPUs / RAM / CPU.Model
	// (used by the Linux Strix Halo path) without re-walking sysfs.
	if p.umaFn != nil {
		p.umaFn(ctx, &prof)
	}

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
	if gotAvail {
		availGB = bytesToGBRounded(uint64(availKB) * 1024)
	}
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

// defaultEngineVersion runs `<binary> --version` and extracts a
// version string. A non-zero exit or missing binary is treated as
// "not installed".
func defaultEngineVersion(ctx context.Context, binary string) (bool, string) {
	if _, err := exec.LookPath(binary); err != nil {
		return false, ""
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, binary, "--version").CombinedOutput()
	if err != nil {
		return false, ""
	}
	ver := ParseEngineVersion(binary, string(out))
	return true, ver
}

// ParseEngineVersion isolates `<engine> --version` line parsing so it can be
// unit-tested without invoking the real binaries, and reused by callers such
// as internal/setup's Ollama detection. For ollama it keys off the
// "ollama version is " marker, which skips the "Warning: could not connect to
// a running Ollama instance" line the CLI prints when the server isn't up —
// the naive "last token of the first line" approach returned "instance" there
// and mis-flagged a perfectly good engine as unsupported.
func ParseEngineVersion(binary, output string) string {
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
		case "vllm":
			// `vllm --version` (recent versions) prints just the
			// version string on its own line, e.g. "0.6.3.post1".
			if !strings.HasPrefix(line, "Warning") && !strings.HasPrefix(line, "WARNING") {
				return line
			}
		}
	}
	return ""
}
