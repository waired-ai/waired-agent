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

func TestLogs_MaskPII(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 3 {
		t.Skip("no usable home dir to exercise masking")
	}
	dir := t.TempDir()
	logs := filepath.Join(dir, "runtimes", "ollama", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed an engine log that embeds the real home path, so masking has
	// something concrete to redact.
	line := "loaded model from " + home + "/models/foo.gguf"
	if err := os.WriteFile(filepath.Join(logs, "engine.log"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without --mask-pii, the home path passes through verbatim (control).
	plain := filepath.Join(dir, "plain.txt")
	_ = captureStdout(t, func() {
		if err := runLogs([]string{"-o", plain, "--state-dir", dir, "--since", "5m"}); err != nil {
			t.Fatalf("runLogs plain: %v", err)
		}
	})
	if b, _ := os.ReadFile(plain); !strings.Contains(string(b), home) {
		t.Fatalf("control: expected the home path to appear unmasked; got:\n%s", b)
	}

	// With --mask-pii, the home path is replaced by <home>.
	masked := filepath.Join(dir, "masked.txt")
	out := captureStdout(t, func() {
		if err := runLogs([]string{"-o", masked, "--state-dir", dir, "--since", "5m", "--mask-pii"}); err != nil {
			t.Fatalf("runLogs masked: %v", err)
		}
	})
	body, err := os.ReadFile(masked)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), home) {
		t.Errorf("masked bundle still contains the home path %q:\n%s", home, body)
	}
	if !strings.Contains(string(body), "<home>") {
		t.Errorf("masked bundle should contain the <home> token; got:\n%s", body)
	}
	if !strings.Contains(out, "Masked") {
		t.Errorf("stdout should note masking; got: %q", out)
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
