//go:build !linux

package trayhost

// Check is a no-op off Linux: the SNI system tray and its host extensions are a
// Linux-desktop concern (macOS and Windows have native tray hosts). Returns
// NotApplicable so callers emit no finding.
func Check() Result { return Result{Status: NotApplicable} }

// MenuLabels is the spec dialect off Linux, and unused there: Win32 and
// AppKit menus have their own markup rules (an ampersand mnemonic on
// Windows, none at all on macOS), and internal/gui/tray's escapeMenuLabel
// keys those off GOOS, never off this.
func MenuLabels() MenuDialect { return MenuDialectSpec }
