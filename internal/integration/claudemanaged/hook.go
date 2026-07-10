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
// (see docs/decisions.md, feedback: Claude integration must never break silently).
//
// It lives in managed-settings.json — not the user's ~/.claude/settings.json —
// because Stop hooks *array-merge* across every settings scope (managed included),
// so a managed entry fires without clobbering the user's own hooks, needs no
// per-user ownership hop, and is removed surgically by matching our command
// substring. The command self-guards on `command -v waired`, so an uninstalled
// binary leaves it a silent no-op rather than a "command not found" per turn.

const (
	// fallbackHookMarker uniquely identifies waired's Stop-hook command inside
	// managed-settings.json so Remove strips only our entry.
	fallbackHookMarker = "waired claude _fallback-hook"
	// fallbackHookTimeout bounds how long Claude Code waits for the hook
	// (seconds). The hook's own mgmt call is far shorter; this is a backstop
	// against a hung agent stalling turn-end.
	fallbackHookTimeout = 5
)

// fallbackHookCommand is the shell command Claude Code runs on Stop. The
// `command -v waired` guard makes it a clean no-op (exit 0, no output) when the
// binary is gone, and `|| true` swallows any non-zero so it never blocks stop.
func fallbackHookCommand() string {
	return "command -v waired >/dev/null 2>&1 && " + fallbackHookMarker + " || true"
}

// newStopHookEntry builds a fresh managed-settings Stop-hook matcher entry
// carrying waired's command. Stop ignores `matcher`, so it is omitted.
func newStopHookEntry() map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": fallbackHookCommand(),
				"timeout": fallbackHookTimeout,
			},
		},
	}
}

// ensureStopHook installs (or refreshes) waired's Stop hook in obj["hooks"].Stop,
// preserving any other hook events and entries. It removes any prior waired
// entry first so the command string always reflects the current version.
func ensureStopHook(obj map[string]any) {
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
	kept = append(kept, newStopHookEntry())
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
func isWairedStopEntry(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, fallbackHookMarker) {
			return true
		}
	}
	return false
}

// StopHookInstalled reports whether managed-settings.json currently carries
// waired's Stop hook. Used by `waired claude status`. A missing / unparseable
// file reports false.
func StopHookInstalled() bool {
	path := resolvePath()
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return false
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return false
	}
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return false
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok {
		return false
	}
	return slices.ContainsFunc(stop, isWairedStopEntry)
}
