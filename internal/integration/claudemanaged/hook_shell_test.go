package claudemanaged

import (
	"strings"
	"testing"
)

// rc9POSIXHookCommand is verbatim what every waired up to v0.0.2-rc9 wrote into
// managed-settings.json on all three OSes, Windows included. Kept as a literal
// rather than built from the constants so a future edit to those constants
// cannot silently redefine what "the old form" means.
const rc9POSIXHookCommand = "command -v waired >/dev/null 2>&1 && waired claude _fallback-hook || true"

// PRODUCT CONTRACT (waired-agent#787): the Stop-hook command must be something
// the shell Claude Code starts hooks with on that OS can parse.
//
// Ratifying source: waired-agent#787, and the Claude Code hooks reference it
// cites — "The command string is passed to a shell: sh -c on macOS and Linux,
// Git Bash on Windows, or PowerShell when Git Bash isn't installed."
// (https://code.claude.com/docs/en/hooks). Windows therefore has no single
// shell to write for, which is why it gets a string both can run rather than a
// PowerShell translation of the POSIX one.
func TestFallbackHookCommandFor(t *testing.T) {
	cases := map[string]struct {
		goos      string
		want      string
		wantGuard bool
	}{
		"linux keeps the sh guard":   {"linux", rc9POSIXHookCommand, true},
		"darwin keeps the sh guard":  {"darwin", rc9POSIXHookCommand, true},
		"windows is the bare marker": {"windows", "waired claude _fallback-hook", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := fallbackHookCommandFor(tc.goos)
			if got != tc.want {
				t.Errorf("fallbackHookCommandFor(%q) = %q, want %q", tc.goos, got, tc.want)
			}
			if !strings.Contains(got, fallbackHookMarker) {
				t.Errorf("command %q lost the marker Remove matches on", got)
			}
			// Asserted separately from the literal so a reworded command still
			// has to answer the question the issue was about.
			for _, posixism := range []string{"command -v", ">/dev/null", "2>&1", "||"} {
				if has := strings.Contains(got, posixism); has != tc.wantGuard {
					t.Errorf("command %q contains %q = %v, want %v", got, posixism, has, tc.wantGuard)
				}
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#787): a host enabled before the fix must be
// reported as needing a rewrite — and reported that way ONLY on Windows, since
// the same string is correct on the Unixes.
func TestStopHookRunsOn(t *testing.T) {
	cases := map[string]struct {
		goos, cmd string
		want      bool
	}{
		"linux, no hook installed":  {"linux", "", false},
		"darwin, no hook installed": {"darwin", "", false},
		"windows, no hook":          {"windows", "", false},

		"linux, rc9 POSIX form":   {"linux", rc9POSIXHookCommand, true},
		"darwin, rc9 POSIX form":  {"darwin", rc9POSIXHookCommand, true},
		"windows, rc9 POSIX form": {"windows", rc9POSIXHookCommand, false},

		"linux, bare form":   {"linux", fallbackHookMarker, true},
		"darwin, bare form":  {"darwin", fallbackHookMarker, true},
		"windows, bare form": {"windows", fallbackHookMarker, true},

		// Someone else's Stop hook is not ours to judge, on any OS.
		"linux, a foreign hook":   {"linux", "echo hi", false},
		"windows, a foreign hook": {"windows", "echo hi", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := StopHookRunsOn(tc.goos, tc.cmd); got != tc.want {
				t.Errorf("StopHookRunsOn(%q, %q) = %v, want %v", tc.goos, tc.cmd, got, tc.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#787): a host carrying the rc9 command must end
// up with exactly one entry, in the new form, after the next write — not two.
// This is the whole migration story: nothing rewrites managed settings on its
// own, so `waired claude enable` has to be able to replace what it finds.
func TestEnsureStopHookReplacesThePreviousFormRatherThanDuplicating(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			obj := map[string]any{
				"hooks": map[string]any{
					"Stop": []any{
						// The rc9 entry.
						map[string]any{"hooks": []any{
							map[string]any{"type": "command", "command": rc9POSIXHookCommand, "timeout": 5},
						}},
						// An operator's own Stop hook, which must survive.
						map[string]any{"hooks": []any{
							map[string]any{"type": "command", "command": "notify-send done"},
						}},
					},
				},
			}
			ensureStopHook(goos, obj)

			stop := obj["hooks"].(map[string]any)["Stop"].([]any)
			if len(stop) != 2 {
				t.Fatalf("Stop has %d entries, want 2 (the operator's plus exactly one of ours)", len(stop))
			}
			if wairedStopEntryCommand(stop[0]) != "" {
				t.Error("the operator's entry was reordered away or replaced")
			}
			if got := wairedStopEntryCommand(stop[1]); got != fallbackHookCommandFor(goos) {
				t.Errorf("our entry = %q, want %q", got, fallbackHookCommandFor(goos))
			}

			if !removeStopHook(obj) {
				t.Fatal("removeStopHook reported nothing removed")
			}
			left := obj["hooks"].(map[string]any)["Stop"].([]any)
			if len(left) != 1 {
				t.Fatalf("after remove, Stop has %d entries, want the operator's 1", len(left))
			}
		})
	}
}
