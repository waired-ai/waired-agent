//go:build !linux && !windows && !darwin

package proclist

// list is unavailable on platforms without a process-enumeration
// implementation (the agent ships for linux/windows/darwin; this keeps the
// package buildable elsewhere). Callers fall back to the intent value.
func list() ([]ProcInfo, error) { return nil, ErrUnsupported }
