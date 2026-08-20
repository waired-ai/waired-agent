//go:build windows

package autostart

import "testing"

// TestRunValueMatchesTheInstallerWriter pins the exact HKCU Run value this
// package writes for the tray.
//
// Product contract (waired-agent#832): since that fix there are TWO writers of
// this value -- the tray's own first run (internal/gui/tray/tray.go
// ensureAutostartOnFirstLaunch, which calls Enable with these arguments) and
// packaging/install/install.ps1's Register-TrayAutostart, which writes it
// during an install that has no interactive desktop for the tray to start on.
// They must agree byte-for-byte: IsEnabled() only checks that a value exists,
// so a disagreement would not be corrected, it would just leave whichever
// entry was written first pointing wherever it pointed.
//
// The PowerShell side pins the same two strings in
// scripts/dev/installtest-windows.ps1 (Get-TrayAutostartCommand's table).
// Change one and the other fails.
func TestRunValueMatchesTheInstallerWriter(t *testing.T) {
	args := []string{"-mgmt", "http://127.0.0.1:9476"}

	for _, tc := range []struct {
		name string
		exe  string
		want string
	}{
		{
			name: "the default install dir has a space, so the path is quoted",
			exe:  `C:\Program Files\Waired\waired-tray.exe`,
			want: `"C:\Program Files\Waired\waired-tray.exe" -mgmt http://127.0.0.1:9476`,
		},
		{
			name: "a space-free dir is left bare",
			exe:  `C:\Waired\waired-tray.exe`,
			want: `C:\Waired\waired-tray.exe -mgmt http://127.0.0.1:9476`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteCommand(tc.exe, args); got != tc.want {
				t.Errorf("quoteCommand:\n  got:  %s\n  want: %s", got, tc.want)
			}
		})
	}
}
