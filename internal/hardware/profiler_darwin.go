//go:build darwin

package hardware

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CPU detection prefers sysctl `machdep.cpu.brand_string` ("Apple M4" on
// modern Apple Silicon macOS, "Intel(R) Core(TM)…" on Intel). When that
// is empty (older Apple Silicon macOS did not expose it) we fall back to
// the friendly chip name from `system_profiler SPHardwareDataType`
// before the raw `hw.model` board code ("Mac16,10"), so the reported
// model stays human-readable. Cores come from the runtime package —
// sysctl `hw.ncpu` would give the same value and requires an extra
// subprocess.
func defaultCPU(ctx context.Context) CPUInfo {
	info := CPUInfo{Cores: runtime.NumCPU()}
	if model, err := sysctlString(ctx, "machdep.cpu.brand_string"); err == nil && model != "" {
		info.Model = model
		return info
	}
	if name := appleChipName(ctx); name != "" {
		info.Model = name
		return info
	}
	if model, err := sysctlString(ctx, "hw.model"); err == nil && model != "" {
		info.Model = model
	}
	return info
}

// appleChipName returns the friendly chip/CPU name from `system_profiler
// SPHardwareDataType -json` (e.g. "Apple M4", "Intel Core i7"), or "" on
// any failure. Mirrors the parse pattern in gpu_apple_darwin.go.
func appleChipName(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "system_profiler", "SPHardwareDataType", "-json").Output()
	if err != nil {
		return ""
	}
	return parseSPHardwareChip(out)
}

// parseSPHardwareChip extracts the chip/CPU name from `system_profiler
// SPHardwareDataType -json`: `chip_type` on Apple Silicon, `cpu_type` on
// Intel. Returns "" when neither is present or the JSON is malformed.
func parseSPHardwareChip(out []byte) string {
	var doc struct {
		SPHardwareDataType []map[string]any `json:"SPHardwareDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return ""
	}
	for _, d := range doc.SPHardwareDataType {
		if v, ok := d["chip_type"].(string); ok && v != "" {
			return v
		}
		if v, ok := d["cpu_type"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// RAM detection on macOS uses sysctl `hw.memsize` for the total. macOS
// exposes no single "available" sysctl, so we approximate it from the
// reclaimable page classes reported by `vm_stat` (free + inactive +
// speculative + purgeable) — a cgo-free read that matches the existing
// subprocess pattern. On any parse failure RAMAvailableGB falls back to
// RAMTotalGB (the prior conservative behaviour); the value is also
// clamped to ≤ total. The Auto Selector only uses RAMAvailableGB for
// soft warnings, not fit decisions, so this is a strict accuracy
// improvement with no fit-decision risk.
func defaultRAM(ctx context.Context) (int, int, error) {
	total, err := sysctlUint64(ctx, "hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	totalGB := int(total / (1024 * 1024 * 1024))
	availGB := totalGB // conservative fallback
	if out, verr := vmStat(ctx); verr == nil {
		if availBytes, perr := parseVMStatAvailableBytes(out); perr == nil {
			ag := int(availBytes / (1024 * 1024 * 1024))
			if ag < 0 {
				ag = 0
			}
			if ag > totalGB {
				ag = totalGB
			}
			availGB = ag
		}
	}
	return totalGB, availGB, nil
}

// vmStat runs `vm_stat` with a short timeout, mirroring sysctlString.
func vmStat(ctx context.Context) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, "vm_stat").Output()
}

// parseVMStatAvailableBytes approximates available memory from `vm_stat`
// output: (Pages free + inactive + speculative + purgeable) × the page
// size declared in the header line "(page size of N bytes)". Returns an
// error when the page size or the "Pages free" line cannot be parsed.
func parseVMStatAvailableBytes(out []byte) (uint64, error) {
	pageSize, err := parseVMStatPageSize(out)
	if err != nil {
		return 0, err
	}
	counts := map[string]uint64{}
	sawFree := false
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		num := strings.TrimRight(strings.TrimSpace(val), ".")
		n, perr := strconv.ParseUint(num, 10, 64)
		if perr != nil {
			continue
		}
		k := strings.TrimSpace(key)
		counts[k] = n
		if k == "Pages free" {
			sawFree = true
		}
	}
	if !sawFree {
		return 0, errors.New("vm_stat: 'Pages free' line not found")
	}
	pages := counts["Pages free"] + counts["Pages inactive"] +
		counts["Pages speculative"] + counts["Pages purgeable"]
	return pages * pageSize, nil
}

// parseVMStatPageSize extracts the page size (bytes) from the vm_stat
// header line, e.g. "Mach Virtual Memory Statistics: (page size of 16384 bytes)".
func parseVMStatPageSize(out []byte) (uint64, error) {
	const marker = "page size of "
	s := string(out)
	i := strings.Index(s, marker)
	if i < 0 {
		return 0, errors.New("vm_stat: page-size marker not found")
	}
	rest := s[i+len(marker):]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		return 0, errors.New("vm_stat: malformed page-size header")
	}
	return strconv.ParseUint(rest[:end], 10, 64)
}

func defaultStorage(_ context.Context, path string) (int64, error) {
	if path == "" {
		return 0, errors.New("storage: empty path")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

// defaultUMA on macOS recognises Apple Silicon and reads the operator-
// tuned `iogpu.wired_limit_mb` sysctl introduced in macOS 14. When the
// sysctl is unavailable (older macOS, or default of 0 meaning "OS
// decides") we fall back to 75 % of total RAM — matches Apple's own
// documented default split between wired GPU memory and reclaimable
// system memory.
func defaultUMA(ctx context.Context, p *Profile) {
	if runtime.GOARCH != "arm64" {
		return
	}
	p.UnifiedMemory = true
	if v, err := sysctlUint64(ctx, "iogpu.wired_limit_mb"); err == nil && v > 0 {
		p.UsableVRAMMB = int(v)
		return
	}
	// Fallback: 75 % of total RAM (Apple Silicon's documented default
	// upper bound for GPU-wired memory).
	if p.RAMTotalGB > 0 {
		p.UsableVRAMMB = p.RAMTotalGB * 3 / 4 * 1024
	}
}

func sysctlString(ctx context.Context, name string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "sysctl", "-n", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sysctlUint64(ctx context.Context, name string) (uint64, error) {
	s, err := sysctlString(ctx, name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}
