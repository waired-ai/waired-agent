//go:build darwin

package proclist

import (
	"context"
	"os/exec"
	"time"
)

// list enumerates processes via `ps`. -ww disables column truncation so a
// long runner command line survives intact; `-o pid=,command=` prints the
// PID and full argv with no header. Reading argv via sysctl KERN_PROCARGS2
// is the zero-dependency alternative; `ps` is used here to avoid cgo/syscall
// for a once-per-verification probe.
func list() ([]ProcInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,command=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parsePsOutput(out), nil
}
