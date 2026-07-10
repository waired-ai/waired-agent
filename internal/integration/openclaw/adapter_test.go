package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

func newOpts(t *testing.T) integration.ApplyOptions {
	t.Helper()
	return integration.ApplyOptions{
		HomeDir:        t.TempDir(),
		StateDir:       t.TempDir(),
		GatewayBaseURL: "http://127.0.0.1:9473",
		GatewayToken:   strings.Repeat("a", 64),
		Force:          true,
		NonInteractive: true,
	}
}

// readJSON parses a file into a generic map for assertions.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return m
}

func TestApply_WritesPluginAndConfig(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. plugin entry file content.
	body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatalf("read plugin entry: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"registerProvider",
		"resolveDynamicModel",
		"resolveSyntheticAuth",
		`BASE_URL = "http://127.0.0.1:9479/v1"`,
		`"waired/" + key`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plugin entry missing %q:\n%s", want, s)
		}
	}

	// 2. manifest + package.json present.
	for _, f := range []string{PluginManifestFile(opts.HomeDir), PluginPackageFile(opts.HomeDir)} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("plugin file missing: %s (%v)", f, err)
		}
	}

	// 3. openclaw.json registers + enables the plugin and allowlists models.
	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	plugins, _ := cfg["plugins"].(map[string]any)
	if plugins == nil {
		t.Fatalf("openclaw.json missing plugins block: %v", cfg)
	}
	load, _ := plugins["load"].(map[string]any)
	if !hasInAnySlice(load["paths"], PluginDir(opts.HomeDir)) {
		t.Errorf("plugins.load.paths missing %s: %v", PluginDir(opts.HomeDir), load)
	}
	entries, _ := plugins["entries"].(map[string]any)
	waired, _ := entries["waired"].(map[string]any)
	if en, _ := waired["enabled"].(bool); !en {
		t.Errorf("plugins.entries.waired.enabled not true: %v", entries)
	}
	models := navModels(cfg)
	for _, ref := range modelRefs() {
		if _, ok := models[ref]; !ok {
			t.Errorf("agents.defaults.models missing %q: %v", ref, models)
		}
	}
	// We MUST NOT touch the user's default model.
	if defaults := navDefaults(cfg); defaults != nil {
		if _, ok := defaults["model"]; ok {
			t.Errorf("adapter must not set agents.defaults.model: %v", defaults)
		}
	}

	// 4. ledger.
	paths, _ := integration.PathsFor(opts.StateDir)
	ledger, _ := integration.LoadLedger(paths.Ledger)
	rec, ok := ledger.Get(integration.AgentOpenClaw)
	if !ok {
		t.Fatal("ledger missing openclaw entry")
	}
	if rec.ConfigPath != ConfigFile(opts.HomeDir) {
		t.Errorf("ConfigPath = %s, want %s", rec.ConfigPath, ConfigFile(opts.HomeDir))
	}
	if len(rec.SkillFiles) != 3 {
		t.Errorf("expected 3 SkillFiles, got %d: %v", len(rec.SkillFiles), rec.SkillFiles)
	}
	if len(rec.AddedPaths) == 0 {
		t.Error("expected non-empty AddedPaths")
	}
}

func TestApply_Idempotent_NoDuplicatePath(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	load, _ := cfg["plugins"].(map[string]any)["load"].(map[string]any)
	paths, _ := load["paths"].([]any)
	count := 0
	for _, p := range paths {
		if p == PluginDir(opts.HomeDir) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("plugin dir appears %d times in load.paths, want 1: %v", count, paths)
	}
}

func TestApply_PreservesUserConfig(t *testing.T) {
	a := New()
	opts := newOpts(t)
	// Pre-seed openclaw.json with the user's own content: a gateway block,
	// a peer plugin entry, a peer model, and a chosen default model.
	if err := os.MkdirAll(ConfigDir(opts.HomeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"gateway": map[string]any{"port": float64(18789), "auth": map[string]any{"token": "secret-token"}},
		"plugins": map[string]any{
			"load":    map[string]any{"paths": []any{"/home/u/.openclaw/plugins/other"}},
			"entries": map[string]any{"other": map[string]any{"enabled": true}},
		},
		"agents": map[string]any{"defaults": map[string]any{
			"model":  map[string]any{"primary": "openai/gpt-5.5"},
			"models": map[string]any{"openai/gpt-5.5": map[string]any{}},
		}},
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(ConfigFile(opts.HomeDir), seedBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	// User's gateway block untouched.
	gw, _ := cfg["gateway"].(map[string]any)
	if gw == nil || gw["auth"].(map[string]any)["token"] != "secret-token" {
		t.Errorf("gateway block not preserved: %v", cfg["gateway"])
	}
	// User's default model untouched.
	if navDefaults(cfg)["model"].(map[string]any)["primary"] != "openai/gpt-5.5" {
		t.Errorf("user default model changed: %v", navDefaults(cfg)["model"])
	}
	// User's peer plugin + our plugin both present.
	load, _ := cfg["plugins"].(map[string]any)["load"].(map[string]any)
	if !hasInAnySlice(load["paths"], "/home/u/.openclaw/plugins/other") {
		t.Error("peer plugin path dropped")
	}
	if !hasInAnySlice(load["paths"], PluginDir(opts.HomeDir)) {
		t.Error("our plugin path missing")
	}
	entries, _ := cfg["plugins"].(map[string]any)["entries"].(map[string]any)
	if _, ok := entries["other"]; !ok {
		t.Error("peer plugin entry dropped")
	}
	if _, ok := entries["waired"]; !ok {
		t.Error("our plugin entry missing")
	}
	// Backup taken.
	rec, _ := loadRec(t, opts)
	if rec.BackupPath == "" {
		t.Error("expected a backup of the pre-existing config")
	}
	if _, err := os.Stat(rec.BackupPath); err != nil {
		t.Errorf("backup file missing: %v", err)
	}

	// Uninstall restores the user's config to exactly its peers.
	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	cfg = readJSON(t, ConfigFile(opts.HomeDir))
	entries, _ = cfg["plugins"].(map[string]any)["entries"].(map[string]any)
	if _, ok := entries["waired"]; ok {
		t.Error("waired entry survived uninstall")
	}
	if _, ok := entries["other"]; !ok {
		t.Error("peer plugin entry removed by uninstall")
	}
	models := navModels(cfg)
	if _, ok := models["waired/coding"]; ok {
		t.Error("waired model allowlist survived uninstall")
	}
	if _, ok := models["openai/gpt-5.5"]; !ok {
		t.Error("peer model allowlist removed by uninstall")
	}
	load, _ = cfg["plugins"].(map[string]any)["load"].(map[string]any)
	if hasInAnySlice(load["paths"], PluginDir(opts.HomeDir)) {
		t.Error("our plugin path survived uninstall")
	}
	if !hasInAnySlice(load["paths"], "/home/u/.openclaw/plugins/other") {
		t.Error("peer plugin path removed by uninstall")
	}
}

// TestApply_PrunesLegacyAutoModelRef verifies the migration: a config that an
// older waired version seeded with the now-renamed `waired/auto` ref has that
// stale key removed on re-link (and replaced by waired/default), while a
// user-owned model in the same map is preserved.
func TestApply_PrunesLegacyAutoModelRef(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := os.MkdirAll(ConfigDir(opts.HomeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"agents": map[string]any{"defaults": map[string]any{
			"models": map[string]any{
				"waired/auto":    map[string]any{}, // stale, pre-rename
				"openai/gpt-5.5": map[string]any{}, // user-owned
			},
		}},
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(ConfigFile(opts.HomeDir), seedBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	models := navModels(readJSON(t, ConfigFile(opts.HomeDir)))
	if _, ok := models["waired/auto"]; ok {
		t.Errorf("stale waired/auto ref not pruned on re-link: %v", models)
	}
	if _, ok := models["waired/default"]; !ok {
		t.Errorf("waired/default ref not added: %v", models)
	}
	if _, ok := models["openai/gpt-5.5"]; !ok {
		t.Errorf("user-owned model ref dropped: %v", models)
	}
}

func TestUninstall_RemovesPluginDir(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(PluginDir(opts.HomeDir)); !os.IsNotExist(err) {
		t.Errorf("plugin dir survived uninstall: %v", err)
	}
	// openclaw.json had ONLY our keys → should be removed entirely.
	if _, err := os.Stat(ConfigFile(opts.HomeDir)); !os.IsNotExist(err) {
		t.Errorf("self-owned openclaw.json should be removed when empty: %v", err)
	}
	paths, _ := integration.PathsFor(opts.StateDir)
	ledger, _ := integration.LoadLedger(paths.Ledger)
	if _, ok := ledger.Get(integration.AgentOpenClaw); ok {
		t.Error("ledger still has openclaw entry after uninstall")
	}
}

func TestApply_DetectGate(t *testing.T) {
	a := New()
	opts := newOpts(t)
	opts.Force = false
	err := a.Apply(context.Background(), opts)
	var nf *integration.AgentNotInstalledError
	if !errors.As(err, &nf) && err != nil {
		t.Fatalf("expected AgentNotInstalledError or success, got: %v", err)
	}
}

func TestAudit_MissingThenOK(t *testing.T) {
	a := New()
	opts := newOpts(t)
	findings, err := a.Audit(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var fails int
	for _, f := range findings {
		if f.Status == integration.StatusFail {
			fails++
		}
	}
	if fails < 2 {
		t.Errorf("expected ≥2 fail findings before apply, got %d: %+v", fails, findings)
	}

	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	findings, err = a.Audit(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if (f.Subject == "openclaw plugin" || f.Subject == "openclaw config") && f.Status != integration.StatusOK {
			t.Errorf("expected OK for %q, got %s: %s", f.Subject, f.Status, f.Detail)
		}
	}
}

// --- helpers ---

func hasInAnySlice(v any, want string) bool {
	arr, _ := v.([]any)
	for _, e := range arr {
		if e == want {
			return true
		}
	}
	return false
}

func navDefaults(cfg map[string]any) map[string]any {
	agents, _ := cfg["agents"].(map[string]any)
	if agents == nil {
		return nil
	}
	defaults, _ := agents["defaults"].(map[string]any)
	return defaults
}

func navModels(cfg map[string]any) map[string]any {
	d := navDefaults(cfg)
	if d == nil {
		return map[string]any{}
	}
	m, _ := d["models"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func loadRec(t *testing.T, opts integration.ApplyOptions) (integration.AgentRecord, bool) {
	t.Helper()
	paths, _ := integration.PathsFor(opts.StateDir)
	ledger, _ := integration.LoadLedger(paths.Ledger)
	return ledger.Get(integration.AgentOpenClaw)
}
