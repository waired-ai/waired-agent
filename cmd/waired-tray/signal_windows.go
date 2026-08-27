//go:build windows

package main

import "os"

// shutdownSignals on Windows is just os.Interrupt (Ctrl-C), and even
// that never arrives in practice: the tray is linked -H windowsgui
// (Makefile), so it has no console to receive control events from.
//
// SIGTERM exists as a constant in package syscall on Windows but is
// never delivered to a Windows process; listing it would be cargo-cult
// — the same ruling cmd/waired-agent/signal_windows.go records for the
// daemon, and it was exactly what cmd/waired-tray did until
// waired-agent#1045.
//
// The Windows way to ask this process to go away is therefore a window
// message, not a signal: fyne.io/systray's hidden window handles
// WM_CLOSE (which is what `taskkill` without /F posts) by removing the
// notification icon and leaving the event loop. packaging/install/
// uninstall.ps1's Stop-Tray is the caller that matters.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
