//go:build windows

package elevation

import "golang.org/x/sys/windows"

// isElevated reports whether the current process token is elevated (high
// integrity — launched via "Run as administrator" / UAC). It mirrors the
// token-handle pattern already used in internal/platform/secrets:
// GetCurrentProcessToken returns a pseudo-handle to the process token that
// must NOT be closed. Token.IsElevated reads TokenElevation and returns
// false on any query error, so a hardened/denied token reads as
// not-elevated rather than panicking.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
