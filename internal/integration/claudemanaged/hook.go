package claudemanaged

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// The Claude Code Stop hook waired installs alongside ANTHROPIC_BASE_URL (#580).
// It fires after every assistant turn and lets `waired claude _fallback-hook`
// surface a user-visible `systemMessage` when that turn was served by the real
// Anthropic API because local inference errored and auto-mode fell back. This is
// the one built-in Claude Code channel (besides the statusline) that shows text
// *in the TUI*, so it is how waired keeps the fallback honest and non-silent
// (see waired/docs/decisions/, feedback: Claude integration must never break silently).
//
// It lives in managed-settings.json — not the user's ~/.claude/settings.json —
// because Stop hooks *array-merge* across every settings scope (managed included),
// so a managed entry fires without clobbering the user's own hooks, needs no
// per-user ownership hop, and is removed surgically by matching our command
// substring. On the Unixes the command self-guards on `command -v waired`, so an
// uninstalled binary leaves it a silent no-op rather than a "command not found"
// per turn; fallbackHookCommandFor says why Windows cannot carry that guard.

const (
	// fallbackHookMarker uniquely identifies waired's Stop-hook command inside
	// managed-settings.json so Remove strips only our entry.
	fallbackHookMarker = "waired claude _fallback-hook"
	// posixHookGuard is the `sh` guard every waired before waired-agent#787
	// wrote in front of the command, on every OS including Windows. Recognised
	// by its own text rather than as "anything that is not today's string", so
	// an operator's hand-edited command is not reported as broken.
	posixHookGuard = "command -v waired >/dev/null 2>&1 &&"
	// fallbackHookTimeout bounds how long Claude Code waits for the hook
	// (seconds). The hook's own mgmt call is far shorter; this is a backstop
	// against a hung agent stalling turn-end.
	fallbackHookTimeout = 5
)

// fallbackHookCommandFor is the shell command Claude Code runs on Stop, written
// for the shell that OS will hand it to.
//
// Which shell that is differs per OS, and Windows has no single answer: Claude
// Code passes a hook's command string to `sh -c` on macOS and Linux, and on
// Windows to Git Bash when Git Bash is installed or to PowerShell when it is
// not (https://code.claude.com/docs/en/hooks). So on Windows one string has to
// survive either shell, which rules out the POSIX guard — `command -v`,
// `>/dev/null 2>&1` and `|| true` are all meaningless to PowerShell, and a
// PowerShell-native guard would be equally meaningless to Git Bash. Writing the
// POSIX form there made the hook depend on Git Bash being installed, which
// Claude Code does not require (waired-agent#787).
//
//   - unix: the guard makes an uninstalled binary a clean no-op (exit 0, no
//     output), and `|| true` swallows any non-zero so it never blocks stop.
//   - windows: the bare command, which every candidate shell can run. Record of
//     today's behaviour, not a guarantee: with the binary gone the hook fails to
//     start and Claude Code shows one line of stderr per turn rather than
//     staying silent. `waired claude disable` (which uninstall.ps1 runs) deletes
//     the whole managed-settings file, so that only happens to a host whose
//     binary was removed without it.
func fallbackHookCommandFor(goos string) string {
	if goos == "windows" {
		return fallbackHookMarker
	}
	return posixHookGuard + " " + fallbackHookMarker + " || true"
}

// StopHookRunsOn reports whether cmd is a waired Stop-hook command the shell
// Claude Code starts hooks with on goos can actually run. It is false only for
// the pre-waired-agent#787 POSIX one-liner on Windows, where the string needs
// Git Bash and gets PowerShell whenever Git Bash is absent. A command that is
// not ours at all (including "") is not "runs" — callers ask StopHookCommandAt
// first and only pass a non-empty result.
func StopHookRunsOn(goos, cmd string) bool {
	if !strings.Contains(cmd, fallbackHookMarker) {
		return false
	}
	return goos != "windows" || !strings.Contains(cmd, posixHookGuard)
}

// newStopHookEntry builds a fresh managed-settings Stop-hook matcher entry
// carrying waired's command for goos. Stop ignores `matcher`, so it is omitted.
func newStopHookEntry(goos string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": fallbackHookCommandFor(goos),
				"timeout": fallbackHookTimeout,
			},
		},
	}
}

// ensureStopHook installs (or refreshes) waired's Stop hook in obj["hooks"].Stop,
// preserving any other hook events and entries. It removes any prior waired
// entry first so the command string always reflects the current version — which
// is also how a host carrying the pre-waired-agent#787 POSIX string on Windows
// picks up the runnable one: isWairedStopEntry matches on the marker both forms
// share, so the old entry is replaced rather than duplicated.
func ensureStopHook(goos string, obj map[string]any) {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	stop, _ := hooks["Stop"].([]any)
	kept := stop[:0:0]
	for _, e := range stop {
		if !isWairedStopEntry(e) {
			kept = append(kept, e)
		}
	}
	kept = append(kept, newStopHookEntry(goos))
	hooks["Stop"] = kept
	obj["hooks"] = hooks
}

// removeStopHook strips waired's Stop-hook entries from obj, collapsing an
// emptied Stop array and hooks object. Returns whether anything was removed.
func removeStopHook(obj map[string]any) bool {
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return false
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok {
		return false
	}
	kept := stop[:0:0]
	for _, e := range stop {
		if !isWairedStopEntry(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(stop) {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = kept
	}
	if len(hooks) == 0 {
		delete(obj, "hooks")
	} else {
		obj["hooks"] = hooks
	}
	return true
}

// isWairedStopEntry reports whether a Stop matcher entry contains waired's
// fallback-hook command (identified by fallbackHookMarker anywhere in a
// command string), tolerating the loose map/slice shapes JSON unmarshals to.
// Matching on the marker rather than the whole string is what keeps every
// command form waired has ever written — guarded or bare — recognisable to
// Remove and to ensureStopHook's refresh.
func isWairedStopEntry(entry any) bool { return wairedStopEntryCommand(entry) != "" }

// wairedStopEntryCommand returns the command string of waired's hook inside a
// Stop matcher entry, or "" when the entry is not ours.
func wairedStopEntryCommand(entry any) string {
	m, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return ""
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, fallbackHookMarker) {
			return cmd
		}
	}
	return ""
}

// StopHookInstalled reports whether managed-settings.json currently carries
// waired's Stop hook. Used by `waired claude status`. A missing / unparseable
// file reports false.
func StopHookInstalled() bool { return StopHookCommand() != "" }

// StopHookCommand returns the command string of waired's Stop hook as it stands
// in managed-settings.json, or "" when there is none. `waired claude status` and
// `waired doctor` need the string itself, not just its presence: an entry
// written for another OS's shell is installed and cannot run, and reporting only
// presence is what let that go unnoticed (waired-agent#787).
func StopHookCommand() string { return StopHookCommandAt(resolvePath()) }

// StopHookCommandAt is StopHookCommand against an explicit path, so callers
// outside this package can point it at a non-system location (the #604 reason
// ViewAt exists). An empty path (unsupported OS) reports "".
func StopHookCommandAt(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return ""
	}
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return ""
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok {
		return ""
	}
	i := slices.IndexFunc(stop, isWairedStopEntry)
	if i < 0 {
		return ""
	}
	return wairedStopEntryCommand(stop[i])
}
