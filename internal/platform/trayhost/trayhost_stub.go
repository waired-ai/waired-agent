//go:build !linux

package trayhost

// Check is a no-op off Linux: the SNI system tray and its host extensions are a
// Linux-desktop concern (macOS and Windows have native tray hosts). Returns
// NotApplicable so callers emit no finding.
func Check() Result { return Result{Status: NotApplicable} }
