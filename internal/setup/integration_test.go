package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// fakeAdapter is the same shape as the unit-test fake in
// internal/integration/manager_test.go but rebuilt here so the setup
// tests don't depend on test-only exports of another package.
type fakeAdapter struct {
	id        integration.AgentID
	detect    integration.Detection
	applyErr  error
	uninstErr error

	applyCalls  int
	uninstCalls int
}

func (f *fakeAdapter) ID() integration.AgentID { return f.id }
func (f *fakeAdapter) Detect(_ context.Context, _ integration.ApplyOptions) (integration.Detection, error) {
	return f.detect, nil
}
func (f *fakeAdapter) Apply(_ context.Context, _ integration.ApplyOptions) error {
	f.applyCalls++
	return f.applyErr
}
func (f *fakeAdapter) Audit(_ context.Context, _ integration.ApplyOptions) ([]integration.AuditFinding, error) {
	return nil, nil
}
func (f *fakeAdapter) Uninstall(_ context.Context, _ integration.ApplyOptions) error {
	f.uninstCalls++
	return f.uninstErr
}

// newOpts builds an isolated environment for one Integration() call: a
// fresh home and a fresh state dir.
//
// Both are per-test temp dirs, and that is now the whole of it: the
// gateway token this exercises is read from and written to the state
// dir and nowhere else. It used to need a keychain swap as well —
// LoadOrCreateGatewayToken preferred a machine-global item and returned
// early on a hit, so on a developer Mac the call succeeded and the file
// simply was not there, which made "Integration creates the token"
// silently untestable on the machine editing the code (#386).
func newOpts(t *testing.T, fakes ...integration.Adapter) IntegrationOptions {
	t.Helper()
	home := t.TempDir()
	state := t.TempDir()
	return IntegrationOptions{
		HomeDir:        home,
		StateDir:       state,
		GatewayBaseURL: "http://127.0.0.1:9473",
		NonInteractive: true,
		Adapters:       fakes,
	}
}

// TestIntegration_LoadsTokenAndDispatches verifies the post-v2
// orchestration: load/create the gateway token and dispatch each
// adapter's Apply. The legacy rc/env-file write paths are gone.
func TestIntegration_LoadsTokenAndDispatches(t *testing.T) {
	a := &fakeAdapter{id: integration.AgentClaudeCode, detect: integration.Detection{Found: true}}
	opts := newOpts(t, a)

	res, err := Integration(context.Background(), opts)
	if err != nil {
		t.Fatalf("Integration: %v", err)
	}

	if a.applyCalls != 1 {
		t.Errorf("adapter Apply calls = %d, want 1", a.applyCalls)
	}

	// No credential is minted. This used to assert the opposite — that a
	// 64-hex token had been written to <state>/secrets/gateway-token — and
	// the inversion is the point: nothing reads such a file any more, so
	// creating one would leave a secret on disk that only ever costs
	// something (waired-ai/waired#1277).
	paths, _ := integration.PathsFor(opts.StateDir)
	tokenPath := filepath.Join(paths.SecretsDir, "gateway-token")
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want a not-exist error — linking must not mint a gateway token", tokenPath, err)
	}

	if res == nil {
		t.Fatal("Integration returned a nil result")
	}
}

// TestIntegration_NoEnvFilesWritten asserts the v2 cleanup contract:
// no env.sh / env.fish should be created under <state>/integrations.
// Catching this regression guards the silent-breakage path the wrapper
// subcommand was built to retire.
func TestIntegration_NoEnvFilesWritten(t *testing.T) {
	a := &fakeAdapter{id: integration.AgentClaudeCode, detect: integration.Detection{Found: true}}
	opts := newOpts(t, a)

	if _, err := Integration(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"env.sh", "env.fish"} {
		p := filepath.Join(opts.StateDir, "integrations", name)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("env file unexpectedly present: %s", p)
		}
	}
}

// TestIntegration_NoRcFilesEdited asserts the orchestrator never
// touches `~/.bashrc` (or peers) — even if such a file already exists,
// post-v2 setup leaves it byte-for-byte unchanged.
func TestIntegration_NoRcFilesEdited(t *testing.T) {
	a := &fakeAdapter{id: integration.AgentClaudeCode, detect: integration.Detection{Found: true}}
	opts := newOpts(t, a)

	bashrc := filepath.Join(opts.HomeDir, ".bashrc")
	pre := []byte("# user content\nexport FOO=bar\n")
	if err := os.WriteFile(bashrc, pre, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Integration(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(bashrc)
	if string(got) != string(pre) {
		t.Errorf(".bashrc was modified:\n--- got ---\n%s", got)
	}
}

func TestIntegration_AdapterErrorsCollectedNotShortCircuited(t *testing.T) {
	cc := &fakeAdapter{id: integration.AgentClaudeCode, detect: integration.Detection{Found: true}, applyErr: errors.New("boom")}
	oc := &fakeAdapter{id: integration.AgentOpenClaw, detect: integration.Detection{Found: true}}
	opts := newOpts(t, cc, oc)

	res, err := Integration(context.Background(), opts)
	if err != nil {
		t.Fatalf("orchestrator returned err: %v", err)
	}
	if cc.applyCalls != 1 || oc.applyCalls != 1 {
		t.Errorf("apply calls cc=%d oc=%d (both should be 1)", cc.applyCalls, oc.applyCalls)
	}
	var sawErr bool
	for _, r := range res.Agents {
		if r.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("expected at least one ApplyResult.Err to be set")
	}
}

func TestUninstallAll_DispatchesAdapters(t *testing.T) {
	a := &fakeAdapter{id: integration.AgentClaudeCode, detect: integration.Detection{Found: true}}
	opts := newOpts(t, a)
	if _, err := Integration(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	if err := UninstallAll(context.Background(), opts); err != nil {
		t.Fatalf("UninstallAll: %v", err)
	}
	// UninstallAll uses the production adapter set, not opts.Adapters,
	// so we cannot assert against the fake here. The smoke test is
	// just that the call returns nil; per-adapter coverage lives in
	// each adapter's own *_test.go.
}

func TestIntegration_RejectsBlankInputs(t *testing.T) {
	if _, err := Integration(context.Background(), IntegrationOptions{}); err == nil {
		t.Error("expected error for blank options")
	}
	if _, err := Integration(context.Background(), IntegrationOptions{HomeDir: "/h"}); err == nil {
		t.Error("expected error when StateDir blank")
	}
	if _, err := Integration(context.Background(), IntegrationOptions{HomeDir: "/h", StateDir: "/s"}); err == nil {
		t.Error("expected error when GatewayBaseURL blank")
	}
}
