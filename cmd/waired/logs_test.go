package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogs_WritesBundleFile(t *testing.T) {
	dir := t.TempDir()
	// Seed an engine log so the bundle has deterministic content regardless
	// of whether journalctl exists / has a waired-agent unit on the runner.
	logs := filepath.Join(dir, "runtimes", "ollama", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "engine.log"), []byte("hello from ollama"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "bundle.txt")
	stdout := captureStdout(t, func() {
		if err := runLogs([]string{"--output", out, "--state-dir", dir, "--since", "10m"}); err != nil {
			t.Fatalf("runLogs: %v", err)
		}
	})
	if !strings.Contains(stdout, out) {
		t.Errorf("stdout should name the output file; got: %q", stdout)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "waired log bundle") {
		t.Errorf("bundle missing header; got:\n%s", body)
	}
	if !strings.Contains(body, "hello from ollama") {
		t.Errorf("bundle missing engine log; got:\n%s", body)
	}
}

func TestLogs_DefaultOutputName(t *testing.T) {
	dir := t.TempDir()
	// Run in a temp cwd so the default-named file lands somewhere clean.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()

	_ = captureStdout(t, func() {
		if err := runLogs([]string{"--state-dir", dir}); err != nil {
			t.Fatalf("runLogs: %v", err)
		}
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "waired-logs-") && strings.HasSuffix(e.Name(), ".txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a default-named waired-logs-*.txt file in %s", dir)
	}
}
