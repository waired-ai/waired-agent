package opencode

import (
	"embed"
	"fmt"
	"os"
)

//go:embed templates/command_status.md templates/command_doctor.md
var commandTemplates embed.FS

type commandEntry struct {
	Name   string // file basename without .md
	Source string // path inside the embedded FS
}

func installedCommands() []commandEntry {
	return []commandEntry{
		{Name: "waired-status", Source: "templates/command_status.md"},
		{Name: "waired-doctor", Source: "templates/command_doctor.md"},
	}
}

// installCommands renders the embedded templates into
// <home>/.config/opencode/commands/. Returns the list of files created
// for the ledger.
func installCommands(home string) ([]string, error) {
	dir := CommandsDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("opencode: mkdir %s: %w", dir, err)
	}
	var files []string
	for _, c := range installedCommands() {
		body, err := commandTemplates.ReadFile(c.Source)
		if err != nil {
			return nil, fmt.Errorf("opencode: embed %s: %w", c.Source, err)
		}
		dst := CommandFile(home, c.Name)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return nil, fmt.Errorf("opencode: write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return nil, fmt.Errorf("opencode: rename %s -> %s: %w", tmp, dst, err)
		}
		files = append(files, dst)
	}
	return files, nil
}

// removeCommands deletes the recorded command files. Best-effort:
// missing files are not an error. The commands/ directory is removed
// only when empty so user-added commands stay put.
func removeCommands(files []string, home string) error {
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("opencode: remove %s: %w", f, err)
		}
	}
	dir := CommandsDir(home)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
	return nil
}
