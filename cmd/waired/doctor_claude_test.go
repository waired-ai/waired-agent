package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

const (
	rc9POSIXStatuslineCommand = "command -v waired >/dev/null 2>&1 && exec waired claude statusline"
	bareStatuslineCommand     = "waired claude statusline"
	ps1WrapperCommand         = `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "C:/Users/dev/.claude/waired-statusline.ps1"`
	shWrapperCommand          = "/home/dev/.claude/waired-statusline.sh"
)

// PRODUCT CONTRACT (waired-agent#787): doctor must report a Claude Code entry
// waired wrote that this computer's shell cannot run. `waired doctor` "does not
// flag it either" is quoted in the issue as half the defect — status and doctor
// both judged by presence, so an inert integration looked healthy from every
// surface a user is told to check.
//
// The Stop hook rows are gone with the hook, which announced a fallback that no
// longer happens
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
// The statusline is the entry that remains.
//
// RECORD OF TODAY'S BEHAVIOUR: the severity is Warn, not Fail. runDoctorBody
// derives its non-zero exit from StatusFail alone, and an inert hook costs the
// host no inference — Claude Code still routes through waired. Failing would
// turn every rc9 Windows host into a non-zero `waired doctor`.
func TestClaudeCommandFindings(t *testing.T) {
	byRunnableForm := claudeDoctor{
		StatusLineKind: claudecode.StatusLineOurs,
		StatusLineCmd:  bareStatuslineCommand,
	}
	rc9Form := claudeDoctor{
		StatusLineKind: claudecode.StatusLineOurs,
		StatusLineCmd:  rc9POSIXStatuslineCommand,
	}

	cases := map[string]struct {
		goos    string
		in      claudeDoctor
		want    map[string]integration.Status // subject -> status; absent subject = no finding
		wantFix string                        // substring every Warn detail must carry
	}{
		"a host that never enabled Claude Code gets no rows": {
			goos: "windows", in: claudeDoctor{}, want: map[string]integration.Status{}},

		"linux, rc9 form is the right form there": {
			goos: "linux", in: rc9Form, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusOK,
			}},
		"darwin, rc9 form is the right form there": {
			goos: "darwin", in: rc9Form, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusOK,
			}},
		"windows, rc9 form is inert": {
			goos: "windows", in: rc9Form, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusWarn,
			}, wantFix: "Git Bash"},
		"windows, the runnable form": {
			goos: "windows", in: byRunnableForm, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusOK,
			}},

		"windows, hook rewritten but the statusline left behind": {
			goos: "windows", in: claudeDoctor{
				StatusLineKind: claudecode.StatusLineOurs,
				StatusLineCmd:  rc9POSIXStatuslineCommand,
			}, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusWarn,
			}, wantFix: "Git Bash"},

		"windows, a .sh wrapper left by an older waired": {
			goos: "windows", in: claudeDoctor{
				StatusLineKind: claudecode.StatusLineWrapped,
				StatusLineCmd:  shWrapperCommand,
			}, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusWarn,
			}, wantFix: "Git Bash"},
		"windows, the .ps1 wrapper": {
			goos: "windows", in: claudeDoctor{
				StatusLineKind: claudecode.StatusLineWrapped,
				StatusLineCmd:  ps1WrapperCommand,
			}, want: map[string]integration.Status{
				"claude-code statusline": integration.StatusOK,
			}},

		"a statusline the user owns is not doctor's to grade": {
			goos: "windows", in: claudeDoctor{
				StatusLineKind: claudecode.StatusLineForeign,
				StatusLineCmd:  "~/my-statusline.sh",
			}, want: map[string]integration.Status{}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := claudeCommandFindings(tc.goos, tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d: %+v", len(got), len(tc.want), got)
			}
			for _, f := range got {
				want, ok := tc.want[f.Subject]
				if !ok {
					t.Errorf("unexpected finding for %q", f.Subject)
					continue
				}
				if f.Status != want {
					t.Errorf("%s status = %v, want %v (detail: %s)", f.Subject, f.Status, want, f.Detail)
				}
				if f.Status == integration.StatusFail {
					t.Errorf("%s is Fail; a shell-form mismatch must not flip doctor's exit code", f.Subject)
				}
				if f.Detail == "" {
					t.Errorf("%s has no detail", f.Subject)
				}
				if f.Status == integration.StatusWarn && tc.wantFix != "" &&
					!strings.Contains(f.Detail, tc.wantFix) {
					t.Errorf("%s detail does not mention %q: %s", f.Subject, tc.wantFix, f.Detail)
				}
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#787): the fix a Windows warning names must be
// runnable as written. `sudo` does not exist there — printing it was
// waired#752. The statusline is a per-user file, so its fix is the plain
// per-user command rather than an elevated one.
func TestClaudeCommandFindingsSpellTheFixForTheHost(t *testing.T) {
	rc9 := claudeDoctor{
		StatusLineKind: claudecode.StatusLineOurs,
		StatusLineCmd:  rc9POSIXStatuslineCommand,
	}
	got := claudeCommandFindings("windows", rc9)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if strings.Contains(got[0].Detail, "sudo") {
		t.Errorf("windows detail tells the user to sudo: %s", got[0].Detail)
	}
	if !strings.Contains(got[0].Detail, "waired claude statusline install") {
		t.Errorf("windows detail does not name the command that fixes it: %s", got[0].Detail)
	}
}
