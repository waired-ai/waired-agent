package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/shellalias"
)

// TestBestEffortUninstallShellAlias seeds a legacy alias block into two rc
// files (the alias is no longer written, so we plant it by hand the way an
// older install would have), then proves the migration scrub removes the
// sentinel block while preserving the user's own lines, and is idempotent.
func TestBestEffortUninstallShellAlias(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	zshrc := filepath.Join(home, ".zshrc")
	pre := "# user\nalias x='y'\n"
	block := shellalias.SentinelOpen + "\n" +
		`alias claude='/p/waired claude'` + "\n" + shellalias.SentinelClose + "\n"
	for _, p := range []string{bashrc, zshrc} {
		if err := os.WriteFile(p, []byte(pre+block), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := bestEffortUninstallShellAlias(home)
	if got != 2 {
		t.Errorf("uninstall touched %d files, want 2", got)
	}
	for _, p := range []string{bashrc, zshrc} {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), shellalias.SentinelOpen) {
			t.Errorf("%s still has alias sentinel after uninstall:\n%s", p, b)
		}
		if !strings.HasPrefix(string(b), pre) {
			t.Errorf("%s: pre-existing user content was lost; got:\n%s", p, b)
		}
	}

	// Second uninstall is a no-op.
	if got := bestEffortUninstallShellAlias(home); got != 0 {
		t.Errorf("second uninstall touched %d files, want 0", got)
	}
}

// TestBestEffortUninstallShellAlias_SkipsMissingRC: with no rc files at all
// the scrub is a clean no-op.
func TestBestEffortUninstallShellAlias_SkipsMissingRC(t *testing.T) {
	home := t.TempDir() // empty: no rc files
	if got := bestEffortUninstallShellAlias(home); got != 0 {
		t.Errorf("expected no changes when rc files missing, got %d", got)
	}
}

// TestRemoveAliasSnippet_NoBlockIsNoOp proves removeAliasSnippet reports
// (false, nil) and leaves the file untouched when no sentinel block is
// present.
func TestRemoveAliasSnippet_NoBlockIsNoOp(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	pre := "# only user content\nexport PATH=$PATH:/opt/x\n"
	if err := os.WriteFile(bashrc, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeAliasSnippet(bashrc)
	if err != nil {
		t.Fatalf("removeAliasSnippet: %v", err)
	}
	if removed {
		t.Errorf("removed=true on a file with no alias block")
	}
	b, _ := os.ReadFile(bashrc)
	if string(b) != pre {
		t.Errorf("file changed despite no block:\n%s", b)
	}
}

func TestCollapseDoubleBlank(t *testing.T) {
	cases := map[string]string{
		"a\n\n\nb\n":     "a\n\nb\n",
		"a\n\n\n\n\nb\n": "a\n\nb\n",
		"a\n\nb\n":       "a\n\nb\n", // single blank preserved
		"a\nb\n":         "a\nb\n",
	}
	for in, want := range cases {
		if got := string(collapseDoubleBlank([]byte(in))); got != want {
			t.Errorf("collapseDoubleBlank(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.txt")
	if err := atomicWriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "hello\n" {
		t.Errorf("content = %q", b)
	}
	fi, _ := os.Stat(path)
	// Windows has no Unix mode bits: os.Chmod maps only the read-only
	// attribute, so a 0600 file stats as 0666 there. The rest of the
	// contract — content, and no temp residue — is checked on all three
	// (same reading as internal/platform/secrets/secrets_test.go, #216).
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
	}
	// No temp residue left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("expected exactly the target file, got %d entries", len(entries))
	}
}

func TestWairedBinaryPath(t *testing.T) {
	// Always resolves to a non-empty absolute path (or the "waired"
	// fallback). We can't assert the exact path, only the invariant.
	got := wairedBinaryPath()
	if got == "" {
		t.Fatal("wairedBinaryPath returned empty")
	}
	if got != "waired" && !filepath.IsAbs(got) {
		t.Errorf("wairedBinaryPath = %q, want absolute path or \"waired\"", got)
	}
}

// TestPrintClaudeSetupHelper_ManagedSettingsStory verifies the Claude helper
// now describes the managed-settings integration (sudo waired claude enable /
// waired claude status) and never mentions the retired alias / VSCode wrapper /
// `waired proxy` command, and that it writes nothing to the user's rc files.
func TestPrintClaudeSetupHelper_ManagedSettingsStory(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("# user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printSetupHelper("claude-code", helperPrintOptions{
		HomeDir:     home,
		WiredBinary: "/p/waired",
		Interactive: false,
	}, &out, strings.NewReader(""))

	s := out.String()
	for _, want := range []string{
		"Claude Code integration:",
		"managed settings",
		elevatedCmdline(runtime.GOOS, "waired claude enable"),
		"waired claude status",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("claude helper missing %q; got:\n%s", want, s)
		}
	}
	for _, gone := range []string{
		"alias claude=",
		"claudeProcessWrapper",
		"claudeCode.claudeCommand",
		"waired proxy install",
		"transparent proxy",
	} {
		if strings.Contains(s, gone) {
			t.Errorf("claude helper still mentions retired %q; got:\n%s", gone, s)
		}
	}

	// The helper must never mutate dotfiles.
	body, _ := os.ReadFile(bashrc)
	if strings.Contains(string(body), shellalias.SentinelOpen) {
		t.Errorf(".bashrc was modified by the helper; got:\n%s", body)
	}
	if string(body) != "# user\n" {
		t.Errorf(".bashrc changed:\n%s", body)
	}
}

// TestPrintSetupHelper_OpenClawFinalBlock verifies that target=openclaw
// prints the OpenClaw-specific reminder (plugin written, tray shows
// status) without ever touching the shell rc files.
func TestPrintSetupHelper_OpenClawFinalBlock(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printSetupHelper("openclaw", helperPrintOptions{
		HomeDir:     home,
		WiredBinary: "/p/waired",
		Interactive: true,
	}, &out, strings.NewReader("y\n"))

	s := out.String()
	if !strings.Contains(s, "OpenClaw integration:") {
		t.Errorf("missing OpenClaw header:\n%s", s)
	}
	if strings.Contains(s, "alias claude=") {
		t.Errorf("openclaw helper should not print Claude alias snippet:\n%s", s)
	}
	body, _ := os.ReadFile(bashrc)
	if strings.Contains(string(body), shellalias.SentinelOpen) {
		t.Errorf(".bashrc was modified by openclaw helper:\n%s", body)
	}
}

// TestPrintSetupHelper_AllPrintsBoth verifies that target=all renders
// the Claude block followed by the OpenClaw block. This is the
// "waired link" / "waired link all" flow the user lands on by default.
func TestPrintSetupHelper_AllPrintsBoth(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printSetupHelper("all", helperPrintOptions{
		HomeDir:     home,
		WiredBinary: "/p/waired",
		Interactive: false,
	}, &out, strings.NewReader(""))

	s := out.String()
	claudeIdx := strings.Index(s, "Claude Code integration:")
	openClawIdx := strings.Index(s, "OpenClaw integration:")
	if claudeIdx < 0 || openClawIdx < 0 {
		t.Fatalf("expected both headers; claudeIdx=%d openClawIdx=%d\nfull:\n%s", claudeIdx, openClawIdx, s)
	}
	if claudeIdx > openClawIdx {
		t.Errorf("Claude header should appear before OpenClaw header; claudeIdx=%d openClawIdx=%d", claudeIdx, openClawIdx)
	}
}
