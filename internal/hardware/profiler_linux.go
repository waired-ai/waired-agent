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

// defaultUMA settles the UMA POLICY for this host: whether its
// accelerator memory and its RAM are one pool, and what the GPU may
// wire down if so.
//
// The rule itself is unifiedBudgetFor, untagged and shared with Windows.
// It used to live here in a Linux-shaped copy and in a Windows-shaped
// one, and the copies had drifted; see that function for the per-vendor
// rules and for why the platform difference is an argument rather than a
// build tag (waired-agent#459).
func defaultUMA(_ context.Context, p *Profile) {
	applyUnifiedBudget(runtime.GOOS, p)
}
