// Package claudecode implements the Adapter for Claude Code.
//
// Since the transparent proxy became the Claude routing method on Linux
// (docs/decisions.md), this adapter installs ONLY the two slash-command
// skills under ~/.claude/skills/. It no longer writes any shell alias or
// VSCode `claudeProcessWrapper` — request routing is handled by the proxy,
// not by env injection. Uninstall still reverts any legacy VSCode wrapper a
// pre-proxy install recorded (via internal/integration/vscode.Remove) so an
// upgrader's IDE does not end up pointing at the removed `waired claude`.
package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/vscode"
)

// New returns the Claude Code adapter.
func New() integration.Adapter { return &adapter{} }

type adapter struct{}

func (a *adapter) ID() integration.AgentID { return integration.AgentClaudeCode }

// claudeUserInstallPaths are well-known per-user install locations,
// relative to $HOME. A plain PATH lookup misses these in sudo /
// minimal-PATH contexts (the native installer drops the binary in
// ~/.local/bin; `claude install` manages ~/.claude/local/claude), which
// used to make the integration silently skip on hosts that clearly
// have Claude Code.
var claudeUserInstallPaths = []string{
	".local/bin/claude",
	".claude/local/claude",
}

// Detect returns Found=true when the `claude` binary is on $PATH, at a
// well-known per-user install location under opts.HomeDir, or when
// ~/.claude exists. The config-dir check catches users who installed
// Claude Code via npm into a place that isn't on root-shell PATH but
// who have already initialised it (so $HOME/.claude/ is real).
func (a *adapter) Detect(_ context.Context, opts integration.ApplyOptions) (integration.Detection, error) {
	det := integration.Detection{}
	if path, ok := integration.LookPath("claude"); ok {
		det.Found = true
		det.BinaryPath = path
		det.Notes = append(det.Notes, fmt.Sprintf("claude on PATH: %s", path))
	}
	for _, rel := range claudeUserInstallPaths {
		path := integration.HomeJoin(opts.HomeDir, rel)
		if !integration.ExecutableFileExists(path) {
			continue
		}
		det.Found = true
		if det.BinaryPath == "" {
			det.BinaryPath = path
			det.Notes = append(det.Notes, fmt.Sprintf("claude at %s (not on PATH)", path))
		}
	}
	// Key the config-dir signal on content waired did NOT write. Apply
	// only ever creates ~/.claude/skills/, so a bare DirExists check on
	// ~/.claude self-poisons once applied (waired#753). A real Claude Code
	// install always leaves settings.json / .credentials.json / projects/
	// etc. — any entry other than skills/ marks it.
	configDir := integration.HomeJoin(opts.HomeDir, ".claude")
	if integration.ConfigDirHasForeignEntry(configDir, filepath.Base(SkillsRoot(opts.HomeDir))) {
		det.Found = true
		det.ConfigDir = configDir
		det.Notes = append(det.Notes, fmt.Sprintf("~/.claude has non-waired content: %s", configDir))
	}
	return det, nil
}

// Apply installs the skills and updates the ledger entry for claude-code. It
// writes nothing else: Claude request routing is handled by Claude Code managed
// settings (`waired claude enable`, ANTHROPIC_BASE_URL), not by this per-user
// adapter.
func (a *adapter) Apply(_ context.Context, opts integration.ApplyOptions) error {
	if opts.HomeDir == "" {
		return fmt.Errorf("claudecode: empty HomeDir")
	}
	if !opts.Force {
		// Adapter.Apply runs inside Manager.applyOne which has
		// already gated on Detect; defensive recheck only.
		det, err := a.Detect(context.Background(), opts)
		if err != nil {
			return err
		}
		if !det.Found {
			return &integration.AgentNotInstalledError{Agent: a.ID()}
		}
	}

	if err := os.MkdirAll(SkillsRoot(opts.HomeDir), 0o755); err != nil {
		return fmt.Errorf("claudecode: mkdir %s: %w", SkillsRoot(opts.HomeDir), err)
	}
	files, dirs, err := installSkills(opts.HomeDir)
	if err != nil {
		return err
	}

	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		return err
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		return err
	}
	ledger.Set(a.ID(), integration.AgentRecord{
		AppliedAt:  time.Now().UTC(),
		SkillFiles: files,
		SkillDirs:  dirs,
	})
	return ledger.Save(paths.Ledger)
}

// Audit reports whether each managed skill file is present and
// readable. Missing files surface as StatusFail; intact installs
// surface as StatusOK.
func (a *adapter) Audit(_ context.Context, opts integration.ApplyOptions) ([]integration.AuditFinding, error) {
	var out []integration.AuditFinding
	for _, e := range installedSkills() {
		path := SkillFile(opts.HomeDir, e.Name)
		fi, err := os.Stat(path)
		switch {
		case err == nil && fi.Mode().IsRegular():
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusOK,
				Subject: fmt.Sprintf("claude-code skill %s", e.Name),
				Detail:  path,
			})
		case os.IsNotExist(err):
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusFail,
				Subject: fmt.Sprintf("claude-code skill %s", e.Name),
				Detail:  fmt.Sprintf("missing: %s", path),
			})
		default:
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusFail,
				Subject: fmt.Sprintf("claude-code skill %s", e.Name),
				Detail:  fmt.Sprintf("stat %s: %v", path, err),
			})
		}
	}
	// Detect feedback (binary / config dir presence) surfaces too,
	// so the operator sees a single coherent block in `waired doctor`.
	det, err := a.Detect(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	switch {
	case det.Found:
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "claude-code installation",
			Detail:  fmt.Sprintf("binary=%s configDir=%s", det.BinaryPath, det.ConfigDir),
		})
	default:
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusSkip,
			Subject: "claude-code installation",
			Detail:  "claude binary not on PATH and ~/.claude is absent",
		})
	}
	return out, nil
}

// Uninstall reads the ledger to find the exact files / dirs Apply
// created and removes them. Fallback: if no ledger entry exists (the
// user ran Uninstall on a fresh install), best-effort removes the
// well-known skill paths anyway.
func (a *adapter) Uninstall(_ context.Context, opts integration.ApplyOptions) error {
	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		return err
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		return err
	}
	rec, _ := ledger.Get(a.ID())

	// Revert each VSCode settings.json edit before deleting the shim
	// (which is removed below as part of SkillFiles). Best-effort: a
	// failure on one file should not block skill removal.
	for _, vc := range rec.VSCodeConfigs {
		if err := vscode.Remove(vc); err != nil {
			integration.EffectiveLogger(opts.Logger).Warnf("claude-code: VSCode uninstall %s: %v", vc.Path, err)
		}
	}

	files := rec.SkillFiles
	dirs := rec.SkillDirs
	if len(files) == 0 && len(dirs) == 0 {
		// Best-effort fallback: synthesize the canonical set so we
		// can still clean up after a stripped / hand-edited ledger.
		for _, e := range installedSkills() {
			files = append(files, SkillFile(opts.HomeDir, e.Name))
			dirs = append(dirs, SkillDir(opts.HomeDir, e.Name))
		}
	}
	if err := removeSkills(files, dirs); err != nil {
		return err
	}

	// Try to clean up the empty `~/.claude/skills/` parent so we
	// don't leave a Waired-only artefact, but only when it really
	// is empty (other tools install skills there too).
	skillsRoot := SkillsRoot(opts.HomeDir)
	if entries, err := os.ReadDir(skillsRoot); err == nil && len(entries) == 0 {
		_ = os.Remove(skillsRoot)
	}

	ledger.Delete(a.ID())
	return ledger.Save(paths.Ledger)
}
