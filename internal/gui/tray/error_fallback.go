//go:build linux || darwin

package tray

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/waired-ai/waired-agent/internal/platform/notification"
)

// errorFallback carries a message the user needs to see on a desktop
// where ShowError's dialog could not be raised — no zenity and no
// kdialog on Linux, no usable osascript on macOS.
//
// It used to be `fmt.Fprintln(os.Stderr, …)` and nothing else. The tray
// is a background process a desktop session starts; on Linux that
// stream goes to the systemd journal and on macOS to a launchd log, so
// the person who just clicked something got no answer at all. That is
// the same silence waired-agent#831 is about, one layer down: a failed
// model switch reported this way was indistinguishable from a click
// that did nothing.
//
// All three channels, because they fail in different places: the toast
// is the one the user sees, WARN is the one an operator finds
// afterwards, and stderr stays for the case where the notification
// backend is missing too (a headless or minimal session, where it is
// the only one of the three that still lands anywhere).
//
// Windows has no fallback path to fix — MessageBoxW is always there.
func errorFallback(message string) {
	slog.Warn("tray: no dialog backend for an error the user needs to see", "message", message)
	notify(message, notification.Error)
	fmt.Fprintln(os.Stderr, "waired-tray:", message)
}
