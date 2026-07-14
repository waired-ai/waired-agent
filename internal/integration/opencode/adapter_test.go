package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// TestDetect_ConfigDirWairedOnly_NotDetected is the waired#753 regression:
// after a force-apply, ~/.config/opencode holds only plugin/ and commands/
// that waired created, so the config-dir signal must NOT fire. Asserting on
// ConfigDir (not Found) keeps it portable — `opencode` may be on the dev/CI
// host PATH and legitimately set Found via the binary branch.
func TestDetect_ConfigDirWairedOnly_NotDetected(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	det, err := a.Detect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if det.ConfigDir != "" {
		t.Fatalf("config dir must not read as installed with only waired's plugin/ + commands/, got %q", det.ConfigDir)
	}
}

// TestDetect_FoundViaUserConfig: a user's own opencode.json (waired never
// writes it) is the foreign entry that marks a real install.
func TestDetect_FoundViaUserConfig(t *testing.T) {
	a := New()
	home := t.TempDir()
	if err := os.MkdirAll(ConfigDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ConfigDir(home), "opencode.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	det, err := a.Detect(context.Background(), integration.ApplyOptions{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if det.ConfigDir != ConfigDir(home) {
		t.Fatalf("ConfigDir = %q, want %q", det.ConfigDir, ConfigDir(home))
	}
}

func TestApply_WritesPlugin(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	body, err := os.ReadFile(PluginFile(opts.HomeDir))
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"export const WairedPlugin",
		"config.provider.waired",
		`"@ai-sdk/openai-compatible"`,
		`baseURL: "http://127.0.0.1:9479/v1"`,
		`id: "waired/default"`,
		`id: "waired/coding"`,
		`id: "waired/small"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plugin missing %q:\n%s", want, s)
		}
	}

	// No opencode.json is ever written (the whole point of the plugin).
	if _, err := os.Stat(filepath.Join(ConfigDir(opts.HomeDir), "opencode.json")); !os.IsNotExist(err) {
		t.Errorf("opencode.json should not be created: err=%v", err)
	}

	for _, name := range []string{"waired-status", "waired-doctor"} {
		if _, err := os.Stat(CommandFile(opts.HomeDir, name)); err != nil {
			t.Errorf("command %s missing: %v", name, err)
		}
	}

	paths, _ := integration.PathsFor(opts.StateDir)
	ledger, _ := integration.LoadLedger(paths.Ledger)
	rec, ok := ledger.Get(integration.AgentOpenCode)
	if !ok {
		t.Fatal("ledger missing opencode entry")
	}
	if !rec.OwnedFully {
		t.Errorf("expected owned_fully=true, got %+v", rec)
	}
	if rec.ConfigPath != PluginFile(opts.HomeDir) {
		t.Errorf("ConfigPath = %s, want %s", rec.ConfigPath, PluginFile(opts.HomeDir))
	}
	if len(rec.SkillFiles) != 2 {
		t.Errorf("expected 2 SkillFiles, got %d", len(rec.SkillFiles))
	}
	for _, f := range rec.SkillFiles {
		if !filepath.IsAbs(f) {
			t.Errorf("non-absolute SkillFile path: %s", f)
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
	body, _ := os.ReadFile(PluginFile(opts.HomeDir))
	if !strings.Contains(string(body), `baseURL: "http://127.0.0.1:9479/v1"`) {
		t.Errorf("plugin baseURL wrong after re-apply:\n%s", body)
	}
}

func TestApply_DetectGate(t *testing.T) {
	a := New()
	opts := newOpts(t)
	opts.Force = false
	// Empty HomeDir → Detect=false → AgentNotInstalledError, UNLESS opencode
	// happens to be on the dev host's PATH, in which case Apply succeeds.
	err := a.Apply(context.Background(), opts)
	var nf *integration.AgentNotInstalledError
	if !errors.As(err, &nf) && err != nil {
		t.Fatalf("expected AgentNotInstalledError or success, got: %v", err)
	}
}

func TestUninstall_RemovesPluginAndCommands(t *testing.T) {
	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(PluginFile(opts.HomeDir)); !os.IsNotExist(err) {
		t.Errorf("plugin survived uninstall: %v", err)
	}
	for _, name := range []string{"waired-status", "waired-doctor"} {
		if _, err := os.Stat(CommandFile(opts.HomeDir, name)); !os.IsNotExist(err) {
			t.Errorf("command %s survived uninstall", name)
		}
	}
	paths, _ := integration.PathsFor(opts.StateDir)
	ledger, _ := integration.LoadLedger(paths.Ledger)
	if _, ok := ledger.Get(integration.AgentOpenCode); ok {
		t.Error("ledger still has opencode entry after uninstall")
	}
}

func TestUninstall_FallbackEmptyLedger(t *testing.T) {
	a := New()
	opts := newOpts(t)
	// Plant a plugin + commands without a ledger entry; uninstall must
	// still remove them via the canonical-path fallback.
	if _, err := installPlugin(opts.HomeDir, opts.GatewayBaseURL); err != nil {
		t.Fatal(err)
	}
	if _, err := installCommands(opts.HomeDir); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PluginFile(opts.HomeDir)); !os.IsNotExist(err) {
		t.Error("fallback uninstall did not remove plugin")
	}
	for _, name := range []string{"waired-status", "waired-doctor"} {
		if _, err := os.Stat(CommandFile(opts.HomeDir, name)); !os.IsNotExist(err) {
			t.Errorf("fallback uninstall did not remove %s", name)
		}
	}
}

func TestAudit_ReportsMissing(t *testing.T) {
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
	if fails < 3 {
		t.Errorf("expected ≥3 fail findings (plugin + 2 commands), got %d: %+v", fails, findings)
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
		if (f.Subject == "opencode plugin" || strings.HasPrefix(f.Subject, "opencode command ")) &&
			f.Status != integration.StatusOK {
			t.Errorf("expected OK for %q, got %s: %s", f.Subject, f.Status, f.Detail)
		}
	}
}
