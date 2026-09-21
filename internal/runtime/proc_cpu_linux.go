//go:build linux

package runtime

import (
	"bytes"
	"os"
	"strconv"
	"time"
)

// clockTicksPerSecond is USER_HZ, the unit of the CPU times in
// /proc/<pid>/stat. It is 100 on every Linux ABI Go supports; reading it
// properly needs sysconf(_SC_CLK_TCK), which needs cgo.
const clockTicksPerSecond = 100

// TreeCPUTime is the CPU time the child's process group has used: every
// member's user and system time, plus what its reaped children used. vLLM's
// EngineCore, its workers and the nvcc compiles flashinfer runs through
// ninja all stay in the group (see TreeAlive), so this is the start's own
// work (waired-agent#1508).
func (p *osProcess) TreeCPUTime() (time.Duration, error) {
	ticks, err := groupCPUTicks(p.cmd.Process.Pid)
	if err != nil {
		return 0, err
	}
	return time.Duration(ticks) * time.Second / clockTicksPerSecond, nil
}

// groupCPUTicks sums utime+stime+cutime+cstime over the processes whose
// process group is pgid.
func groupCPUTicks(pgid int) (int64, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		pgrp, ticks, ok := parseProcStatCPU(raw)
		if !ok || pgrp != pgid {
			continue
		}
		total += ticks
	}
	return total, nil
}

// parseProcStatCPU reads the process group and utime+stime+cutime+cstime
// from a /proc/<pid>/stat line. After the last ')' the fields are: state
// ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime
// stime cutime cstime …, so the four times are fields 11–14 counted from
// state (proc(5)).
func parseProcStatCPU(raw []byte) (pgrp int, ticks int64, ok bool) {
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(raw[i+1:])
	if len(fields) < 15 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, false
	}
	for _, f := range fields[11:15] {
		n, err := strconv.ParseInt(string(f), 10, 64)
		if err != nil {
			return 0, 0, false
		}
		ticks += n
	}
	return pgrp, ticks, true
}
