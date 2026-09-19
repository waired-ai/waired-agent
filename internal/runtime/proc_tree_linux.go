//go:build linux

package runtime

import (
	"bytes"
	"os"
	"strconv"
)

// groupMembers lists the processes whose process group is pgid, from
// /proc/<pid>/stat. A process that exits between the directory read and
// the stat read is skipped.
func groupMembers(pgid int) ([]treeMember, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []treeMember
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		state, pgrp, ok := parseProcStat(raw)
		if !ok || pgrp != pgid {
			continue
		}
		out = append(out, treeMember{PID: pid, Zombie: state == 'Z' || state == 'X'})
	}
	return out, nil
}

// parseProcStat reads the state and the process group from a
// /proc/<pid>/stat line: "pid (comm) state ppid pgrp …". comm may itself
// contain spaces and parentheses, so the fields are read after the LAST
// closing parenthesis.
func parseProcStat(raw []byte) (state byte, pgrp int, ok bool) {
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(raw[i+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pgrp, true
}
