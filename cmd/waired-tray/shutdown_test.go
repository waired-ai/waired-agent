package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/gui/tray"
)

// TestShutdownDeadlineOutlastsTheTrayBudget pins the relationship, not
// the number.
//
// shutdownDeadline is the point at which a signalled tray stops waiting
// for its GUI event loop and exits regardless (waired-agent#1045). Set
// it at or below the tray's own shutdown budget and the backstop starts
// killing trays that are winding down correctly — the mesh withdrawal
// lands, the engine stop does not, and the daemon is left suspended.
//
// Same shape, one layer up, as internal/gui/tray's
// TestEngineWriteTimeoutOutlastsDaemonStopBudget (#316).
func TestShutdownDeadlineOutlastsTheTrayBudget(t *testing.T) {
	if shutdownDeadline <= tray.ShutdownBudget {
		t.Errorf("shutdownDeadline (%v) must outlast tray.ShutdownBudget (%v)",
			shutdownDeadline, tray.ShutdownBudget)
	}
}

// TestShutdownSignalsAreDeliverable is the cross-OS parity note as a
// test: the list this binary registers must be one the OS actually
// delivers. cmd/waired-agent/signal_windows.go already rules that
// listing SIGTERM on Windows is cargo-cult, and cmd/waired-tray did
// exactly that in an untagged file until waired-agent#1045. The per-OS
// files are what fix it; this asserts neither of them is empty, since an
// empty list would silently disarm the shutdown watcher.
func TestShutdownSignalsAreDeliverable(t *testing.T) {
	if got := shutdownSignals(); len(got) == 0 {
		t.Fatal("shutdownSignals() is empty: nothing would ever cancel the tray's context")
	}
}
