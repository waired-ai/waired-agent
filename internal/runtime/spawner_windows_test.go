//go:build windows

package runtime

import (
	"errors"
	"syscall"
	"testing"
)

// TestOSProcess_Signal_ReportsUnsupported pins a PRODUCT CONTRACT: the
// Windows spawner must tell the adapter that no signal can be delivered,
// so the stop escalates to Kill immediately instead of waiting out a
// grace period the child never received (#316). Returning nil here — the
// pre-#316 behaviour — is what made every tray-initiated stop time out
// with the engine still resident.
//
// The untagged half of this contract is covered on every CI leg by
// TestOllamaAdapter_Stop_ImmediateKillWhenSignalsUnsupported, which
// injects the same sentinel through the RunningProcess seam.
func TestOSProcess_Signal_ReportsUnsupported(t *testing.T) {
	// Signal touches no process state, so a zero osProcess is enough.
	p := &osProcess{}
	err := p.Signal(syscall.SIGTERM)
	if !errors.Is(err, ErrSignalUnsupported) {
		t.Fatalf("Signal(SIGTERM) = %v, want ErrSignalUnsupported", err)
	}
}
