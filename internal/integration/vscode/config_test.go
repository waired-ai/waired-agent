package vscode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// managedKey is the settings.json key a pre-proxy install used to write. We
// only ever Remove it now, so the tests seed it by hand the way an older
// install would have.
const managedKey = "claudeCode.claudeProcessWrapper"

func TestRemove_RevertsAddedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{
  // keep me
  "editor.fontSize": 14,
  "` + managedKey + `": "/usr/local/bin/waired claude"
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := integration.ManagedJSONConfig{Path: path, AddedKeys: []string{managedKey}}
	if err := Remove(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), managedKey) {
		t.Errorf("key not removed:\n%s", got)
	}
	if !strings.Contains(string(got), "// keep me") || !strings.Contains(string(got), "editor.fontSize") {
		t.Errorf("user content lost on uninstall:\n%s", got)
	}
}

func TestRemove_DeletesFileWhenItCollapses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// A file that contained ONLY our managed key (as if Waired created it
	// from scratch). Removing the key collapses it to {} and the file is
	// deleted.
	body := `{
  "` + managedKey + `": "/usr/local/bin/waired claude"
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := integration.ManagedJSONConfig{Path: path, AddedKeys: []string{managedKey}}
	if err := Remove(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("settings.json should be removed when it collapses to {}; stat err=%v", err)
	}
}

func TestRemove_MissingFileIsNoError(t *testing.T) {
	rec := integration.ManagedJSONConfig{
		Path:      filepath.Join(t.TempDir(), "does-not-exist.json"),
		AddedKeys: []string{managedKey},
	}
	if err := Remove(rec); err != nil {
		t.Fatalf("removing a missing file should be a no-op, got %v", err)
	}
}

func TestRemove_KeyAbsentIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{
  "editor.fontSize": 14
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := integration.ManagedJSONConfig{Path: path, AddedKeys: []string{managedKey}}
	if err := Remove(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("file changed despite key being absent:\n%s", got)
	}
}
