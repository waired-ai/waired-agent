package codeui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// runtime.json records the live user-side instance so `open` is idempotent and
// `stop`/`status` can find it. It holds the capability token / basic password,
// so it is written 0600 and lives under the user-owned codeui dir.

func runtimePath(baseDir string) string { return filepath.Join(baseDir, "runtime.json") }

func readRuntime(baseDir string) (*RuntimeInfo, bool) {
	b, err := os.ReadFile(runtimePath(baseDir))
	if err != nil {
		return nil, false
	}
	var info RuntimeInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, false
	}
	if info.ProxyAddr == "" {
		return nil, false
	}
	return &info, true
}

func writeRuntime(baseDir string, info *RuntimeInfo) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("codeui: mkdir %s: %w", baseDir, err)
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("codeui: marshal runtime.json: %w", err)
	}
	dst := runtimePath(baseDir)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("codeui: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("codeui: rename %s -> %s: %w", tmp, dst, err)
	}
	return nil
}

func removeRuntime(baseDir string) { _ = os.Remove(runtimePath(baseDir)) }
