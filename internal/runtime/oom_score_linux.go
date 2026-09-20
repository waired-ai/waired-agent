//go:build linux

package runtime

import (
	"fmt"
	"os"
	"strconv"
)

// engineOOMScoreAdj is what the kernel's OOM killer is told to think of the
// engine, relative to everything else on the machine.
//
// 500 of a possible 1000: a strong preference, not a death sentence. When
// Linux has to kill something to survive, the engine and its model runner
// are what it should take — they are the largest thing here, they are
// restartable, and the alternative is the kernel picking the operator's
// editor or the ssh session they would use to fix it. It is not 1000
// because that also makes the engine the victim of a small, unrelated
// memory spike somewhere else on the box, and a needlessly dead engine
// costs a multi-minute reload.
//
// This is the Linux half of making a wrong memory estimate survivable
// (waired-agent#1453). The Windows half is the Job Object; macOS has no
// public equivalent, and jetsam already prefers the largest consumer.
const engineOOMScoreAdj = 500

// applyEngineOOMScore writes oom_score_adj for a spawned engine.
//
// Written to the LEADER only, and that is enough: Linux inherits
// oom_score_adj across fork and exec, measured on 2026-09-20 (a parent set
// to 753 produced a child reading 753). So ollama's llama-server runner —
// the process that actually holds the model, and the one worth killing —
// picks this up without the spawner knowing anything about it.
//
// Best-effort. A container without write access to /proc/<pid>/oom_score_adj,
// or a kernel that does not have it, leaves the engine at the default, which
// is the behaviour before this existed.
func applyEngineOOMScore(pid int) error {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	if err := os.WriteFile(path, []byte(strconv.Itoa(engineOOMScoreAdj)), 0o644); err != nil {
		return fmt.Errorf("runtime: set %s: %w", path, err)
	}
	return nil
}
