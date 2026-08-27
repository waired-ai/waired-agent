package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
// and waired/docs/decisions/); it is confined to the statusLine key and is fully
// restorable.
//
// On the Unixes the injected command self-guards on `command -v waired`, so an
// uninstalled binary yields an empty statusLine segment (Claude Code renders a
// blank footer on empty/error) rather than a broken one — matching the
// "invisible when uninstalled" requirement. Windows gets the bare command
// instead; statuslineRenderCommandFor says why, and lands in the same place
// because a command that cannot start also prints nothing.

const (
	// posixStatuslineGuard is the `sh` guard every waired before
	// waired-agent#787 wrote in front of the render command, on every OS
	// including Windows.
	posixStatuslineGuard = "command -v waired >/dev/null 2>&1 && exec"
	// statuslineMarker identifies a waired-owned bare statusLine command.
	statuslineMarker = "waired claude statusline"

	statuslineKey      = "statusLine"
	statuslineStashKey = "waired_original_statusLine"
	// statuslineWrapperStem is the wrapper script's name without its extension.
	// The extension is per-OS (statuslineWrapperNameFor) but the stem is not, so
	// classifyStatusLine recognises a wrapper this waired did not write —
	// including the `.sh` one a pre-waired-agent#787 waired left on a Windows
	// host, which has to stay restorable.
	statuslineWrapperStem = "waired-statusline"
	statuslineOrigStore   = "waired-statusline.orig"
)

// statuslineRenderCommandFor is the settings.json statusLine command for goos.
//
// Same split, and the same reason, as claudemanaged.fallbackHookCommandFor:
// Claude Code runs status-line commands through Git Bash when Git Bash is
// installed and through PowerShell when it is not
// (https://code.claude.com/docs/en/statusline). `exec` and `>/dev/null 2>&1`
// mean nothing to PowerShell, so writing the POSIX form on Windows made the
// segment depend on Git Bash being installed — which Claude Code does not
// require (waired-agent#787). Unlike hooks, statusLine has no `shell` field and
// no exec form to select with, so the Windows string has to be one both shells
// can run: the bare command.
func statuslineRenderCommandFor(goos string) string {
	if goos == "windows" {
		return statuslineMarker
	}
	return posixStatuslineGuard + " " + statuslineMarker
}

// StatusLineRunsOn reports whether a waired-owned statusLine command can be run
// by the shell Claude Code uses on goos. Like claudemanaged.StopHookRunsOn it is
// false only for the pre-waired-agent#787 POSIX form on Windows. A wrapper
// command is judged by its script's extension, which is what actually decides
// whether the interpreter exists.
func StatusLineRunsOn(goos string, kind StatusLineKind, cmd string) bool {
	if goos != "windows" {
		return kind == StatusLineOurs || kind == StatusLineWrapped
	}
	switch kind {
	case StatusLineOurs:
		return !strings.Contains(cmd, posixStatuslineGuard)
	case StatusLineWrapped:
		return strings.Contains(cmd, statuslineWrapperNameFor(goos))
	default:
		return false
	}
}

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

// statuslineWrapperNameFor names the wrapper script: a POSIX shell script where
// Claude Code has a POSIX shell, a PowerShell script on Windows.
func statuslineWrapperNameFor(goos string) string {
	if goos == "windows" {
		return statuslineWrapperStem + ".ps1"
	}
	return statuslineWrapperStem + ".sh"
}

func statuslineWrapperPathFor(goos, home string) string {
	return filepath.Join(home, ".claude", statuslineWrapperNameFor(goos))
}

// statuslineWrapperCommandFor renders the settings.json command that runs the
// wrapper at wrapperPath. It takes the path rather than (goos, home) so the
// Windows spelling can be asserted against a literal on a Linux runner.
//
// The Windows form is `powershell.exe -NoProfile -ExecutionPolicy Bypass -File
// "<forward-slashed path>"`, and every part of that is load-bearing:
//   - forward slashes, because Git Bash eats an unquoted backslash as an escape
//     and the path then reaches PowerShell with its separators missing
//     (https://code.claude.com/docs/en/statusline);
//   - quoted always, so a home directory with a space stays one argument in
//     Git Bash, PowerShell and cmd alike — one spelling, not three;
//   - `powershell.exe` spelled with the extension, the one form all three
//     resolve (`pwsh` is not guaranteed to exist);
//   - `-ExecutionPolicy Bypass`, because the default policy on Windows client
//     SKUs refuses to run an unsigned .ps1 by -File, which would leave the
//     wrapper written and silently failing every refresh;
//   - `-File` last, since everything after it is script arguments.
//
// The separator swap is strings.ReplaceAll and not filepath.ToSlash on purpose:
// ToSlash only rewrites when the RUNNING host's separator is a backslash, so on
// a Linux CI runner it would leave a Windows fixture path untouched and the
// assertion below would pass over a command that ships broken.
func statuslineWrapperCommandFor(goos, wrapperPath string) string {
	if goos == "windows" {
		return `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "` +
			strings.ReplaceAll(wrapperPath, `\`, "/") + `"`
	}
	return wrapperPath
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
	Action   string         // injected | refreshed | already-wrapped | rewrapped | wrapped | skipped-foreign
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
	kind, cmd := classifyStatusLine(m)
	return kind, cmd, nil
}

// InstallStatusLine ensures waired's statusLine segment is present, in the form
// this host's OS can run.
func InstallStatusLine(home string, wrap bool) (StatusLineResult, error) {
	return installStatusLine(runtime.GOOS, home, wrap)
}

// installStatusLine is InstallStatusLine with the OS as an argument, so all
// three are table-tested on one runner (CLAUDE.md §Test discipline).
//
//   - none    ⇒ inject the bare command.
//   - ours    ⇒ refresh the command (picks up a changed invocation, and is how
//     a host enabled before waired-agent#787 gets the runnable Windows form).
//   - wrapped ⇒ no-op when the wrapper is already this OS's; otherwise rebuild
//     it from the original we preserved. Without that second arm a Windows host
//     wrapped by an older waired keeps a `sh` script Claude Code cannot run
//     here, and no amount of re-running enable would ever fix it.
//   - foreign ⇒ if wrap, wrap the existing statusLine (marked, restorable);
//     otherwise leave it untouched and report skipped-foreign so the caller can
//     print guidance.
func installStatusLine(goos, home string, wrap bool) (StatusLineResult, error) {
	if home == "" {
		return StatusLineResult{}, errors.New("claudecode: empty home")
	}
	path := SettingsPath(home)
	m, err := readSettings(path)
	if err != nil {
		return StatusLineResult{}, err
	}
	kind, cmd := classifyStatusLine(m)
	res := StatusLineResult{Kind: kind, Existing: cmd, Path: path}
	switch kind {
	case StatusLineNone:
		m[statuslineKey] = ourStatusLineRaw(goos)
		res.Action = "injected"
	case StatusLineOurs:
		m[statuslineKey] = ourStatusLineRaw(goos)
		res.Action = "refreshed"
	case StatusLineWrapped:
		want := statuslineWrapperCommandFor(goos, statuslineWrapperPathFor(goos, home))
		if cmd == want {
			res.Action = "already-wrapped"
			return res, nil
		}
		// The stash is deliberately NOT rewritten: it already holds the user's
		// own statusLine object, and overwriting it with a wrapper command would
		// destroy the only lossless restore source.
		orig := preservedOriginalCommand(home, m)
		if orig == "" {
			res.Action = "already-wrapped"
			return res, nil
		}
		if err := writeWrapperScript(goos, home, orig); err != nil {
			return res, err
		}
		m[statuslineKey] = wrapperStatusLineRaw(goos, home)
		res.Action = "rewrapped"
	case StatusLineForeign:
		if !wrap {
			res.Action = "skipped-foreign"
			return res, nil
		}
		if err := writeWrapperScript(goos, home, cmd); err != nil {
			return res, err
		}
		m[statuslineStashKey] = m[statuslineKey] // lossless original for restore
		m[statuslineKey] = wrapperStatusLineRaw(goos, home)
		res.Action = "wrapped"
	}
	if err := writeSettings(path, m); err != nil {
		return res, err
	}
	return res, nil
}

// preservedOriginalCommand recovers the command a wrapped statusLine wraps: the
// sibling .orig file first (what the wrapper itself reads), then the settings
// stash. "" when neither survives, which is the one case a rewrap must decline
// — rebuilding a wrapper around nothing would erase the user's statusLine.
func preservedOriginalCommand(home string, m map[string]json.RawMessage) string {
	if b, err := os.ReadFile(statuslineOrigPath(home)); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	raw, ok := m[statuslineStashKey]
	if !ok {
		return ""
	}
	var obj statusLineObj
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return obj.Command
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
	kind, _ := classifyStatusLine(m)
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
		// Both spellings, not this OS's: since waired-agent#787 the wrapper's
		// extension is per-OS, so a host upgraded across that change can carry
		// the other one — and a name the current OS does not compute is a name
		// nothing would ever clean up.
		_ = os.Remove(statuslineWrapperPathFor("windows", home))
		_ = os.Remove(statuslineWrapperPathFor("linux", home))
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

// classifyStatusLine is deliberately OS-independent: it must recognise every
// form waired has ever written, on whatever machine is reading. Matching the
// wrapper by its stem rather than by this OS's full filename is what keeps a
// `.sh` wrapper left behind on Windows (or a `.ps1` inspected from a test on
// Linux) classified as ours — a wrapper we no longer recognise is one Remove
// leaves behind as if it were the user's own (waired-agent#787).
func classifyStatusLine(m map[string]json.RawMessage) (StatusLineKind, string) {
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
	case strings.Contains(cmd, statuslineWrapperStem):
		return StatusLineWrapped, cmd
	default:
		return StatusLineForeign, cmd
	}
}

func ourStatusLineRaw(goos string) json.RawMessage {
	b, _ := json.Marshal(statusLineObj{Type: "command", Command: statuslineRenderCommandFor(goos)})
	return b
}

func wrapperStatusLineRaw(goos, home string) json.RawMessage {
	cmd := statuslineWrapperCommandFor(goos, statuslineWrapperPathFor(goos, home))
	b, _ := json.Marshal(statusLineObj{Type: "command", Command: cmd})
	return b
}

// writeWrapperScript writes the .orig command store and the wrapper script that
// runs the user's original statusline and appends waired's segment, in the
// language goos can execute. Any wrapper left over from the other OS's spelling
// is swept, so a rewrapped host is not left with two scripts and no way to tell
// which one settings.json points at.
//
// The 0o755 mode matters only on the Unixes; on Windows a .ps1 is gated by the
// execution policy (which the wrapper command bypasses per process), not by a
// mode bit.
func writeWrapperScript(goos, home, origCommand string) error {
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("claudecode: mkdir %s: %w", dir, err)
	}
	if err := secrets.WriteFile(statuslineOrigPath(home), []byte(origCommand+"\n"), secrets.NonSecret); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", statuslineOrigPath(home), err)
	}
	dst := statuslineWrapperPathFor(goos, home)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(wrapperScriptFor(goos)), 0o755); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("claudecode: rename %s -> %s: %w", tmp, dst, err)
	}
	for _, other := range []string{"windows", "linux"} {
		if p := statuslineWrapperPathFor(other, home); p != dst {
			_ = os.Remove(p)
		}
	}
	return nil
}

func wrapperScriptFor(goos string) string {
	if goos == "windows" {
		return wrapperScriptPS1
	}
	return wrapperScriptSh
}

// wrapperScriptSh feeds the statusline stdin JSON to both the user's original
// command and `waired claude statusline`, appending waired's segment. It reads
// the original from the sibling .orig file (avoiding shell-quoting hazards) and
// self-guards on `command -v waired` so an uninstall degrades to just the
// original output.
const wrapperScriptSh = `#!/bin/sh
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

// wrapperScriptPS1 is wrapperScriptSh's Windows counterpart: same contract —
// the original's output, two spaces, waired's segment, no trailing newline, and
// just the original's output when waired is gone.
//
// It runs the preserved original in the shell its author wrote it for. Claude
// Code itself resolves Git Bash first and PowerShell second on Windows, so a
// statusLine already on the machine was authored against whichever of the two
// that machine has; guessing the other one would run it in a shell that cannot
// parse it. Reading stdin via [Console]::In rather than the `$input` automatic
// variable is deliberate — `$input` is consumed by the first enumeration and is
// empty for the second command.
//
// Finding Git Bash is the part that has to be done by install location rather
// than by name. `bash.exe` on a Windows PATH is C:\WINDOWS\system32\bash.exe —
// the WSL launcher — and Git for Windows does not add itself to PATH, so a
// `Get-Command bash.exe` lookup finds WSL on a machine that has Git Bash sitting
// right there. Handing a Git Bash statusline to WSL runs it in a different
// filesystem namespace, where it fails and the user's own output silently
// disappears (waired-agent#816). The lookup also has to fail closed: no Git Bash
// means PowerShell, never "whatever answers to that name".
const wrapperScriptPS1 = `# waired-managed Claude Code statusline wrapper (waired-agent#787).
# waired runs your original statusline and appends its routing segment.
# Restore/remove with: waired claude statusline remove   (or  waired claude disable)
# Your original command is preserved in waired-statusline.orig and in
# settings.json under "waired_original_statusLine".
$ErrorActionPreference = 'SilentlyContinue'
$dir = Split-Path -Parent $PSCommandPath
$orig = (Get-Content -LiteralPath (Join-Path $dir 'waired-statusline.orig') -Raw)
$payload = [Console]::In.ReadToEnd()
$out = ''
# Git Bash is found by where it is installed, never by the name on PATH:
# ` + "`" + `bash.exe` + "`" + ` there is C:\WINDOWS\system32\bash.exe, the WSL launcher, and
# Git for Windows does not put itself on PATH at all (waired-agent#816).
function Find-GitBash {
	$candidates = @()
	$git = Get-Command git.exe -ErrorAction SilentlyContinue
	if ($git) {
		$candidates += (Join-Path (Split-Path -Parent (Split-Path -Parent $git.Source)) 'bin\bash.exe')
	}
	$candidates += (Join-Path $env:ProgramFiles 'Git\bin\bash.exe')
	if (${env:ProgramFiles(x86)}) {
		$candidates += (Join-Path ${env:ProgramFiles(x86)} 'Git\bin\bash.exe')
	}
	if ($env:LOCALAPPDATA) {
		$candidates += (Join-Path $env:LOCALAPPDATA 'Programs\Git\bin\bash.exe')
	}
	foreach ($c in $candidates) {
		if ($c -and (Test-Path -LiteralPath $c -PathType Leaf)) { return $c }
	}
	return $null
}
if ($orig) {
	$orig = $orig.Trim()
	$bash = Find-GitBash
	if ($bash) { $out = ($payload | & $bash -c $orig) -join "` + "`" + `n" }
	else { $out = ($payload | & { Invoke-Expression $orig }) -join "` + "`" + `n" }
}
if (Get-Command waired -ErrorAction SilentlyContinue) {
	$seg = ($payload | & waired claude statusline) -join "` + "`" + `n"
	if ($seg) {
		if ($out) { $out = "$out  $seg" } else { $out = $seg }
	}
}
[Console]::Out.Write($out)
`

func readSettings(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claudecode: read %s: %w", path, err)
	}
	// A UTF-8 BOM is tolerated: Windows editors and PowerShell add one,
	// Claude Code reads such a file without complaint, and refusing it would
	// make waired the odd one out on the platform where it is easiest to
	// acquire (waired-agent#1067).
	b = bytes.TrimPrefix(b, utf8BOM)
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
