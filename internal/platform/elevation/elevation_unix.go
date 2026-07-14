//go:build linux || darwin

package elevation

import "os"

// isElevated reports whether the process runs as root. euid (not real uid)
// is the effective-privilege check, matching the existing `os.Geteuid()==0`
// gates for `sudo waired init` (initStateDirMode, service install).
func isElevated() bool { return os.Geteuid() == 0 }
