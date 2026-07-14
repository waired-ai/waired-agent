// Package elevation reports whether the current process is running with
// elevated privileges — root on Unix, an elevated (high-integrity) token
// on Windows.
//
// It exists because os.Geteuid() is the only portable elevation signal Go
// exposes, and it returns -1 on Windows, silently defeating `euid == 0`
// gates (waired#749: an elevated `waired init` never auto-enabled the
// system-wide Claude Code managed settings). Callers that gate a
// machine-wide action route through IsElevated instead of a bare euid
// check so the decision is correct on all three OSes.
package elevation

// IsElevated reports whether the current process can perform actions that
// require administrative privileges on this OS (root on Unix; an elevated
// token on Windows). On an OS with no known elevation model it is
// conservatively false.
func IsElevated() bool { return isElevated() }
