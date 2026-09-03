package claudecode

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed templates/skill_status.md templates/skill_doctor.md
var skillTemplates embed.FS

// RouteSkillName is the RETIRED /waired-route slash-command skill (#580). It
// switched the machine-wide route between auto / waired / anthropic, and there
// is no route left to switch — a turn runs where its model id says
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313). Nothing installs it; the name survives
// so RemoveRouteSkill can clear one an earlier build left in a user's home.
const RouteSkillName = "waired-route"

// skillFiles maps the skill subdirectory name to the embedded template
// file. The on-disk filename inside each subdir is always SKILL.md per
// Claude Code skill convention.
var skillFiles = map[string]string{
	"waired-status": "templates/skill_status.md",
	"waired-doctor": "templates/skill_doctor.md",
}

// SkillsRoot returns the user-global Claude Code skills directory.
func SkillsRoot(home string) string {
	return filepath.Join(home, ".claude", "skills")
}

// SkillDir returns the directory for one waired skill.
func SkillDir(home, name string) string {
	return filepath.Join(SkillsRoot(home), name)
}

// SkillFile returns the SKILL.md path for one waired skill.
func SkillFile(home, name string) string {
	return filepath.Join(SkillDir(home, name), "SKILL.md")
}

// installedSkills returns the canonical list of (dirName, embedded
// template path) entries this package owns. Stable order so ledger
// updates are deterministic across runs.
func installedSkills() []skillEntry {
	return []skillEntry{
		{Name: "waired-status", Source: skillFiles["waired-status"]},
		{Name: "waired-doctor", Source: skillFiles["waired-doctor"]},
	}
}

type skillEntry struct {
	Name   string
	Source string
}

// installSkills writes every Waired skill into <home>/.claude/skills/.
// Returns (createdFiles, createdDirs) for the ledger.
func installSkills(home string) (files, dirs []string, err error) {
	for _, e := range installedSkills() {
		dir := SkillDir(home, e.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("claudecode: mkdir %s: %w", dir, err)
		}
		dirs = append(dirs, dir)

		body, err := skillTemplates.ReadFile(e.Source)
		if err != nil {
			return nil, nil, fmt.Errorf("claudecode: embed %s: %w", e.Source, err)
		}

		dst := SkillFile(home, e.Name)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return nil, nil, fmt.Errorf("claudecode: write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return nil, nil, fmt.Errorf("claudecode: rename %s -> %s: %w", tmp, dst, err)
		}
		files = append(files, dst)
	}
	return files, dirs, nil
}

// RemoveRouteSkill deletes the retired /waired-route skill file and its
// now-empty directory. Best-effort: a missing file is not an error. Called by
// both `waired claude enable` and `disable`, so a host upgrading past the
// retirement loses the command that no longer does anything.
func RemoveRouteSkill(home string) error {
	if home == "" {
		return fmt.Errorf("claudecode: empty home")
	}
	if err := os.Remove(SkillFile(home, RouteSkillName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("claudecode: remove %s: %w", SkillFile(home, RouteSkillName), err)
	}
	dir := SkillDir(home, RouteSkillName)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
	return nil
}

// removeSkills deletes every file Apply created for this adapter.
// Best-effort: missing files are not an error. Dirs are removed only
// when empty (so user-added skills don't get clobbered if they
// happened to live under one of our directory names).
func removeSkills(files, dirs []string) error {
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("claudecode: remove %s: %w", f, err)
		}
	}
	for _, d := range dirs {
		// Only remove if empty.
		entries, err := os.ReadDir(d)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("claudecode: read %s: %w", d, err)
		}
		if len(entries) == 0 {
			if err := os.Remove(d); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("claudecode: rmdir %s: %w", d, err)
			}
		}
	}
	return nil
}
