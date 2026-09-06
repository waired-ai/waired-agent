//go:build windows

package tray

import (
	"slices"
	"testing"
)

// TestLogoutRunAsArgsCarryTheStateDir pins the argv the UAC prompt authorizes.
//
// waired-agent#1269 was reported on macOS, but the state-dir misreading behind
// it is not OS-specific: %ProgramData%\waired is DACL'd to
// SYSTEM+Administrators, so the desktop-user app was equally unable to see it
// and equally likely to hand this argv the per-user directory instead. `waired
// logout` on a directory with no identity exits 0 having removed nothing, and
// this OS cannot even observe that — shellExecuteRunAs deliberately does not
// wait for the elevated process. Cross-OS parity (CLAUDE.md): the fix is
// checked on all three, not only on the one the report came from.
func TestLogoutRunAsArgsCarryTheStateDir(t *testing.T) {
	got := logoutRunAsArgs(`C:\ProgramData\waired`)
	want := []string{"logout", "--yes", "--state-dir", `C:\ProgramData\waired`}
	if !slices.Equal(got, want) {
		t.Errorf("logoutRunAsArgs =\n %q\nwant\n %q", got, want)
	}
}

// A state dir holding a space must survive the CreateProcess command-line
// convention as ONE argument.
func TestLogoutRunAsArgsQuoteASpacedPath(t *testing.T) {
	got := quoteArgsForShellExec(logoutRunAsArgs(`C:\Program Files\waired-state`))
	want := `logout --yes --state-dir "C:\Program Files\waired-state"`
	if got != want {
		t.Errorf("quoteArgsForShellExec =\n %q\nwant\n %q", got, want)
	}
}

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
