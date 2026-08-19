//go:build linux

package hardware

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
)

func defaultCPU(_ context.Context) CPUInfo {
	info := CPUInfo{Cores: runtime.NumCPU()}
	f, err := os.Open("/proc/cpuinfo")
	if err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					info.Model = strings.TrimSpace(line[i+1:])
					break
				}
			}
		}
	}
	return info
}

func defaultRAM(_ context.Context) (int, int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseProcMeminfo(f)
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

// defaultUMA on Linux detects AMD Strix Halo APUs (Ryzen AI Max series)
// which share physical memory between CPU and GPU. The detector reads
// VRAMTotalMB from the AMD GPU entry that detectAMD (gpu_amd.go) has
// already populated (via rocm-smi, which reads sysfs
// mem_info_vram_total internally) and treats it as the
// authoritative carve-out budget, clamped to the BIOS UMA ceiling (see
// strixHaloUMA). Linux requires a real reading to flip
// UnifiedMemory — the iGPU is invisible without it — so the heuristic
// fallback inside the helper is effectively unused here; sharing the
// helper with Windows just keeps the clamp logic in one place.
//
// Strix Halo is the only Linux UMA target this PR scopes in. Other AMD
// APUs (Phoenix, Hawk Point) have much smaller iGPUs and don't change
// the picker's decisions, so they remain UnifiedMemory=false here.
func defaultUMA(_ context.Context, p *Profile) {
	if !IsStrixHaloAPU(p.CPU.Model) {
		return
	}
	var amdVRAMMB int
	for _, g := range p.GPUs {
		if strings.EqualFold(g.Vendor, "amd") && g.VRAMTotalMB > 0 {
			amdVRAMMB = g.VRAMTotalMB
			break
		}
	}
	if amdVRAMMB == 0 {
		return
	}
	p.UnifiedMemory = true
	// Linux only gets here with a real reading, so the carve-out is
	// always the non-zero one — but it comes from the shared helper
	// anyway, so this file and the Windows one cannot disagree about
	// which branch produced the budget. Windows takes a different branch
	// of that helper: there the carve-out is neither the budget nor an
	// addend (waired-ai/waired-agent#863). Nothing on the Linux side was
	// measured, so nothing here changes — see waired-ai/waired-agent#868.
	p.UsableVRAMMB, p.CarveOutVRAMMB = strixHaloUMA(
		runtime.GOOS, amdVRAMMB, p.RAMTotalGB, p.RAMAvailableAtInstallGB)
}
