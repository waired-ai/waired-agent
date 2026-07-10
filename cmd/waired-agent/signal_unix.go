//go:build !windows

package main

import (
	"os"
	"syscall"
)

// shutdownSignals lists the OS signals that trigger a graceful
// daemon shutdown. On Unix this is SIGINT (Ctrl-C) and SIGTERM
// (systemd's stop signal).
func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}
