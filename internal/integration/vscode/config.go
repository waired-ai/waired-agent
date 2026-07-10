// Package vscode is the migration-scrub remnant of the old VSCode-family
// Claude Code wrapper integration.
//
// Since the transparent proxy became the sole Claude-routing method on
// Linux (docs/decisions.md), Waired no longer WRITES a VSCode
// `claudeProcessWrapper` setting — request routing is handled by the proxy,
// not by env injection inside the IDE. The only surviving surface is
// Remove: claudecode's Uninstall calls it to revert a settings.json edit a
// pre-proxy install recorded in the ledger, so an upgrader's IDE stops
// pointing at the removed `waired claude` wrapper. Everything else (Apply /
// the shim / variant discovery) has been deleted.
package vscode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tailscale/hujson"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// Remove reverts one ledger-recorded settings.json edit by deleting our
// recorded keys from the object, preserving everything else byte-for-byte.
// If the document collapses to an empty object (a file we created from
// scratch), it is removed. Missing files / keys are not an error.
func Remove(rec integration.ManagedJSONConfig) error {
	body, err := os.ReadFile(rec.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vscode: read %s: %w", rec.Path, err)
	}
	v, err := hujson.Parse(body)
	if err != nil {
		// Leave an unparseable user file alone; the backup is the escape
		// hatch.
		return fmt.Errorf("vscode: parse %s for uninstall: %w", rec.Path, err)
	}
	obj, ok := v.Value.(*hujson.Object)
	if !ok {
		return nil
	}

	want := make(map[string]bool, len(rec.AddedKeys))
	for _, k := range rec.AddedKeys {
		want[k] = true
	}
	kept := obj.Members[:0]
	changed := false
	for _, m := range obj.Members {
		if name, isStr := stringLiteral(m.Name.Value); isStr && want[name] {
			changed = true
			continue
		}
		kept = append(kept, m)
	}
	if !changed {
		return nil
	}
	obj.Members = kept

	out := v.Pack()
	if len(obj.Members) == 0 {
		if err := os.Remove(rec.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("vscode: remove %s: %w", rec.Path, err)
		}
		return nil
	}
	return writeAtomic(rec.Path, out)
}

// stringLiteral extracts the Go string from a hujson value when it is a
// JSON string literal; ok is false for any other kind.
func stringLiteral(vt hujson.ValueTrimmed) (string, bool) {
	lit, ok := vt.(hujson.Literal)
	if !ok || lit.Kind() != '"' {
		return "", false
	}
	return lit.String(), true
}

// writeAtomic writes data to path via tmp+rename (mode 0644), creating
// the parent directory if needed.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vscode: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".waired-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("vscode: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("vscode: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
