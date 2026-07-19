package main

import (
	"log/slog"
	"os"
	"path/filepath"
)

// migrateLegacyOllamaModels moves the blob store a pre-9475 bundled
// engine left behind. Before the engine exported OLLAMA_MODELS, a
// spawned `ollama serve` defaulted to $HOME/.ollama/models (the waired
// user's home, /var/lib/waired/.ollama/models) — same filesystem as
// the new waired-owned store, so a one-time rename saves re-pulling
// multi-GB models after the upgrade. Best-effort: on any failure the
// engine simply re-pulls into the new store.
//
// Only runs when the new store does not exist yet, so an already
// migrated (or fresh) install is a no-op.
func migrateLegacyOllamaModels(logger *slog.Logger, modelsDir, home string) {
	if _, err := os.Stat(modelsDir); err == nil {
		return // new store already exists — never overwrite
	}
	if home == "" {
		// Resolve the OS home when the caller injects none. Production passes
		// "" here, so runtime behaviour is identical to the prior direct
		// os.UserHomeDir() call; tests inject a temp dir instead. The home is
		// a parameter (not resolved unconditionally) because os.UserHomeDir()
		// reads %USERPROFILE% on Windows, not $HOME — so a test that only set
		// $HOME could never redirect it, and read the real profile (#82).
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return
		}
		home = h
	}
	legacy := filepath.Join(home, ".ollama", "models")
	fi, err := os.Stat(legacy)
	if err != nil || !fi.IsDir() {
		return // nothing to migrate
	}
	if err := os.MkdirAll(filepath.Dir(modelsDir), 0o755); err != nil {
		logger.Warn("legacy ollama model store migration failed; engine will re-pull",
			"from", legacy, "to", modelsDir, "err", err)
		return
	}
	if err := os.Rename(legacy, modelsDir); err != nil {
		logger.Warn("legacy ollama model store migration failed; engine will re-pull",
			"from", legacy, "to", modelsDir, "err", err)
		return
	}
	logger.Info("migrated legacy ollama model store", "from", legacy, "to", modelsDir)
}
