package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// newOpts builds an ApplyOptions pointing at a fresh tempdir-based
// $HOME and state dir for hermetic tests.
func newOpts(t *testing.T) integration.ApplyOptions {
	t.Helper()
	home := t.TempDir()
	state := t.TempDir()
	return integration.ApplyOptions{
		HomeDir:        home,
		StateDir:       state,
		GatewayBaseURL: "http://127.0.0.1:9473",
		GatewayToken:   strings.Repeat("a", 64),
		Force:          true, // detect-bypass; tests assert install behaviour
	}
}

func TestAdapter_ID(t *testing.T) {
	if got := New().ID(); got != integration.AgentClaudeCode {
		t.Fatalf("ID = %q", got)
	}
}

func TestApply_InstallsBothSkills(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, name := range []string{"waired-status", "waired-doctor"} {
		path := SkillFile(opts.HomeDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(body), "name: "+name) {
			t.Errorf("%s frontmatter missing name field:\n%s", path, body)
		}
		if !strings.Contains(string(body), "allowed-tools") {
			t.Errorf("%s missing allowed-tools field", path)
		}
	}
}

func TestApply_LedgerRecordsFiles(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := ledger.Get(integration.AgentClaudeCode)
	if !ok {
		t.Fatal("ledger missing claude-code record")
	}
	if len(rec.SkillFiles) != 2 || len(rec.SkillDirs) != 2 {
		t.Fatalf("ledger record incomplete: %+v", rec)
	}
	for _, f := range rec.SkillFiles {
		if !filepath.IsAbs(f) {
			t.Errorf("ledger SkillFiles entry not absolute: %s", f)
		}
	}
}

func TestApply_Idempotent(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	dir := SkillDir(opts.HomeDir, "waired-status")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 file in %s, got %d", dir, len(entries))
	}
}

func TestUninstall_RemovesEverythingLedgerSays(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	for _, name := range []string{"waired-status", "waired-doctor"} {
		if _, err := os.Stat(SkillFile(opts.HomeDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall", SkillFile(opts.HomeDir, name))
		}
		if _, err := os.Stat(SkillDir(opts.HomeDir, name)); !os.IsNotExist(err) {
			t.Errorf("dir %s survived uninstall", SkillDir(opts.HomeDir, name))
		}
	}

	// Ledger must no longer reference claude-code.
	paths, _ := integration.PathsFor(opts.StateDir)
	ledger, _ := integration.LoadLedger(paths.Ledger)
	if _, ok := ledger.Get(integration.AgentClaudeCode); ok {
		t.Error("ledger still has claude-code after uninstall")
	}
}

// TestUninstall_RevertsLedgerRecordedVSCodeConfig is the migration
// contract: this adapter no longer WRITES a VSCode wrapper, but Uninstall
// must still revert one that a pre-proxy install recorded in the ledger
// (via vscode.Remove), so an upgrader's IDE stops pointing at the removed
// `waired claude`. We seed a settings.json + a ledger AgentRecord whose
// VSCodeConfigs names our managed key, run Uninstall, and assert the key
// is gone while the user's own setting survives.
func TestUninstall_RevertsLedgerRecordedVSCodeConfig(t *testing.T) {
	a := New()
	opts := newOpts(t)

	// A settings.json containing one user key + one Waired-managed key.
	const managedKey = "claudeCode.claudeProcessWrapper"
	settings := filepath.Join(opts.HomeDir, ".config", "Code", "User", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "editor.fontSize": 14,
  "` + managedKey + `": "/usr/local/bin/waired claude"
}
`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed the ledger so Uninstall finds the recorded VSCode config.
	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Set(integration.AgentClaudeCode, integration.AgentRecord{
		VSCodeConfigs: []integration.ManagedJSONConfig{
			{Path: settings, AddedKeys: []string{managedKey}},
		},
	})
	if err := ledger.Save(paths.Ledger); err != nil {
		t.Fatal(err)
	}

	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("settings.json should survive (user key remains): %v", err)
	}
	if strings.Contains(string(got), managedKey) {
		t.Errorf("managed VSCode key not reverted; settings.json:\n%s", got)
	}
	if !strings.Contains(string(got), "editor.fontSize") {
		t.Errorf("user key was clobbered; settings.json:\n%s", got)
	}
}

func TestUninstall_FallbackWithEmptyLedger(t *testing.T) {
	// Hand-craft state: skills exist but ledger is empty (e.g. ledger
	// was deleted by the user). Uninstall must still clean up.
	a := New()
	opts := newOpts(t)

	// Manually plant skills (mimicking a successful Apply that lost
	// its ledger entry).
	if _, _, err := installSkills(opts.HomeDir); err != nil {
		t.Fatal(err)
	}

	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(SkillFile(opts.HomeDir, "waired-status")); !os.IsNotExist(err) {
		t.Error("fallback uninstall failed")
	}
}

func TestUninstall_PreservesUserSkillsInRoot(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// User has a skill alongside ours; uninstall must NOT remove ~/.claude/skills.
	userSkill := filepath.Join(SkillsRoot(opts.HomeDir), "user-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(userSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userSkill, []byte("---\nname: user-skill\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(userSkill); err != nil {
		t.Errorf("user skill clobbered: %v", err)
	}
	if _, err := os.Stat(SkillsRoot(opts.HomeDir)); err != nil {
		t.Errorf("skills root removed despite user skill: %v", err)
	}
}

func TestDetect_FoundViaConfigDir(t *testing.T) {
	a := New()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	det, err := a.Detect(context.Background(), integration.ApplyOptions{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if !det.Found {
		t.Fatalf("expected found via config dir, got %+v", det)
	}
}

func TestDetect_NotFound(t *testing.T) {
	a := New()
	home := t.TempDir()
	det, err := a.Detect(context.Background(), integration.ApplyOptions{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	// A `claude` binary may legitimately exist on the dev host (we are
	// Claude Code ourselves), so we only assert config-dir detection
	// here, not Found-false. If `claude` is on PATH, det.Found will
	// be true via the binary; that's expected.
	if det.ConfigDir != "" {
		t.Fatalf("unexpected config dir on empty home: %s", det.ConfigDir)
	}
}

func TestAudit_ReportsMissingSkills(t *testing.T) {
	a := New()
	opts := newOpts(t)
	findings, err := a.Audit(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var failCount int
	for _, f := range findings {
		if f.Status == integration.StatusFail {
			failCount++
		}
	}
	if failCount < 2 {
		t.Errorf("expected at least 2 fail findings, got %d: %+v", failCount, findings)
	}
}

func TestAudit_ReportsOKAfterApply(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	findings, err := a.Audit(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if strings.HasPrefix(f.Subject, "claude-code skill ") && f.Status != integration.StatusOK {
			t.Errorf("skill audit not OK: %+v", f)
		}
	}
}

// writeExecutable creates path (and parents) with the given mode.
func writeExecutable(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

// TestDetect_FoundViaLocalBin proves the per-user install location
// ~/.local/bin/claude (the native installer's default) is detected even
// when claude is not on PATH — the sudo / minimal-PATH context that
// used to silently skip the integration.
func TestDetect_FoundViaLocalBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics are unix-only")
	}
	t.Setenv("PATH", "") // hermetic: LookPath must miss
	a := New()
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "claude")
	writeExecutable(t, bin, 0o755)

	det, err := a.Detect(context.Background(), integration.ApplyOptions{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if !det.Found {
		t.Fatalf("expected found via ~/.local/bin, got %+v", det)
	}
	if det.BinaryPath != bin {
		t.Errorf("BinaryPath = %q, want %q", det.BinaryPath, bin)
	}
	if !hasNote(det.Notes, "not on PATH") {
		t.Errorf("notes = %v, want a 'not on PATH' note", det.Notes)
	}
}

// TestDetect_FoundViaClaudeLocal covers the claude-native install
// location ~/.claude/local/claude.
func TestDetect_FoundViaClaudeLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics are unix-only")
	}
	t.Setenv("PATH", "")
	a := New()
	home := t.TempDir()
	bin := filepath.Join(home, ".claude", "local", "claude")
	writeExecutable(t, bin, 0o755)

	det, err := a.Detect(context.Background(), integration.ApplyOptions{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if !det.Found {
		t.Fatalf("expected found via ~/.claude/local, got %+v", det)
	}
	if det.BinaryPath != bin {
		t.Errorf("BinaryPath = %q, want %q", det.BinaryPath, bin)
	}
}

// TestDetect_LocalBinNotExecutable proves a non-executable file at the
// well-known location does not count as an install.
func TestDetect_LocalBinNotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics are unix-only")
	}
	t.Setenv("PATH", "")
	a := New()
	home := t.TempDir()
	writeExecutable(t, filepath.Join(home, ".local", "bin", "claude"), 0o644)

	det, err := a.Detect(context.Background(), integration.ApplyOptions{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if det.Found {
		t.Fatalf("expected not found for non-executable file, got %+v", det)
	}
}

func hasNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}
