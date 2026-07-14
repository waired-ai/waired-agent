//go:build linux

package proclist

import (
	"os"
	"path/filepath"
	"strconv"
)

// list reads every /proc/<pid>/cmdline. Processes that exit mid-scan (the
// read fails) are skipped rather than aborting the whole enumeration.
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
