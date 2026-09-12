//go:build darwin

package proclist

import (
	"context"
	"os/exec"
	"time"
)

// list enumerates processes via `ps`, in TWO reads.
//
// `-o command=` is one free-form column, so a program path containing a
// space cannot be told from the arguments that follow it — and every macOS
// install has one: the bundled engine lives under
// /Library/Application Support/waired. Splitting that column on whitespace
// gave argv[0] = "/Library/Application", so IsRunnerProc never matched the
// runner and the host advertised the parallelism it asked for rather than
// the one the runner was given (waired-agent#1303).
//
// `-o comm=` answers the same question without the ambiguity: the executable
// path is the whole line after the PID, and the parent PID fits in front of
// it because it is numeric. mergePsReads joins the two by PID.
// The second read is best-effort — if it fails, the merge degrades to the
// single-read behaviour rather than losing the process table.
//
// -ww disables column truncation so a long runner command line survives
// intact; the trailing `=` on each field drops the header.
//
// Reading argv via sysctl KERN_PROCARGS2 remains the zero-dependency
// alternative and is still not used here: `ps` keeps the whole parser in the
// package's shared untagged file, where CI (Linux only) can table-test it.
// It becomes the right answer only if something ever needs a space-bearing
// flag VALUE rather than argv[0] — see argvWithProgram.
func list() ([]ProcInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	comm, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		comm = nil
	}
	return mergePsReads(command, comm), nil
}
