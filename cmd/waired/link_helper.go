package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/integration/shellalias"
)

// helperPrintOptions captures the inputs printSetupHelper needs. Kept narrow
// so the helper is easy to drive from tests.
type helperPrintOptions struct {
	HomeDir     string
	WiredBinary string
	Interactive bool
}

// printSetupHelper dispatches to the per-target setup helper. Since the
// transparent proxy became the Claude routing method on Linux, these helpers
// only print informational next-steps — they never modify the user's shell rc
// files or IDE settings.
func printSetupHelper(target string, opts helperPrintOptions, out io.Writer, in io.Reader) {
	switch target {
	case "claude-code":
		printClaudeSetupHelper(opts, out, in)
	case "opencode":
		printOpenCodeSetupHelper(opts, out)
	case "openclaw":
		printOpenClawSetupHelper(opts, out)
	case "all", "":
		printClaudeSetupHelper(opts, out, in)
		printOpenCodeSetupHelper(opts, out)
		printOpenClawSetupHelper(opts, out)
	}
}

// printClaudeSetupHelper covers `waired link claude-code` (and the claude half
// of `waired link all`). The per-user integration now installs only the Claude
// Code skills; request routing is handled by Claude Code managed settings
// (ANTHROPIC_BASE_URL -> the local gateway; #488), not by a shell alias or
// VSCode wrapper, so this helper writes nothing — it just points at the
// `waired claude` command.
func printClaudeSetupHelper(_ helperPrintOptions, out io.Writer, _ io.Reader) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, bold("Claude Code integration:"))
	_, _ = fmt.Fprintln(out, "  - Skills installed under ~/.claude/skills/ (/doctor, /status helpers).")
	_, _ = fmt.Fprintln(out, "  - Claude request routing uses Claude Code managed settings: ANTHROPIC_BASE_URL")
	_, _ = fmt.Fprintln(out, "    points at your local inference (no credential, so your subscription and")
	_, _ = fmt.Fprintln(out, "    auto-mode are preserved), falling back to the real Anthropic API when local")
	_, _ = fmt.Fprintln(out, "    serving is unavailable.")
	_, _ = fmt.Fprintln(out, "      set up:  sudo waired claude enable      (done automatically by `waired init`)")
	_, _ = fmt.Fprintln(out, "      status:  waired claude status")
	_, _ = fmt.Fprintln(out, "      remove:  sudo waired claude disable")
}

// printOpenCodeSetupHelper is the OpenCode-specific final block. OpenCode loads
// the waired-authored plugin at ~/.config/opencode/plugin/waired.js, so once
// `waired link opencode` wrote it there is no follow-up config the user needs
// to install. The helper only confirms what happened and points at the tray
// for live status.
func printOpenCodeSetupHelper(_ helperPrintOptions, out io.Writer) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, bold("OpenCode integration:"))
	_, _ = fmt.Fprintln(out, "  - Plugin written to ~/.config/opencode/plugin/waired.js")
	_, _ = fmt.Fprintln(out, "    (registers the 'waired' provider). Restart opencode to pick it up.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Tip: the system tray shows live OpenCode integration status under")
	_, _ = fmt.Fprintln(out, "  \"OpenCode integration:\" — green dot = configured, amber = stale baseURL.")
}

// printOpenClawSetupHelper is the OpenClaw-specific final block. OpenClaw
// loads the waired-authored plugin at ~/.openclaw/plugins/waired/ and is
// pointed at it by a small openclaw.json merge (plugins.load.paths +
// plugins.entries.waired.enabled + the agents.defaults.models allowlist), so
// once `waired link openclaw` ran there is no follow-up config the user needs
// to install. The helper only confirms what happened and points at the tray
// for live status.
func printOpenClawSetupHelper(_ helperPrintOptions, out io.Writer) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, bold("OpenClaw integration:"))
	_, _ = fmt.Fprintln(out, "  - Plugin written to ~/.openclaw/plugins/waired/ and registered+enabled")
	_, _ = fmt.Fprintln(out, "    in ~/.openclaw/openclaw.json (provider 'waired', models waired/default|coding|small).")
	_, _ = fmt.Fprintln(out, "    Your default model is untouched; restart openclaw to pick it up.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Tip: the system tray shows live OpenClaw integration status under")
	_, _ = fmt.Fprintln(out, "  \"OpenClaw integration:\" — green dot = configured, amber = stale baseURL.")
}

// bestEffortUninstallShellAlias removes the legacy `waired claude` alias
// sentinel block from every detected rc file. Errors are swallowed
// (best-effort cleanup). Returns the count of files actually changed. This is
// the migration scrub: the alias is no longer written, but `waired unlink`
// still removes one an older install left behind (so an upgraded host's `claude`
// stops pointing at the removed `waired claude` wrapper).
func bestEffortUninstallShellAlias(homeDir string) int {
	if homeDir == "" {
		return 0
	}
	changed := 0
	for _, c := range shellalias.RCCandidates(homeDir) {
		if _, err := os.Stat(c.Path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if removed, _ := removeAliasSnippet(c.Path); removed {
			changed++
		}
	}
	return changed
}

// removeAliasSnippet strips the legacy alias block from path. Returns
// (removed, error).
func removeAliasSnippet(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	start, end, ok := shellalias.FindBlock(body)
	if !ok {
		return false, nil
	}
	out := append([]byte{}, body[:start]...)
	out = append(out, body[end:]...)
	return true, atomicWriteFile(path, collapseDoubleBlank(out), 0o644)
}

func collapseDoubleBlank(data []byte) []byte {
	for {
		i := strings.Index(string(data), "\n\n\n")
		if i < 0 {
			return data
		}
		data = append(data[:i+1], data[i+2:]...)
	}
}

// atomicWriteFile writes data via tmp+rename so a crashed write never leaves a
// half-edited rc file behind.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".waired-rc-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// wairedBinaryPath resolves the absolute path of the running waired binary.
// Returns "waired" as a fallback when the binary path is not resolvable.
func wairedBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "waired"
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe
}
