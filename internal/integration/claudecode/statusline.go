package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// Claude Code statusLine integration (#580).
//
// Unlike managed-settings.json (which waired owns and where the Stop hook lives,
// see internal/integration/claudemanaged), the statusLine is a *single-slot*
// value in the USER's ~/.claude/settings.json: only the highest-precedence scope
// that sets it applies, so it does not array-merge. waired therefore edits the
// user's file directly, per-user (via the sudo-user hop), and — where a foreign
// statusLine already exists — only after the user consents, wrapping rather than
// clobbering it. This narrowly overrides the historical "never touch
// ~/.claude/settings.json" posture (see internal/integration/claudecode/adapter.go
// and docs/decisions.md); it is confined to the statusLine key and is fully
// restorable.
//
// The injected command self-guards on `command -v waired`, so an uninstalled
// binary yields an empty statusLine segment (Claude Code renders a blank footer
// on empty/error) rather than a broken one — matching the "invisible when
// uninstalled" requirement.

const (
	// statuslineRenderCommand is the shell command written into settings.json.
	// The `command -v waired` guard makes it a clean no-op (empty output ⇒ blank
	// segment) once the binary is gone.
	statuslineRenderCommand = "command -v waired >/dev/null 2>&1 && exec waired claude statusline"
	// statuslineMarker identifies a waired-owned bare statusLine command.
	statuslineMarker = "waired claude statusline"

	statuslineKey       = "statusLine"
	statuslineStashKey  = "waired_original_statusLine"
	statuslineWrapper   = "waired-statusline.sh"
	statuslineOrigStore = "waired-statusline.orig"
)

// StatusLineKind classifies the current ~/.claude/settings.json statusLine.
type StatusLineKind int

const (
	// StatusLineNone: no statusLine is set.
	StatusLineNone StatusLineKind = iota
	// StatusLineOurs: a bare waired-injected statusLine command.
	StatusLineOurs
	// StatusLineWrapped: a waired wrapper script around a pre-existing statusLine.
	StatusLineWrapped
	// StatusLineForeign: the user's own statusLine — waired never edits it
	// without consent.
	StatusLineForeign
)

// SettingsPath is the user-global Claude Code settings file.
func SettingsPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }

func statuslineWrapperPath(home string) string {
	return filepath.Join(home, ".claude", statuslineWrapper)
}
func statuslineOrigPath(home string) string {
	return filepath.Join(home, ".claude", statuslineOrigStore)
}

// statusLineObj is the minimal shape we read/write. Claude Code only defines the
// "command" statusLine type; padding/refreshInterval on a wrapped-then-restored
// statusLine are preserved losslessly via the settings.json stash key, not this
// struct.
type statusLineObj struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command,omitempty"`
}

// StatusLineResult reports what InstallStatusLine did.
type StatusLineResult struct {
	Kind     StatusLineKind // state BEFORE the call
	Action   string         // injected | refreshed | already-wrapped | wrapped | skipped-foreign
	Existing string         // the pre-existing (foreign) command, when relevant
	Path     string         // settings.json path
}

// DetectStatusLine reports the current statusLine classification and, for a
// foreign one, its command. Used by `waired claude enable` to decide whether to
// prompt before editing.
func DetectStatusLine(home string) (StatusLineKind, string, error) {
	if home == "" {
		return StatusLineNone, "", errors.New("claudecode: empty home")
	}
	m, err := readSettings(SettingsPath(home))
	if err != nil {
		return StatusLineNone, "", err
	}
	kind, cmd := classifyStatusLine(home, m)
	return kind, cmd, nil
}

// InstallStatusLine ensures waired's statusLine segment is present.
//   - none    ⇒ inject the bare command.
//   - ours    ⇒ refresh the command (picks up a changed invocation).
//   - wrapped ⇒ no-op.
//   - foreign ⇒ if wrap, wrap the existing statusLine (marked, restorable);
//     otherwise leave it untouched and report skipped-foreign so the caller can
//     print guidance.
func InstallStatusLine(home string, wrap bool) (StatusLineResult, error) {
	if home == "" {
		return StatusLineResult{}, errors.New("claudecode: empty home")
	}
	path := SettingsPath(home)
	m, err := readSettings(path)
	if err != nil {
		return StatusLineResult{}, err
	}
	kind, cmd := classifyStatusLine(home, m)
	res := StatusLineResult{Kind: kind, Existing: cmd, Path: path}
	switch kind {
	case StatusLineNone:
		m[statuslineKey] = ourStatusLineRaw()
		res.Action = "injected"
	case StatusLineOurs:
		m[statuslineKey] = ourStatusLineRaw()
		res.Action = "refreshed"
	case StatusLineWrapped:
		res.Action = "already-wrapped"
		return res, nil
	case StatusLineForeign:
		if !wrap {
			res.Action = "skipped-foreign"
			return res, nil
		}
		if err := writeWrapperScript(home, cmd); err != nil {
			return res, err
		}
		m[statuslineStashKey] = m[statuslineKey] // lossless original for restore
		m[statuslineKey] = wrapperStatusLineRaw(home)
		res.Action = "wrapped"
	}
	if err := writeSettings(path, m); err != nil {
		return res, err
	}
	return res, nil
}

// RemoveStatusLine undoes InstallStatusLine: a bare waired statusLine is dropped;
// a wrapped one is restored to its original and the wrapper artifacts deleted; a
// foreign or absent statusLine is left untouched. Best-effort and idempotent.
func RemoveStatusLine(home string) error {
	if home == "" {
		return errors.New("claudecode: empty home")
	}
	path := SettingsPath(home)
	m, err := readSettings(path)
	if err != nil {
		return err
	}
	kind, _ := classifyStatusLine(home, m)
	switch kind {
	case StatusLineNone, StatusLineForeign:
		return nil
	case StatusLineOurs:
		delete(m, statuslineKey)
		delete(m, statuslineStashKey)
	case StatusLineWrapped:
		if stash, ok := m[statuslineStashKey]; ok {
			m[statuslineKey] = stash
		} else {
			delete(m, statuslineKey)
		}
		delete(m, statuslineStashKey)
		_ = os.Remove(statuslineWrapperPath(home))
		_ = os.Remove(statuslineOrigPath(home))
	}
	if len(m) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("claudecode: remove %s: %w", path, err)
		}
		return nil
	}
	return writeSettings(path, m)
}

func classifyStatusLine(home string, m map[string]json.RawMessage) (StatusLineKind, string) {
	raw, ok := m[statuslineKey]
	if !ok {
		return StatusLineNone, ""
	}
	var obj statusLineObj
	if json.Unmarshal(raw, &obj) != nil {
		return StatusLineForeign, "" // present but not our shape — never edit it
	}
	cmd := obj.Command
	switch {
	case strings.Contains(cmd, statuslineMarker):
		return StatusLineOurs, cmd
	case cmd == statuslineWrapperPath(home) || strings.Contains(cmd, statuslineWrapper):
		return StatusLineWrapped, cmd
	case cmd == "":
		return StatusLineForeign, cmd
	default:
		return StatusLineForeign, cmd
	}
}

func ourStatusLineRaw() json.RawMessage {
	b, _ := json.Marshal(statusLineObj{Type: "command", Command: statuslineRenderCommand})
	return b
}

func wrapperStatusLineRaw(home string) json.RawMessage {
	b, _ := json.Marshal(statusLineObj{Type: "command", Command: statuslineWrapperPath(home)})
	return b
}

// writeWrapperScript writes the .orig command store and the executable wrapper
// script that runs the user's original statusline and appends waired's segment.
func writeWrapperScript(home, origCommand string) error {
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("claudecode: mkdir %s: %w", dir, err)
	}
	if err := secrets.WriteFile(statuslineOrigPath(home), []byte(origCommand+"\n"), secrets.NonSecret); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", statuslineOrigPath(home), err)
	}
	dst := statuslineWrapperPath(home)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(wrapperScript), 0o755); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("claudecode: rename %s -> %s: %w", tmp, dst, err)
	}
	return nil
}

// wrapperScript feeds the statusline stdin JSON to both the user's original
// command and `waired claude statusline`, appending waired's segment. It reads
// the original from the sibling .orig file (avoiding shell-quoting hazards) and
// self-guards on `command -v waired` so an uninstall degrades to just the
// original output.
const wrapperScript = `#!/bin/sh
# waired-managed Claude Code statusline wrapper (#580).
# waired runs your original statusline and appends its routing segment.
# Restore/remove with: waired claude statusline remove   (or  waired claude disable)
# Your original command is preserved in waired-statusline.orig and in
# settings.json under "waired_original_statusLine".
_dir=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd)
_orig=$(cat "$_dir/waired-statusline.orig" 2>/dev/null)
_input=$(cat)
_out=$(printf '%s' "$_input" | sh -c "$_orig" 2>/dev/null)
if command -v waired >/dev/null 2>&1; then
	_seg=$(printf '%s' "$_input" | waired claude statusline 2>/dev/null)
	if [ -n "$_seg" ]; then
		_out="$_out  $_seg"
	fi
fi
printf '%s' "$_out"
`

func readSettings(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claudecode: read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("claudecode: %s is not a JSON object: %w", path, err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func writeSettings(path string, m map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("claudecode: mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("claudecode: marshal settings: %w", err)
	}
	if err := secrets.WriteFile(path, append(data, '\n'), secrets.NonSecret); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", path, err)
	}
	return nil
}
