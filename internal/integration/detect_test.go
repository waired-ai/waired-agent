package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigDirHasForeignEntry covers the waired#753 primitive that lets
// Detect tell a real agent install from a config dir waired's own Apply
// pre-provisioned.
func TestConfigDirHasForeignEntry(t *testing.T) {
	mkdir := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("empty dir arg is false", func(t *testing.T) {
		if ConfigDirHasForeignEntry("", "plugin") {
			t.Fatal("empty dir arg must be false")
		}
	})
	t.Run("missing dir is false", func(t *testing.T) {
		if ConfigDirHasForeignEntry(filepath.Join(t.TempDir(), "nope"), "plugin") {
			t.Fatal("missing dir must be false")
		}
	})
	t.Run("empty dir is false", func(t *testing.T) {
		if ConfigDirHasForeignEntry(t.TempDir(), "plugin") {
			t.Fatal("empty dir must be false")
		}
	})
	t.Run("only owned entries is false", func(t *testing.T) {
		dir := t.TempDir()
		mkdir(t, filepath.Join(dir, "plugin"))
		mkdir(t, filepath.Join(dir, "commands"))
		if ConfigDirHasForeignEntry(dir, "plugin", "commands") {
			t.Fatal("only-owned entries must be false")
		}
	})
	t.Run("a foreign entry is true", func(t *testing.T) {
		dir := t.TempDir()
		mkdir(t, filepath.Join(dir, "plugin"))
		if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !ConfigDirHasForeignEntry(dir, "plugin", "commands") {
			t.Fatal("a foreign file must be true")
		}
	})
}
