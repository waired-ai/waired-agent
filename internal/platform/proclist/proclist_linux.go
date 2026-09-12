//go:build linux

package proclist

import (
	"os"
	"path/filepath"
	"strconv"
)

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
		out = append(out, ProcInfo{PID: pid, Argv: argv})
	}
	return out, nil
}
