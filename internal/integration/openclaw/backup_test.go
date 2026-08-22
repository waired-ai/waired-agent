package openclaw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backupCount returns how many .waired-bak-* files sit next to openclaw.json.
func backupCount(t *testing.T, home string) int {
	t.Helper()
	matches, err := filepath.Glob(ConfigFile(home) + ".waired-bak-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return len(matches)
}

// walkingBackupClock puts every backup name in a distinct second. Without
// it a burst of applies inside one wall-clock second all produce the SAME
// file name, so "how many backups are left behind" passes whether or not
// the adapter skips the ones it should — the assertion would be measuring
// the filesystem's name collisions, not the code.
func walkingBackupClock(t *testing.T) {
	t.Helper()
	prev := backupClock
	n := 0
	backupClock = func() time.Time {
		n++
		return time.Unix(1700000000+int64(n), 0)
	}
	t.Cleanup(func() { backupClock = prev })
}

// seedUserConfig writes a config the user owns, so Apply has something real
// to merge into (and therefore something worth backing up).
func seedUserConfig(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(ConfigDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"agents": map[string]any{"defaults": map[string]any{
			"model": map[string]any{"primary": "openai/gpt-5.5"},
		}},
	}
	body, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(ConfigFile(home), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestApply_RepeatedApplyLeavesOneBackup is the #995 regression. Every Apply
// used to copy openclaw.json to a new .waired-bak-<unix-ts> because the
// backup was gated on "the file exists", not on "the merge changes it". A
// user who re-links — or clicks the tray's Reconfigure row, which runs
// `waired link openclaw --force --no-prompt` — collected one file per run in
// a directory another product owns.
func TestApply_RepeatedApplyLeavesOneBackup(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)

	for i := 1; i <= 4; i++ {
		if err := a.Apply(context.Background(), opts); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}
	if got := backupCount(t, opts.HomeDir); got != 1 {
		t.Errorf("after 4 applies over a user config: %d backups, want 1", got)
	}
}

// TestApply_FromNothingLeavesNoBackup: waired created the file itself, so
// there was never a user version to keep.
func TestApply_FromNothingLeavesNoBackup(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)

	for i := 1; i <= 3; i++ {
		if err := a.Apply(context.Background(), opts); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}
	if got := backupCount(t, opts.HomeDir); got != 0 {
		t.Errorf("after 3 applies on an empty home: %d backups, want 0", got)
	}
	rec, ok := loadRec(t, opts)
	if !ok {
		t.Fatal("no ledger record")
	}
	if rec.BackupPath != "" {
		t.Errorf("ledger names a backup that was never taken: %q", rec.BackupPath)
	}
}

// TestApply_ConvergedConfigIsNotRewritten: a config that already carries
// exactly what the adapter owns is left alone byte-for-byte. Rewriting it
// bumped the mtime on every run, which is what made "waired touched my
// config again" visible to file watchers.
func TestApply_ConvergedConfigIsNotRewritten(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before, err := os.ReadFile(ConfigFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	st1, err := os.Stat(ConfigFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	// A filesystem with coarse timestamps would hide a rewrite; push the
	// recorded mtime back so any rewrite is unmistakable.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(ConfigFile(opts.HomeDir), old, old); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	after, err := os.ReadFile(ConfigFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("converged config was rewritten:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	st2, err := os.Stat(ConfigFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(old) {
		t.Errorf("converged config mtime moved: %v -> %v (first apply was %v)",
			old, st2.ModTime(), st1.ModTime())
	}
}

// TestApply_ChangedConfigTakesExactlyOneMoreBackup: the other half. When the
// merge really does change the file, the copy is still taken — and it holds
// the file as the user left it, not as the merge will write it.
func TestApply_ChangedConfigTakesExactlyOneMoreBackup(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if got := backupCount(t, opts.HomeDir); got != 1 {
		t.Fatalf("after the first apply: %d backups, want 1", got)
	}

	// The user edits their config: they drop the plugin registration.
	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	plugins, _ := cfg["plugins"].(map[string]any)
	load, _ := plugins["load"].(map[string]any)
	load["paths"] = []any{}
	edited, _ := json.MarshalIndent(cfg, "", "  ")
	edited = append(edited, '\n')
	if err := os.WriteFile(ConfigFile(opts.HomeDir), edited, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("repair Apply: %v", err)
	}
	if got := backupCount(t, opts.HomeDir); got != 2 {
		t.Errorf("after the repairing apply: %d backups, want 2", got)
	}

	// And it converges again: the repair is not a new steady state that
	// keeps producing a file per run.
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("apply after the repair: %v", err)
	}
	if got := backupCount(t, opts.HomeDir); got != 2 {
		t.Errorf("after re-applying the repaired config: %d backups, want 2", got)
	}

	// The repair was applied.
	cfg = readJSON(t, ConfigFile(opts.HomeDir))
	load, _ = cfg["plugins"].(map[string]any)["load"].(map[string]any)
	if !hasInAnySlice(load["paths"], PluginDir(opts.HomeDir)) {
		t.Error("plugin path not re-registered by the repairing apply")
	}

	// The newest backup holds what the user had, i.e. WITHOUT the path.
	rec, _ := loadRec(t, opts)
	if rec.BackupPath == "" {
		t.Fatal("ledger does not name the backup the repair took")
	}
	var backed map[string]any
	raw, err := os.ReadFile(rec.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if err := json.Unmarshal(raw, &backed); err != nil {
		t.Fatalf("parse backup: %v", err)
	}
	bload, _ := backed["plugins"].(map[string]any)["load"].(map[string]any)
	if hasInAnySlice(bload["paths"], PluginDir(opts.HomeDir)) {
		t.Error("the backup holds the post-merge file, not the user's own")
	}
}

// TestApply_UnchangedApplyKeepsLedgerBackupPath: skipping the backup must not
// make the ledger forget where the user's original went. The record is the
// only thing that names it, and `waired unlink` reads it to say so.
func TestApply_UnchangedApplyKeepsLedgerBackupPath(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first, ok := loadRec(t, opts)
	if !ok || first.BackupPath == "" {
		t.Fatal("first apply recorded no backup")
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	second, _ := loadRec(t, opts)
	if second.BackupPath != first.BackupPath {
		t.Errorf("ledger backup_path after an unchanged apply = %q, want %q",
			second.BackupPath, first.BackupPath)
	}
}

// TestApply_UnchangedApplyDropsAVanishedBackupPath: if the user deleted the
// backup themselves, the ledger must not keep pointing at it.
func TestApply_UnchangedApplyDropsAVanishedBackupPath(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first, _ := loadRec(t, opts)
	if first.BackupPath == "" {
		t.Fatal("first apply recorded no backup")
	}
	if err := os.Remove(first.BackupPath); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	second, _ := loadRec(t, opts)
	if second.BackupPath != "" {
		t.Errorf("ledger still names a deleted backup: %q", second.BackupPath)
	}
}

// TestUninstall_KeepsTheBackup: removal is surgical — it puts the keys back
// but not the user's key order or formatting, so the copy taken before the
// first change is the only record of the file as they wrote it. It stays.
func TestUninstall_KeepsTheBackup(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec, _ := loadRec(t, opts)
	if rec.BackupPath == "" {
		t.Fatal("no backup to keep")
	}
	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(rec.BackupPath); err != nil {
		t.Errorf("uninstall removed the user's original config copy: %v", err)
	}
	if got := backupCount(t, opts.HomeDir); got != 1 {
		t.Errorf("after uninstall: %d backups, want 1", got)
	}
}

// TestBackupConfig_SameSecondKeepsBoth: the name has one-second resolution.
// Two backups of different content inside one second used to silently
// overwrite each other, so the older content — the one nobody else held —
// was the one lost.
func TestBackupConfig_SameSecondKeepsBoth(t *testing.T) {
	// Deliberately pinned, not walking: this is the collision path.
	prev := backupClock
	backupClock = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { backupClock = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")

	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstBak, err := backupConfig(path)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secondBak, err := backupConfig(path)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if firstBak == secondBak {
		t.Fatalf("both backups landed on the same name %q", firstBak)
	}
	got, err := os.ReadFile(firstBak)
	if err != nil {
		t.Fatalf("read first backup: %v", err)
	}
	if string(got) != "first\n" {
		t.Errorf("first backup was overwritten: %q", got)
	}
	got, err = os.ReadFile(secondBak)
	if err != nil {
		t.Fatalf("read second backup: %v", err)
	}
	if string(got) != "second\n" {
		t.Errorf("second backup content = %q", got)
	}
}

// TestBackupConfig_MissingFileIsNotAnError keeps the contract the Apply path
// relies on: nothing to copy is not a failure.
func TestBackupConfig_MissingFileIsNotAnError(t *testing.T) {
	bak, err := backupConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("backupConfig on a missing file: %v", err)
	}
	if bak != "" {
		t.Errorf("backup path for a missing file = %q, want empty", bak)
	}
}

// TestApply_ForeignEditKeepsWorkingAfterRepair guards the interaction between
// the new comparison and the legacy-ref pruning: a config seeded with the old
// waired/coding + waired/small refs is NOT converged, so it must be repaired
// (and backed up) exactly once, and be converged from then on.
func TestApply_ForeignEditKeepsWorkingAfterRepair(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	if err := os.MkdirAll(ConfigDir(opts.HomeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"agents": map[string]any{"defaults": map[string]any{"models": map[string]any{
			"waired/coding": map[string]any{},
			"waired/small":  map[string]any{},
		}}},
	}
	body, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(ConfigFile(opts.HomeDir), body, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if got := backupCount(t, opts.HomeDir); got != 1 {
		t.Fatalf("stale refs must be repaired once: %d backups, want 1", got)
	}
	models := navModels(readJSON(t, ConfigFile(opts.HomeDir)))
	if _, ok := models["waired/coding"]; ok {
		t.Error("legacy waired/coding survived the repair")
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := backupCount(t, opts.HomeDir); got != 1 {
		t.Errorf("repaired config is not converged: %d backups, want 1", got)
	}
}
