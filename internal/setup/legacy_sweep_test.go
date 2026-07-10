package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepLegacyManagedBlocks_RemovesAndPreservesUserContent(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	zshrc := filepath.Join(home, ".zshrc")
	fishrc := filepath.Join(home, ".config", "fish", "config.fish")
	if err := os.MkdirAll(filepath.Dir(fishrc), 0o755); err != nil {
		t.Fatal(err)
	}

	pre := "# user comment\nalias ll='ls -la'\n\n"
	post := "\nexport PATH=$PATH:/opt/x/bin\n"
	block := "# >>> waired managed (do not edit) >>>\n" +
		"_waired_env_file='/home/u/.config/waired/integrations/env.sh'\n" +
		"_waired_apply_env() { [ -f \"$_waired_env_file\" ] && . \"$_waired_env_file\"; }\n" +
		"_waired_apply_env\n" +
		"# <<< waired managed <<<\n"

	for _, p := range []string{bashrc, zshrc, fishrc} {
		if err := os.WriteFile(p, []byte(pre+block+post), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	changed, err := SweepLegacyManagedBlocks(home)
	if err != nil {
		t.Fatalf("SweepLegacyManagedBlocks: %v", err)
	}
	if len(changed) != 3 {
		t.Errorf("changed = %v, want 3 files", changed)
	}

	for _, p := range []string{bashrc, zshrc, fishrc} {
		body, _ := os.ReadFile(p)
		s := string(body)
		if strings.Contains(s, legacyManagedOpen) || strings.Contains(s, legacyManagedClose) {
			t.Errorf("%s: sentinel still present:\n%s", p, s)
		}
		if !strings.Contains(s, "alias ll='ls -la'") {
			t.Errorf("%s: pre-block user content lost:\n%s", p, s)
		}
		if !strings.Contains(s, "/opt/x/bin") {
			t.Errorf("%s: post-block user content lost:\n%s", p, s)
		}
	}
}

func TestSweepLegacyManagedBlocks_NoBlockNoOp(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	pre := "# unrelated rc\nexport FOO=bar\n"
	if err := os.WriteFile(bashrc, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := SweepLegacyManagedBlocks(home)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("expected no files changed, got %v", changed)
	}
	body, _ := os.ReadFile(bashrc)
	if string(body) != pre {
		t.Errorf("file modified despite no block:\n%s", body)
	}
}

func TestSweepLegacyManagedBlocks_MissingFilesIgnored(t *testing.T) {
	home := t.TempDir() // no rc files at all
	changed, err := SweepLegacyManagedBlocks(home)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("expected no changes when files absent, got %v", changed)
	}
}

func TestSweepLegacyManagedBlocks_PartialBlockLeftAlone(t *testing.T) {
	// Open marker without a matching close — leave the file untouched.
	// Destroying everything from open-to-EOF would be too eager.
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	body := "# >>> waired managed (do not edit) >>>\nexport FOO=bar\n# user line below\n"
	if err := os.WriteFile(bashrc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SweepLegacyManagedBlocks(home)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("expected no change for partial block, got %v", changed)
	}
	got, _ := os.ReadFile(bashrc)
	if string(got) != body {
		t.Errorf("file modified despite partial block:\n%s", got)
	}
}

func TestSweepLegacyManagedBlocks_EmptyHome(t *testing.T) {
	if _, err := SweepLegacyManagedBlocks(""); err == nil {
		t.Error("expected error for empty homeDir")
	}
}

func TestSweepLegacyManagedBlocks_OnlySomeFilesHaveBlock(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	zshrc := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(bashrc, []byte("# clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zshrc, []byte("# zsh prefix\n# >>> waired managed (do not edit) >>>\n_x=1\n# <<< waired managed <<<\n# zsh suffix\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := SweepLegacyManagedBlocks(home)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(changed) != 1 || changed[0] != zshrc {
		t.Errorf("expected only zshrc to be changed, got %v", changed)
	}
}
