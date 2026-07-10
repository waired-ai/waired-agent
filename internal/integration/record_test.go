package integration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLedger_MissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	l, err := LoadLedger(filepath.Join(dir, "applied.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if l == nil {
		t.Fatal("nil ledger")
	}
	if l.Version != LedgerVersion {
		t.Fatalf("version = %d, want %d", l.Version, LedgerVersion)
	}
	if len(l.Agents) != 0 {
		t.Fatalf("expected empty agents, got %d", len(l.Agents))
	}
}

func TestLedger_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied.json")

	l, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	l.Set(AgentClaudeCode, AgentRecord{
		SkillFiles: []string{"/h/.claude/skills/waired-status/SKILL.md"},
		SkillDirs:  []string{"/h/.claude/skills/waired-status"},
	})
	l.Set(AgentOpenCode, AgentRecord{
		ConfigPath: "/h/.config/opencode/opencode.json",
		OwnedFully: false,
		AddedPaths: []string{"provider.waired", "model"},
		BackupPath: "/h/.config/opencode/opencode.json.waired-bak-1",
	})

	if err := l.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := LoadLedger(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Version != LedgerVersion {
		t.Fatalf("version = %d", got.Version)
	}
	cc, ok := got.Get(AgentClaudeCode)
	if !ok {
		t.Fatal("claude-code missing after reload")
	}
	if len(cc.SkillFiles) != 1 || cc.SkillDirs[0] != "/h/.claude/skills/waired-status" {
		t.Fatalf("claude-code record corrupt: %+v", cc)
	}
	oc, ok := got.Get(AgentOpenCode)
	if !ok {
		t.Fatal("opencode missing after reload")
	}
	if oc.ConfigPath != "/h/.config/opencode/opencode.json" || len(oc.AddedPaths) != 2 {
		t.Fatalf("opencode record corrupt: %+v", oc)
	}

	got.Delete(AgentOpenCode)
	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}

	final, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := final.Get(AgentOpenCode); ok {
		t.Fatal("opencode still present after Delete")
	}
}

// TestLedger_LegacyShellRCFilesIgnored verifies that ledgers written by
// v1 (which still had a `shell_rc_files` JSON key) load cleanly under
// v2: the unknown key is silently dropped during unmarshal, the rest of
// the document round-trips intact.
func TestLedger_LegacyShellRCFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied.json")

	legacy := []byte(`{
	  "version": 1,
	  "shell_rc_files": ["/home/u/.bashrc", "/home/u/.zshrc"],
	  "agents": {}
	}`)
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadLedger(path)
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if got.Agents == nil {
		t.Error("Agents nil")
	}
}
