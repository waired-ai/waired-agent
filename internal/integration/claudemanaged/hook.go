package claudemanaged

import (
	"slices"
	"strconv"
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
	// fallbackHookMarker identifies the RETIRED Stop-hook command inside
	// managed-settings.json, so Write and Remove can strip a leftover from a
	// build that installed it. Nothing writes it any more: it announced a turn
	// that had fallen back to the real Anthropic API, and nothing falls back
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
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

	// stopHookEvent / sessionStartHookEvent are the two Claude Code hook
	// events waired installs on. Named rather than inlined so the generalised
	// helpers below read as "which event", not "which magic string".
	stopHookEvent         = "Stop"
	sessionStartHookEvent = "SessionStart"

	// refreshHookMarker identifies waired's SessionStart hook, which rewrites
	// this user's /model picker cache so the entries reflect the mesh as it is
	// now rather than as it was at `waired claude enable` time
	// (waired-agent#830).
	//
	// SessionStart, and only SessionStart, because of two facts measured on a
	// real host (docs/knowledges/20260820/0300-model-picker-measured-on-device.md):
	// Claude Code reads the picker cache once per process — re-opening /model
	// does not re-read it — and a SessionStart hook runs BEFORE that read, so
	// its write lands in the same session rather than the next one. A Stop hook
	// would rewrite the file after every assistant turn, for a value nothing
	// reads again until the next launch.
	refreshHookMarker = "waired claude _models-cache write --from-managed"

	// refreshHookTimeout bounds the refresh (seconds). It is the same backstop
	// fallbackHookTimeout is, against a bounded-but-slow mesh read delaying
	// session start; the write itself skips when nothing changed.
	refreshHookTimeout = 5
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
//
// hookCommandFor is the per-OS shell treatment above, generalised over the
// command so every hook waired installs gets it from one place. Extracted when
// the SessionStart refresh hook arrived (waired-agent#830): two hooks with two
// hand-written per-OS forms is how one of them ends up carrying the Windows
// regression waired-agent#787 fixed in the other.
func hookCommandFor(goos, marker string) string {
	if goos == "windows" {
		return marker
	}
	return posixHookGuard + " " + marker + " || true"
}

// hookRunsOn is StopHookRunsOn generalised over the marker.
func hookRunsOn(goos, cmd, marker string) bool {
	if !strings.Contains(cmd, marker) {
		return false
	}
	return goos != "windows" || !strings.Contains(cmd, posixHookGuard)
}

// newHookEntry builds a fresh managed-settings matcher entry carrying waired's
// command for goos. `matcher` is omitted: neither event waired hooks — Stop,
// SessionStart — uses it.
func newHookEntry(goos, marker string, timeout int) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCommandFor(goos, marker),
				"timeout": timeout,
			},
		},
	}
}

// ensureHook installs (or refreshes) one waired hook on one event, preserving
// every other event and every entry that is not ours.
//
// Array-merge across settings scopes is what makes this safe at all: a managed
// entry fires alongside the user's own hooks instead of replacing them, which
// is why waired's hooks live in managed-settings.json rather than in the user's
// settings.json.
func ensureHook(goos string, obj map[string]any, event, marker string, timeout int) {
	ensureHookWithCommand(goos, obj, event, marker, marker, timeout)
}

// ensureHookWithCommand is ensureHook where the command carries arguments
// beyond the marker. marker is what identifies OUR entry for replacement, so
// it must stay a substring of command — otherwise a refresh appends a second
// entry instead of replacing the first.
func ensureHookWithCommand(goos string, obj map[string]any, event, marker, command string, timeout int) {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	existing, _ := hooks[event].([]any)
	kept := existing[:0:0]
	for _, e := range existing {
		if entryCommand(e, marker) == "" {
			kept = append(kept, e)
		}
	}
	kept = append(kept, newHookEntry(goos, command, timeout))
	hooks[event] = kept
	obj["hooks"] = hooks
}

// removeStopHook strips waired's Stop-hook entries from obj, collapsing an
// emptied Stop array and hooks object. Returns whether anything was removed.
func removeStopHook(obj map[string]any) bool {
	return removeHook(obj, stopHookEvent, fallbackHookMarker)
}

// removeHook strips waired's entries for one event/marker pair, collapsing an
// emptied event array and an emptied hooks object. Reports whether anything was
// removed.
func removeHook(obj map[string]any, event, marker string) bool {
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return false
	}
	existing, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	kept := existing[:0:0]
	for _, e := range existing {
		if entryCommand(e, marker) == "" {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(existing) {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
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
func wairedStopEntryCommand(entry any) string { return entryCommand(entry, fallbackHookMarker) }

// entryCommand returns the command string inside a matcher entry whose command
// carries marker, or "" when the entry is not ours. Generalised over the marker
// so one implementation of the loose-JSON walk serves every hook.
func entryCommand(entry any, marker string) string {
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
		if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, marker) {
			return cmd
		}
	}
	return ""
}

// ensureRefreshHook installs (or refreshes) waired's SessionStart picker-cache
// hook. removeRefreshHook takes it back out — for `waired claude disable` and
// for the model-route-directives opt-out, where leaving a hook that maintains
// entries nobody advertises would be maintaining a lie.
// peerEntries is baked into the command rather than looked up by the hook.
// The hook runs unprivileged in the user's session and has no business reading
// a machine-wide agent.json — and this write is already happening in the
// elevated process that resolved the value, so embedding it keeps one reader.
func ensureRefreshHook(goos string, obj map[string]any, peerEntries int) {
	ensureHookWithCommand(goos, obj, sessionStartHookEvent, refreshHookMarker,
		refreshHookCommand(peerEntries), refreshHookTimeout)
}

// refreshHookCommand is the marker plus the arguments the hook needs. The
// marker stays a prefix of it, which is what keeps every command form this has
// ever written recognisable to removeRefreshHook and to ensureRefreshHook's own
// refresh — including one written before the peer cap existed.
func refreshHookCommand(peerEntries int) string {
	return refreshHookMarker + " --peer-entries " + strconv.Itoa(peerEntries)
}

func removeRefreshHook(obj map[string]any) bool {
	return removeHook(obj, sessionStartHookEvent, refreshHookMarker)
}

// RefreshHookCommandAt returns the command string of waired's SessionStart hook
// as it stands in managed settings, or "" when there is none. Same shape as
// StopHookCommandAt and for the same reason: `waired claude status` has to be
// able to say "installed, but not in the form this computer runs".
func RefreshHookCommandAt(path string) string {
	return hookCommandAt(path, sessionStartHookEvent, refreshHookMarker)
}

// RefreshHookRunsOn is StopHookRunsOn for the refresh hook.
func RefreshHookRunsOn(goos, cmd string) bool { return hookRunsOn(goos, cmd, refreshHookMarker) }

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
	return hookCommandAt(path, stopHookEvent, fallbackHookMarker)
}

// hookCommandAt is StopHookCommandAt generalised over the event and marker.
func hookCommandAt(path, event, marker string) string {
	if path == "" {
		return ""
	}
	obj, _, err := readSettingsObject(path)
	if err != nil || obj == nil {
		return ""
	}
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return ""
	}
	entries, ok := hooks[event].([]any)
	if !ok {
		return ""
	}
	i := slices.IndexFunc(entries, func(e any) bool { return entryCommand(e, marker) != "" })
	if i < 0 {
		return ""
	}
	return entryCommand(entries[i], marker)
}
