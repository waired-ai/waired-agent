package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rc9POSIXStatuslineCommand is verbatim what every waired up to v0.0.2-rc9 wrote
// into ~/.claude/settings.json on all three OSes, Windows included.
const rc9POSIXStatuslineCommand = "command -v waired >/dev/null 2>&1 && exec waired claude statusline"

// PRODUCT CONTRACT (waired-agent#787): the statusLine command must be one the
// shell Claude Code uses on that OS can run.
//
// Ratifying source: waired-agent#787, and the Claude Code status-line reference
// it cites — "On Windows, Claude Code runs status line commands through Git Bash
// when Git Bash is installed, or through PowerShell when Git Bash is absent."
// (https://code.claude.com/docs/en/statusline). statusLine has neither a `shell`
// field nor an exec form, so the Windows string must satisfy both shells at
// once.
func TestStatuslineRenderCommandFor(t *testing.T) {
	cases := map[string]struct {
		goos      string
		want      string
		wantGuard bool
	}{
		"linux keeps the sh guard":   {"linux", rc9POSIXStatuslineCommand, true},
		"darwin keeps the sh guard":  {"darwin", rc9POSIXStatuslineCommand, true},
		"windows is the bare marker": {"windows", "waired claude statusline", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := statuslineRenderCommandFor(tc.goos)
			if got != tc.want {
				t.Errorf("statuslineRenderCommandFor(%q) = %q, want %q", tc.goos, got, tc.want)
			}
			if !strings.Contains(got, statuslineMarker) {
				t.Errorf("command %q lost the marker classifyStatusLine matches on", got)
			}
			for _, posixism := range []string{"command -v", ">/dev/null", "2>&1", "exec "} {
				if has := strings.Contains(got, posixism); has != tc.wantGuard {
					t.Errorf("command %q contains %q = %v, want %v", got, posixism, has, tc.wantGuard)
				}
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#787): the wrapper command must survive both
// shells Claude Code may hand it to on Windows. The literal is spelled out
// because every part of it is load-bearing and each has its own failure mode —
// see statuslineWrapperCommandFor.
func TestStatuslineWrapperCommandFor(t *testing.T) {
	// A home directory with a space in it: the ordinary Windows case, and the
	// one an unquoted command silently splits.
	const winPath = `C:\Users\John Smith\.claude\waired-statusline.ps1`
	const winWant = `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "C:/Users/John Smith/.claude/waired-statusline.ps1"`

	if got := statuslineWrapperCommandFor("windows", winPath); got != winWant {
		t.Errorf("windows command =\n  %q\nwant\n  %q", got, winWant)
	}
	if strings.Contains(statuslineWrapperCommandFor("windows", winPath), `\`) {
		t.Error("a backslash survived: Git Bash consumes it as an escape before PowerShell ever sees the path")
	}
	for _, goos := range []string{"linux", "darwin"} {
		const unixPath = "/home/dev/.claude/waired-statusline.sh"
		if got := statuslineWrapperCommandFor(goos, unixPath); got != unixPath {
			t.Errorf("%s command = %q, want the bare path %q", goos, got, unixPath)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#787): the wrapper script must be written in a
// language the host can execute — a `#!/bin/sh` script on Windows is the defect
// this issue is about, one layer down from the command that names it.
func TestWrapperScriptFor(t *testing.T) {
	unix := wrapperScriptFor("linux")
	if wrapperScriptFor("darwin") != unix {
		t.Error("darwin and linux must share one wrapper script")
	}
	if !strings.HasPrefix(unix, "#!/bin/sh") {
		t.Errorf("unix wrapper lost its shebang: %q", unix[:min(24, len(unix))])
	}

	win := wrapperScriptFor("windows")
	if strings.Contains(win, "#!/bin/sh") {
		t.Error("windows wrapper is still a POSIX shell script")
	}
	if !strings.Contains(win, "[Console]::In.ReadToEnd()") {
		t.Error("windows wrapper does not read the statusline JSON from stdin")
	}
	// $input is a PowerShell automatic variable and is drained by its first
	// enumeration — assigning to it, or reading it twice, silently gives the
	// second consumer nothing.
	if strings.Contains(win, "$input") {
		t.Error("windows wrapper uses the reserved $input automatic variable")
	}
	if !strings.Contains(win, "Get-Command waired") {
		t.Error("windows wrapper lost the guard that keeps an uninstall from breaking the footer")
	}
}

// PRODUCT CONTRACT (waired-agent#816): the Windows wrapper must locate Git Bash
// by where it is installed, never by the name `bash.exe` on PATH.
//
// On Windows that name is C:\WINDOWS\system32\bash.exe — the WSL launcher — and
// Git for Windows does not add itself to PATH. A `Get-Command bash.exe` lookup
// therefore finds WSL on a machine that has Git Bash installed, hands it a
// command written for Git Bash, and the user's own status-line output silently
// disappears. Shipped that way in #808 and found on real hardware.
//
// This is asserted as a property of the script text because the failure is a
// PATH-resolution difference on a live Windows host with WSL present: nothing in
// this suite can execute the script, so the closest reachable guard is "the
// script is not allowed to ask that question".
func TestWrapperScriptResolvesGitBashByLocationNotByName(t *testing.T) {
	win := wrapperScriptFor("windows")

	if strings.Contains(win, "Get-Command bash") {
		t.Error("the wrapper looks bash up by name; on Windows that finds the WSL launcher")
	}
	// The installed locations it must actually probe. `git.exe` is the useful
	// one — it IS on PATH when Git for Windows is installed, and its own
	// directory locates the bash beside it.
	for _, want := range []string{
		"Get-Command git.exe",
		`Git\bin\bash.exe`,
		"Test-Path",
	} {
		if !strings.Contains(win, want) {
			t.Errorf("wrapper does not probe %q", want)
		}
	}
	// Fails closed: no Git Bash means PowerShell runs the original, not some
	// other program that happens to answer to the name.
	if !strings.Contains(win, "Invoke-Expression") {
		t.Error("wrapper lost the PowerShell fallback for a host with no Git Bash")
	}
}

// PRODUCT CONTRACT (waired-agent#787): a wrapper waired wrote must stay
// recognisable as ours whichever OS spelled it. classifyStatusLine is the gate
// RemoveStatusLine passes through, so a wrapper it fails to recognise is one
// `waired claude disable` leaves on disk while restoring nothing.
func TestClassifyStatusLineRecognisesEitherWrapperSpelling(t *testing.T) {
	cases := map[string]struct {
		command string
		want    StatusLineKind
	}{
		"bare windows render command": {"waired claude statusline", StatusLineOurs},
		"rc9 posix render command":    {rc9POSIXStatuslineCommand, StatusLineOurs},
		"unix wrapper path": {
			"/home/dev/.claude/waired-statusline.sh", StatusLineWrapped},
		"windows wrapper command": {
			`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "C:/Users/dev/.claude/waired-statusline.ps1"`,
			StatusLineWrapped},
		"a .sh wrapper left on a windows host": {
			`C:\Users\dev\.claude\waired-statusline.sh`, StatusLineWrapped},
		"someone else's statusline": {"~/my-statusline.sh --flag", StatusLineForeign},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(statusLineObj{Type: "command", Command: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			got, gotCmd := classifyStatusLine(map[string]json.RawMessage{statuslineKey: raw})
			if got != tc.want {
				t.Errorf("classifyStatusLine(%q) = %v, want %v", tc.command, got, tc.want)
			}
			if gotCmd != tc.command {
				t.Errorf("command = %q, want %q", gotCmd, tc.command)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#787): status must not call a command installed
// when this OS cannot run it. Only the rc9 Windows combination is unrunnable.
func TestStatusLineRunsOn(t *testing.T) {
	winWrapper := `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "C:/Users/dev/.claude/waired-statusline.ps1"`
	shWrapper := `C:\Users\dev\.claude\waired-statusline.sh`
	cases := map[string]struct {
		goos string
		kind StatusLineKind
		cmd  string
		want bool
	}{
		"linux, rc9 posix":   {"linux", StatusLineOurs, rc9POSIXStatuslineCommand, true},
		"darwin, rc9 posix":  {"darwin", StatusLineOurs, rc9POSIXStatuslineCommand, true},
		"windows, rc9 posix": {"windows", StatusLineOurs, rc9POSIXStatuslineCommand, false},
		"windows, bare":      {"windows", StatusLineOurs, statuslineMarker, true},

		"windows, ps1 wrapper": {"windows", StatusLineWrapped, winWrapper, true},
		"windows, sh wrapper":  {"windows", StatusLineWrapped, shWrapper, false},
		"linux, sh wrapper":    {"linux", StatusLineWrapped, "/home/dev/.claude/waired-statusline.sh", true},

		"windows, foreign": {"windows", StatusLineForeign, "~/mine.sh", false},
		"linux, foreign":   {"linux", StatusLineForeign, "~/mine.sh", false},
		"windows, none":    {"windows", StatusLineNone, "", false},
		"linux, none":      {"linux", StatusLineNone, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := StatusLineRunsOn(tc.goos, tc.kind, tc.cmd); got != tc.want {
				t.Errorf("StatusLineRunsOn(%q, %v, %q) = %v, want %v", tc.goos, tc.kind, tc.cmd, got, tc.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#787): a Windows host that an older waired
// wrapped with a `sh` script must be repaired by the next enable. Without this
// arm InstallStatusLine's wrapped case is an unconditional no-op, so re-running
// enable — the one thing status and doctor tell the operator to do — would
// change nothing at all.
//
// Driving installStatusLine("windows", ...) from a Linux runner is the point:
// CI's test jobs run on Linux only, so a Windows-only branch behind
// runtime.GOOS would never execute here.
func TestInstallStatusLineRewrapsAWrapperFromTheOtherOS(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shWrapper := statuslineWrapperPathFor("linux", home)
	if err := os.WriteFile(shWrapper, []byte(wrapperScriptSh), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statuslineOrigPath(home), []byte("~/my-statusline.sh --flag\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seed := `{"statusLine":{"type":"command","command":` + jsonQuote(shWrapper) +
		`},"waired_original_statusLine":{"type":"command","command":"~/my-statusline.sh --flag","padding":2}}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := installStatusLine("windows", home, false)
	if err != nil {
		t.Fatalf("installStatusLine: %v", err)
	}
	if res.Action != "rewrapped" {
		t.Fatalf("Action = %q, want rewrapped", res.Action)
	}

	wantCmd := statuslineWrapperCommandFor("windows", statuslineWrapperPathFor("windows", home))
	if got := statusLineCmd(t, home); got != wantCmd {
		t.Errorf("statusLine.command = %q, want %q", got, wantCmd)
	}
	if _, err := os.Stat(statuslineWrapperPathFor("windows", home)); err != nil {
		t.Errorf(".ps1 wrapper missing: %v", err)
	}
	if _, err := os.Stat(shWrapper); !os.IsNotExist(err) {
		t.Error(".sh wrapper survived the rewrap — two scripts, and no way to tell which one is live")
	}
	// The stash is the only lossless restore source; a rewrap must not touch it.
	stash, ok := readSettingsMap(t, home)[statuslineStashKey].(map[string]any)
	if !ok {
		t.Fatal("stash lost")
	}
	if stash["command"] != "~/my-statusline.sh --flag" || stash["padding"] != float64(2) {
		t.Errorf("rewrap damaged the stash: %v", stash)
	}

	// And it still restores.
	if err := RemoveStatusLine(home); err != nil {
		t.Fatalf("remove: %v", err)
	}
	sl := readSettingsMap(t, home)[statuslineKey].(map[string]any)
	if sl["command"] != "~/my-statusline.sh --flag" || sl["padding"] != float64(2) {
		t.Errorf("restored statusLine = %v", sl)
	}
}

// RECORD OF TODAY'S BEHAVIOUR: with nothing preserved to rebuild from, a rewrap
// declines rather than wrapping the wrapper. Rebuilding around an empty original
// would leave the user with waired's segment and none of their own statusline.
func TestInstallStatusLineDeclinesToRewrapWithNoPreservedOriginal(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"statusLine":{"type":"command","command":` + jsonQuote(statuslineWrapperPathFor("linux", home)) + `}}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := installStatusLine("windows", home, false)
	if err != nil {
		t.Fatalf("installStatusLine: %v", err)
	}
	if res.Action != "already-wrapped" {
		t.Errorf("Action = %q, want already-wrapped (declined)", res.Action)
	}
}

// PRODUCT CONTRACT (waired-agent#787): disable must clean up whichever wrapper
// spelling is on disk, not only the one this OS would write today.
func TestRemoveStatusLineDeletesBothWrapperSpellings(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh := statuslineWrapperPathFor("linux", home)
	ps1 := statuslineWrapperPathFor("windows", home)
	for _, p := range []string{sh, ps1} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seed := `{"statusLine":{"type":"command","command":` + jsonQuote(ps1) +
		`},"waired_original_statusLine":{"type":"command","command":"~/mine.sh"}}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStatusLine(home); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, p := range []string{sh, ps1} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived remove", filepath.Base(p))
		}
	}
}
