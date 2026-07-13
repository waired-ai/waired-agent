//go:build linux || darwin

package secrets

import (
	"fmt"
	"os"
	"path/filepath"
)

func writeFile(path string, data []byte, s Sensitivity) error {
	mode := os.FileMode(0o644)
	if s == Secret {
		mode = 0o600
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("secrets: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("secrets: rename %s -> %s: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}

func secureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("secrets: mkdir %s: %w", path, err)
	}
	// MkdirAll respects umask, so re-chmod to guarantee 0o700 even when
	// the directory already existed.
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secrets: chmod %s: %w", path, err)
	}
	return nil
}
