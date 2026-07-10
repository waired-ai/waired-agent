package codeui

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultModel is the model the bundled coding agent selects out of the box.
// It is a gateway catalog alias surfaced by the waired provider plugin
// (waired/default -> the agent's default coding model), so the UI opens
// pre-wired with no model picker or auth prompt.
const DefaultModel = "waired/default"

// defaultConfigJSON is the opencode.json seeded into the isolated config dir.
// It only sets the default model; the "waired" provider itself is registered
// by the plugin (plugin/waired.js) written via
// opencode.WritePluginInConfigDir. Keeping the provider in the plugin (not
// here) means the bundle and the user-facing `waired link opencode`
// integration share one source of truth and cannot drift.
const defaultConfigJSON = `{
  "$schema": "https://opencode.ai/config.json",
  "model": "` + DefaultModel + `"
}
`

// WriteDefaultConfig writes opencode.json (default model) into configDir,
// creating it if needed. Idempotent: the file is overwritten via tmp+rename
// so a half-written config is never observed.
func WriteDefaultConfig(configDir string) (string, error) {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", fmt.Errorf("codeui: mkdir %s: %w", configDir, err)
	}
	dst := filepath.Join(configDir, "opencode.json")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(defaultConfigJSON), 0o644); err != nil {
		return "", fmt.Errorf("codeui: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("codeui: rename %s -> %s: %w", tmp, dst, err)
	}
	return dst, nil
}
