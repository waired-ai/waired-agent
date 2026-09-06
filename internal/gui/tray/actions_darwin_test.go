//go:build darwin

package tray

import "testing"

// The command builders only. Nothing here reaches runOsascriptAdmin or any
// other seamed helper: on darwin those raise a real administrator prompt or a
// modal dialog, and a test that reaches one does not fail on a headless runner
// — it hangs to the job timeout (#152). scripts/ci/tray-dialog-seam-guard.sh
// enforces that, and darwin-tagged test files get no exemption from it.

// TestLogoutShellCommandQuotesTheStateDir pins the command the administrator
// prompt authorizes.
//
// The default macOS state dir contains a space, so the quoting is load-bearing
// rather than defensive: unquoted, `do shell script` would hand `waired
// logout` two arguments and the flag would take "/Library/Application" as the
// directory. Nothing pinned this before waired-agent#1269 — actions_darwin.go
// had no test file at all, which is why the issue could reasonably suspect
// this layer when the defect was one caller up.
func TestLogoutShellCommandQuotesTheStateDir(t *testing.T) {
	got := logoutShellCommand("/usr/local/bin/waired", "/Library/Application Support/waired")
	want := `'/usr/local/bin/waired' logout --yes --state-dir '/Library/Application Support/waired'`
	if got != want {
		t.Errorf("logoutShellCommand =\n %q\nwant\n %q", got, want)
	}
}

// A path holding a single quote must not be able to end the quoting and start
// a second command.
func TestLogoutShellCommandEscapesSingleQuotes(t *testing.T) {
	got := logoutShellCommand("/usr/local/bin/waired", "/tmp/it's here")
	want := `'/usr/local/bin/waired' logout --yes --state-dir '/tmp/it'\'' here'`
	if got != want {
		t.Errorf("logoutShellCommand =\n %q\nwant\n %q", got, want)
	}
}

// TestInstallOllamaShellCommandOmitsAnEmptyStateDir: passing the flag with an
// empty value would tell the elevated CLI to use the empty string rather than
// let it fall back to its own default.
func TestInstallOllamaShellCommandOmitsAnEmptyStateDir(t *testing.T) {
	withDir := installOllamaShellCommand("/usr/local/bin/waired", "/Library/Application Support/waired")
	wantWith := `'/usr/local/bin/waired' runtimes install ollama -y --state-dir '/Library/Application Support/waired'`
	if withDir != wantWith {
		t.Errorf("with a state dir =\n %q\nwant\n %q", withDir, wantWith)
	}
	without := installOllamaShellCommand("/usr/local/bin/waired", "")
	wantWithout := `'/usr/local/bin/waired' runtimes install ollama -y`
	if without != wantWithout {
		t.Errorf("with no state dir =\n %q\nwant\n %q", without, wantWithout)
	}
}

// TestOsascriptAdminScriptWrapsTheCommand pins the AppleScript round trip: the
// shell command is a double-quoted AppleScript string literal, so its single
// quotes survive untouched and its backslashes and double quotes are escaped.
func TestOsascriptAdminScriptWrapsTheCommand(t *testing.T) {
	got := osascriptAdminScript(logoutShellCommand("/usr/local/bin/waired", "/Library/Application Support/waired"))
	want := `do shell script "'/usr/local/bin/waired' logout --yes --state-dir '/Library/Application Support/waired'" with administrator privileges`
	if got != want {
		t.Errorf("osascriptAdminScript =\n %q\nwant\n %q", got, want)
	}

	esc := osascriptAdminScript(`echo "hi" \ there`)
	wantEsc := `do shell script "echo \"hi\" \\ there" with administrator privileges`
	if esc != wantEsc {
		t.Errorf("osascriptAdminScript(escapes) =\n %q\nwant\n %q", esc, wantEsc)
	}
}
