//go:build !windows

package appcontrol

import "context"

// Check has nothing to read off Windows. Application Control is a Windows
// feature: Linux and macOS refuse to execute a file on permissions or on
// Gatekeeper's quarantine attribute, neither of which is a reputation verdict
// that changes its mind about the same bytes, and neither of which leaves the
// per-launch record this package reads. Returning NotApplicable makes callers
// emit nothing.
func Check(context.Context) Result { return Result{Status: NotApplicable} }
