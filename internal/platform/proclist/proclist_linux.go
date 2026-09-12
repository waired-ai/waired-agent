//go:build linux

package proclist

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readProcPPID reads the parent PID out of /proc/<pid>/stat, or 0 when the
// process vanished or the field cannot be read. The comm field (2) is
// parenthesised and may itself contain spaces and parentheses, so the scan
// starts after the LAST ')' — the standard way to parse this file.
func readProcPPID(name string) int {
	raw, err := os.ReadFile(filepath.Join("/proc", name, "stat"))
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(raw[i+1:]))
	// after ')': state (3), ppid (4)
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// list reads every /proc/<pid>/cmdline. Processes that exit mid-scan (the
// read fails) are skipped rather than aborting the whole enumeration.
//
// ProcInfo.Program is deliberately left empty here, and Linux needs no
// second read to fill it: /proc/<pid>/cmdline is NUL-separated, so
// parseProcCmdline already yields the real argv and a program path
// containing a space is recovered exactly. The defect the other two
// platforms carry (waired-agent#1303) cannot occur on this one, so
// ProcInfo.IsRunner falls through to IsRunnerProc(Argv) with no change in
// behaviour. Reading /proc/<pid>/exe would add a syscall per process for
// nothing.
func list() ([]ProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]ProcInfo, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID dir
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // vanished or unreadable
		}
		argv := parseProcCmdline(raw)
		if len(argv) == 0 {
			continue // kernel threads have an empty cmdline
		}
		out = append(out, ProcInfo{PID: pid, Argv: argv, PPID: readProcPPID(e.Name())})
	}
	return out, nil
}
