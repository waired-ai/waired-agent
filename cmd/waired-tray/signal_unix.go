//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

// shutdownSignals lists the OS signals that end the tray. On Unix this
// is SIGINT (Ctrl-C from a shell) and SIGTERM — which the desktop
// session manager sends its children at logout, launchctl bootout sends
// a LaunchAgent, and the uninstaller sends a running tray
// (waired-agent#1031).
//
// Same list and same shape as cmd/waired-agent/signal_unix.go, so the
// two binaries answer a stop the same way.
func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}
