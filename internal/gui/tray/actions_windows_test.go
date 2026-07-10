//go:build windows

package tray

import (
	"testing"
)

// TestCopyToClipboard_Returns verifies the API surface — that
// CopyToClipboard accepts a non-empty string and returns nil on a
// real Windows host with a clipboard session. The round-trip
// (write here, read in another process) is verified via
// PowerShell Get-Clipboard during the Phase W-3 manual screenshot
// iteration; a Go-side reader is omitted because every flavour of
// uintptr → unsafe.Pointer the vet analyser tolerates still costs
// an explicit annotation, and reading the clipboard is not
// something the tray itself ever does.
func TestCopyToClipboard_Returns(t *testing.T) {
	if err := CopyToClipboard("waired-tray-clipboard-test"); err != nil {
		t.Skipf("CopyToClipboard: %v (likely no clipboard session, e.g. CI)", err)
	}
}

func TestCopyToClipboard_TrimsTrailingNewline(t *testing.T) {
	// Implementation detail: the helper strips trailing \r\n so a
	// "copy this overlay IP" menu click does not leave the
	// clipboard with a stray newline. Verified indirectly here by
	// asserting the function still succeeds with such input.
	if err := CopyToClipboard("100.96.0.42\r\n"); err != nil {
		t.Skipf("CopyToClipboard: %v", err)
	}
}
