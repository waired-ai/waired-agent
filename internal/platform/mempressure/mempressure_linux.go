package mempressure

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hostPressurePath is the host-wide reading, which is the one this package
// wants: the question is whether the MACHINE is starving, not whether one
// cgroup is being held to its limit.
const hostPressurePath = "/proc/pressure/memory"

type linuxSampler struct {
	// path is resolved once. Re-resolving per sample would read
	// /proc/self/cgroup on every tick for an answer that cannot change.
	path string
	err  error
}

func newPlatformSampler() platformSampler {
	s := &linuxSampler{}
	if _, err := os.Stat(hostPressurePath); err == nil {
		s.path = hostPressurePath
		return s
	}
	// No host-wide file: a container with a masked /proc, or a kernel built
	// without PSI. This process's own cgroup may still have one, and in a
	// container that IS the machine as far as anything here can starve.
	if p := selfCgroupPressurePath(); p != "" {
		s.path = p
		return s
	}
	s.err = fmt.Errorf("mempressure: no %s and no cgroup memory.pressure", hostPressurePath)
	return s
}

// selfCgroupPressurePath returns this process's cgroup v2 memory.pressure,
// or "" when there is none. The v2 line is the one with an empty controller
// list ("0::<path>").
func selfCgroupPressurePath() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		p := "/sys/fs/cgroup" + strings.TrimSpace(rel) + "/memory.pressure"
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func (s *linuxSampler) facts() Facts {
	f := Facts{SwapOutTotalMB: swapOutTotalMB(), SwapOutMBPerSec: -1}
	f.AvailMB, f.TotalMB = availTotalMB()
	if s.err != nil {
		f.LinuxErr = s.err
		return f
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		f.LinuxErr = fmt.Errorf("mempressure: read %s: %w", s.path, err)
		return f
	}
	some, full, err := parsePressure(b)
	if err != nil {
		f.LinuxErr = err
		return f
	}
	f.LinuxSomeAvg10, f.LinuxFullAvg10 = some, full
	return f
}

// availTotalMB reads MemAvailable and MemTotal. MemAvailable rather than
// MemFree: free memory on Linux is the memory nobody has found a use for,
// and a host with 80 GB of page cache has almost none of it.
func availTotalMB() (avail, total uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemAvailable":
			avail = kb / 1024
		case "MemTotal":
			total = kb / 1024
		}
	}
	return avail, total
}

// swapOutTotalMB is pswpout, the pages written to swap since boot, in MB.
// Monotonic, which is what the rate wants: swap freed later does not undo
// the writing. -1 when /proc/vmstat cannot be read.
func swapOutTotalMB() float64 {
	b, err := os.ReadFile("/proc/vmstat")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, " ")
		if !ok || name != "pswpout" {
			continue
		}
		pages, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return -1
		}
		return float64(pages) * float64(os.Getpagesize()) / (1 << 20)
	}
	return -1
}

func (s *linuxSampler) close() error { return nil }
