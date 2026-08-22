package openclaw

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// writeBOM prepends a UTF-8 BOM to a file, the way Windows PowerShell 5.1's
// `Set-Content -Encoding utf8` and several Windows editors do.
func writeBOM(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, body...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestApply_ToleratesAUTF8BOM is the #1002 regression. OpenClaw reads such a
// config without complaint; waired used to fail the link with encoding/json's
// raw "invalid character 'ï'".
func TestApply_ToleratesAUTF8BOM(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)
	writeBOM(t, ConfigFile(opts.HomeDir))

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply over a BOM'd config: %v", err)
	}

	// The user's own key survived, and ours went in.
	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	if navDefaults(cfg)["model"] == nil {
		t.Error("the user's default model was lost")
	}
	load, _ := cfg["plugins"].(map[string]any)["load"].(map[string]any)
	if !hasInAnySlice(load["paths"], PluginDir(opts.HomeDir)) {
		t.Error("plugin path not registered")
	}

	// The mark is gone from what we wrote, and the copy taken first keeps it.
	body, err := os.ReadFile(ConfigFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(body), "\ufeff") {
		t.Error("waired wrote the BOM back into the config")
	}
	rec, _ := loadRec(t, opts)
	if rec.BackupPath == "" {
		t.Fatal("no backup taken of a config waired changed")
	}
	bak, err := os.ReadFile(rec.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(bak), "\ufeff") {
		t.Error("the backup does not hold the file as the user had it (BOM and all)")
	}

	// And it converges: the BOM is repaired once, not on every link.
	before := backupCount(t, opts.HomeDir)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := backupCount(t, opts.HomeDir); got != before {
		t.Errorf("re-linking after the BOM repair took another backup: %d, want %d", got, before)
	}
}

// TestUninstall_ToleratesAUTF8BOM: the same read is on the removal path, so a
// BOM used to make the integration impossible to remove as well as to apply.
func TestUninstall_ToleratesAUTF8BOM(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	writeBOM(t, ConfigFile(opts.HomeDir))

	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall over a BOM'd config: %v", err)
	}
	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	if _, ok := cfg["plugins"]; ok {
		t.Errorf("our keys survived the uninstall: %v", cfg)
	}
	if navDefaults(cfg)["model"] == nil {
		t.Error("the user's default model was lost by the uninstall")
	}
}

// TestAudit_ToleratesAUTF8BOM: the audit row feeds `waired doctor`, which is
// exactly where someone would go after a link failed.
func TestAudit_ToleratesAUTF8BOM(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	seedUserConfig(t, opts.HomeDir)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	writeBOM(t, ConfigFile(opts.HomeDir))

	findings, err := a.Audit(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if strings.Contains(f.Detail, "invalid character") {
			t.Errorf("audit reports a parse failure over a BOM: %+v", f)
		}
	}
}

// TestReadConfigObject_BrokenJSONSaysWhatToDo: a config waired cannot parse is
// the user's file, and the message has to say so — the old one was
// encoding/json's raw text naming a character that is not in their editor.
func TestReadConfigObject_BrokenJSONSaysWhatToDo(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/openclaw.json"
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, existed, err := readConfigObject(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !existed {
		t.Error("a present-but-unparseable file must still read as existing")
	}
	for _, want := range []string{path, "not valid JSON", "waired has not changed it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

// TestReadConfigObject_BOMOnlyFileIsEmptyNotBroken: a file holding nothing but
// the mark is the empty config, not a parse failure.
func TestReadConfigObject_BOMOnlyFileIsEmptyNotBroken(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/openclaw.json"
	if err := os.WriteFile(path, []byte{0xEF, 0xBB, 0xBF}, 0o644); err != nil {
		t.Fatal(err)
	}
	m, _, existed, err := readConfigObject(path)
	if err != nil {
		t.Fatalf("BOM-only file: %v", err)
	}
	if !existed {
		t.Error("the file exists")
	}
	if len(m) != 0 {
		t.Errorf("map = %v, want empty", m)
	}
}

// TestConfigHasForeignKeys_ToleratesAUTF8BOM: Detect keys the config-dir
// signal on this. A BOM used to make it read as "real user content" for the
// wrong reason — a parse error — which is the self-poisoning case waired#753
// exists to avoid.
func TestConfigHasForeignKeys_ToleratesAUTF8BOM(t *testing.T) {
	walkingBackupClock(t)
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	writeBOM(t, ConfigFile(opts.HomeDir))
	if configHasForeignKeys(opts.HomeDir) {
		t.Error("a BOM'd config holding only waired's own keys reads as foreign content")
	}

	// A real user key is still foreign, BOM or not. (readJSON is the test
	// helper, and it does not strip the mark — read past it here.)
	raw, err := os.ReadFile(ConfigFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw[3:], &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["somethingOfMine"] = true
	body, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(ConfigFile(opts.HomeDir), append([]byte{0xEF, 0xBB, 0xBF}, body...), 0o644); err != nil {
		t.Fatal(err)
	}
	if !configHasForeignKeys(opts.HomeDir) {
		t.Error("a BOM'd config with a user key does not read as foreign content")
	}
}
