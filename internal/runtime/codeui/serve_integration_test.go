//go:build integration

package codeui_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/opencode"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/codeui"
)

// TestServe_RealLifecycle is the L2 multi-OS smoke (#501). It drives the
// production codeui path end to end with NO fakes:
//
//	Installer.Install      -> real download of the pinned opencode binary,
//	                          sha256 verify, zip/tar.gz extract, .exe naming
//	Service.EnsureRunning  -> real infruntime.DefaultSpawner spawns
//	                          `opencode serve` (process group on unix /
//	                          Job Object on Windows), readiness probe
//	GET /                  -> embedded web UI returns 2xx/3xx
//	POST /session          -> session.create succeeds (no model needed)
//	Service.Stop           -> SIGTERM->StopTimeout->Kill reaps the process
//	                          (the whole tree on Windows)
//
// Inference (POST /session/{id}/message) is explicitly OUT OF SCOPE — the
// provider/gateway is never contacted, so this is deterministic with no
// model/GPU. The plugin baseURL points at a loopback gateway that won't exist
// in CI; that's fine — the provider is not contacted at boot or session-create.
//
// Run with: go test -tags integration ./internal/runtime/codeui/...
// (Make target: `make integration-codeui`.) CI: .github/workflows/codeui-multios.yml.
//
// Skips cleanly when the binary can't be obtained (no network AND no
// WAIRED_CODEUI_BINARY) — the same "binary is the missing prerequisite" skip
// stance as ollama_integration_test.go.
func TestServe_RealLifecycle(t *testing.T) {
	base := t.TempDir()
	inst := codeui.NewInstaller(base)
	// Offline / fork fallback: an already-present binary skips the download.
	if local := os.Getenv("WAIRED_CODEUI_BINARY"); local != "" {
		inst.LocalBinary = local
	}

	// Install: real download + sha256 verify + extract, unless LocalBinary
	// short-circuits it. A network failure with no override is a clean skip
	// (the binary, not env config, is the missing prerequisite).
	installCtx, cancelInstall := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelInstall()
	if err := inst.Install(installCtx, func(p codeui.InstallProgress) {
		t.Logf("install %s: %s", p.Stage, p.Message)
	}); err != nil {
		if inst.LocalBinary == "" {
			t.Skipf("cannot obtain opencode binary (no WAIRED_CODEUI_BINARY, install failed): %v", err)
		}
		t.Fatalf("Install with WAIRED_CODEUI_BINARY=%q: %v", inst.LocalBinary, err)
	}
	if !inst.Active() {
		t.Fatalf("opencode binary not active after Install at %s", inst.BinaryPath())
	}

	// Seed the isolated config exactly as production does: default-model
	// opencode.json + the waired provider plugin. The gateway URL is a dummy
	// loopback — the provider is not contacted at boot or session-create.
	if _, err := codeui.WriteDefaultConfig(inst.OpenCodeConfigDir()); err != nil {
		t.Fatalf("WriteDefaultConfig: %v", err)
	}
	if _, err := opencode.WritePluginInConfigDir(inst.OpenCodeConfigDir(), "http://127.0.0.1:9473"); err != nil {
		t.Fatalf("WritePluginInConfigDir: %v", err)
	}

	// Scratch project the coding agent operates on. The installer no longer
	// owns a workspace dir (XDG split: ConfigDir is XDG_CONFIG_HOME); production
	// passes the user's --project as Workspace, so the test does the same.
	workspace := t.TempDir()
	port := freePort(t)
	svc := codeui.New(codeui.Config{
		Host:          "127.0.0.1",
		Port:          port,
		BinaryPath:    inst.BinaryPath(),
		XDGConfigHome: inst.ConfigDir(),
		DataDir:       inst.DataDir(),
		Workspace:     workspace,
		LogDir:        inst.LogDir(),
		// REAL spawner with the scratch workspace as cwd — this is the whole
		// point of L2 (Setpgid on unix, Job Object on Windows).
		Spawner: infruntime.DefaultSpawner{Dir: workspace},
		// Generous first-ready budget: a cold binary on a slow hosted runner
		// (esp. Windows, first exec + AV scan) can take a while to listen.
		StartupReadyTimeout: 90 * time.Second,
		// On Windows Signal(SIGTERM) is a no-op, so Stop always rides this
		// grace to Kill; keep the production default (5s) so we exercise that
		// real timeout path rather than masking it.
		StopTimeout: 5 * time.Second,
	})

	// EnsureRunning spawns the child under a Service-owned context; the ctx we
	// pass only bounds the readiness WAIT, never the process lifetime.
	startCtx, cancelStart := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancelStart()
	if err := svc.EnsureRunning(startCtx); err != nil {
		dumpLog(t, inst.LogDir())
		t.Fatalf("EnsureRunning real opencode serve: %v", err)
	}
	// Always stop, even on a later failure, so the process (tree) is reaped.
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := svc.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if got := svc.Health().State; got != infruntime.StateStopped {
			t.Errorf("state after Stop = %q, want %q", got, infruntime.StateStopped)
		}
	})

	if got := svc.Health().State; got != infruntime.StateReady {
		dumpLog(t, inst.LogDir())
		t.Fatalf("state after EnsureRunning = %q, want %q", got, infruntime.StateReady)
	}

	// serve answers GET / instantly once ready; 60s is plenty for that.
	client := &http.Client{Timeout: 60 * time.Second}

	// (1) Embedded web UI answers on the loopback port.
	t.Run("web_ui_200", func(t *testing.T) {
		resp, err := client.Get(svc.URL() + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			t.Fatalf("GET / status = %d, want 2xx/3xx", resp.StatusCode)
		}
	})

	// (2) session.create succeeds with no model backend. The assertion is
	// lenient (any 2xx + a non-empty id-like field) so a minor upstream API
	// shape change doesn't make it brittle; the point is that serve is alive
	// and its HTTP API responds on this OS.
	t.Run("session_create", func(t *testing.T) {
		// The first session-create rides opencode's cold init
		// (sqlite/workspace/runtime warmup): a couple of seconds on
		// Linux/macOS, but on a cold GitHub-hosted Windows runner (Defender
		// scanning every first-touched file + slow I/O) it exceeded the
		// shared 60s client budget (waired#848), so it gets its own longer
		// one. Linux and macOS pass session-create against the same
		// unreachable :9473 gateway, so this is opencode's own startup
		// latency, not the dead provider — just be patient.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.URL()+"/session", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("POST /session: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			dumpLog(t, inst.LogDir())
			t.Fatalf("POST /session status = %d, want 2xx", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode /session response: %v", err)
		}
		if id, _ := body["id"].(string); id == "" {
			t.Fatalf("POST /session returned no session id: %v", body)
		}
	})
}

// freePort asks the kernel for an unused loopback TCP port. Same idiom as
// internal/runtime/ollama_integration_test.go (which is package runtime_test
// and so not importable here).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// dumpLog tails the serve log into the test output so a failed boot in CI is
// diagnosable.
func dumpLog(t *testing.T, logDir string) {
	t.Helper()
	if logDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(logDir, "codeui.log"))
	if err != nil {
		return
	}
	t.Logf("--- codeui.log ---\n%s\n--- end codeui.log ---", b)
}
