//go:build windows

package main

import "os"

// shutdownSignals on Windows is just os.Interrupt (Ctrl-C). When the
// agent runs under the SCM, the service handler in service_windows.go
// cancels the context directly on svc.Stop / svc.Shutdown, bypassing
// signal delivery entirely. So this list only matters in interactive
// `waired-agent.exe` / `waired-agent.exe debug` runs.
//
// SIGTERM exists as a constant in package syscall on Windows but is
// never delivered to a Windows process; listing it would be cargo-cult.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
