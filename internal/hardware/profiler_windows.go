//go:build windows

package hardware

import (
	"context"
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// defaultCPU on Windows pulls the human-readable processor name from
// the HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0 registry key
// (ProcessorNameString). We deliberately avoid `wmic` — it is deprecated
// and removed in Windows 11 24H2+, making it unreliable on freshly
// updated machines.
func defaultCPU(_ context.Context) CPUInfo {
	info := CPUInfo{Cores: runtime.NumCPU()}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`,
		registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if name, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			info.Model = trimNul(name)
		}
	}
	return info
}

func trimNul(s string) string {
	for i, r := range s {
		if r == 0 {
			return s[:i]
		}
	}
	// Most registry reads also leave trailing whitespace from the BIOS
	// string; collapse it so JSON output is tidy.
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure. Not
// exposed by golang.org/x/sys/windows so we declare it locally and
// invoke GlobalMemoryStatusEx via LazyProc.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var (
	modKernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
)

// defaultRAM on Windows uses GlobalMemoryStatusEx, which is the same
// API Task Manager and Performance Monitor use. TotalPhys is the
// total physical memory in bytes (excludes swap); AvailPhys is the
// instantaneous available physical memory.
func defaultRAM(_ context.Context) (int, int, error) {
	var memStatus memoryStatusEx
	memStatus.Length = uint32(unsafe.Sizeof(memStatus))
	r1, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&memStatus)))
	if r1 == 0 {
		return 0, 0, callErr
	}
	// Round to nearest GiB (bytesToGBRounded, #61): TotalPhys already
	// excludes hardware-reserved memory, so a 32 GB box reports ~31.9 GiB
	// and flooring produced 31 — a spurious miss against a 32 GB threshold.
	totalGB := bytesToGBRounded(memStatus.TotalPhys)
	availGB := bytesToGBRounded(memStatus.AvailPhys)
	return totalGB, availGB, nil
}

// defaultStorage on Windows uses GetDiskFreeSpaceEx for the volume that
// contains `path`. The "free bytes available to the calling thread"
// value accounts for per-user quotas, which is what we actually want
// when deciding whether the agent (running as LocalSystem or a user)
// can stage a model download.
func defaultStorage(_ context.Context, path string) (int64, error) {
	if path == "" {
		return 0, errors.New("storage: empty path")
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeBytesAvailable, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFree); err != nil {
		return 0, err
	}
	if freeBytesAvailable > 1<<62 {
		// Guard against unsigned-to-signed overflow on absurdly large
		// reported values (driver bugs / virtualised disks).
		return 1 << 62, nil
	}
	return int64(freeBytesAvailable), nil
}

// defaultUMA settles the UMA POLICY for this host. The rule is
// unifiedBudgetFor, untagged and shared with Linux — including the
// Windows-specific part, that the firmware carve-out reading is neither
// the budget nor an addend here (waired-agent#863, decision
// 20260820/0005). See that function.
func defaultUMA(_ context.Context, p *Profile) {
	applyUnifiedBudget(runtime.GOOS, p)
}
