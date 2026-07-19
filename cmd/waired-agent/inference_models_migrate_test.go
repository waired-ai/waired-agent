package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyOllamaModels_Renames(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".ollama", "models")
	if err := os.MkdirAll(filepath.Join(legacy, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "blobs", "sha256-x"), []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "runtimes", "ollama", "models")
	migrateLegacyOllamaModels(discardLogger(), target, home)

	if _, err := os.Stat(filepath.Join(target, "blobs", "sha256-x")); err != nil {
		t.Errorf("blob should have moved to the new store: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy store should be gone, stat err = %v", err)
	}
}

func TestMigrateLegacyOllamaModels_SkipsWhenTargetExists(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".ollama", "models")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "models")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	migrateLegacyOllamaModels(discardLogger(), target, home)

	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy store must not be touched when the target exists: %v", err)
	}
}

func TestMigrateLegacyOllamaModels_IgnoresMissingSource(t *testing.T) {
	home := t.TempDir()

	target := filepath.Join(t.TempDir(), "models")
	migrateLegacyOllamaModels(discardLogger(), target, home)

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("no migration should create the target, stat err = %v", err)
	}
}
