package mempressure

import (
	"fmt"
	"os"
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
	if s.err != nil {
		return Facts{LinuxErr: s.err}
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return Facts{LinuxErr: fmt.Errorf("mempressure: read %s: %w", s.path, err)}
	}
	some, full, err := parsePressure(b)
	if err != nil {
		return Facts{LinuxErr: err}
	}
	return Facts{LinuxSomeAvg10: some, LinuxFullAvg10: full}
}

func (s *linuxSampler) close() error { return nil }
