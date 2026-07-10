package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func writeStateFile(t *testing.T, dir string, s state.State) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.StatePath(dir), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newClaudeServer(cfg ClaudeIntegrationConfig) *Server {
	return New(fakeStatus{}, fakePinger{}).WithClaudeIntegration(cfg)
}

func doClaudeReq(t *testing.T, srv *Server) (int, ClaudeIntegrationStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/claude", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, ClaudeIntegrationStatus{}
	}
	var got ClaudeIntegrationStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return rec.Code, got
}

func TestClaudeIntegration_Disabled404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/claude", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestClaudeIntegration_NoStateFile(t *testing.T) {
	stateDir := t.TempDir()
	homeDir := t.TempDir()
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: stateDir, HomeDir: homeDir, BinaryPath: "/p/waired",
	})
	code, got := doClaudeReq(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got.Wrapper.Reachable {
		t.Errorf("expected unreachable, got %+v", got.Wrapper)
	}
	if got.Wrapper.Reason != state.ReasonAgentStopped {
		t.Errorf("reason = %q", got.Wrapper.Reason)
	}
	if got.BinaryPath != "/p/waired" {
		t.Errorf("BinaryPath = %q", got.BinaryPath)
	}
}

func TestClaudeIntegration_ActiveAgent(t *testing.T) {
	stateDir := t.TempDir()
	homeDir := t.TempDir()
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	writeStateFile(t, stateDir, state.State{
		Phase:                   state.PhaseActive,
		PID:                     os.Getpid(),
		Updated:                 now.Add(-2 * time.Second),
		GatewayURL:              "http://127.0.0.1:9473",
		InferenceReachableLocal: true,
	})
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: stateDir, HomeDir: homeDir, BinaryPath: "/p/waired",
		Now: func() time.Time { return now },
	})
	_, got := doClaudeReq(t, srv)
	if !got.Wrapper.Reachable {
		t.Fatalf("expected reachable: %+v", got.Wrapper)
	}
	if got.Wrapper.State == nil {
		t.Fatal("expected state view")
	}
	if got.Wrapper.State.GatewayURL != "http://127.0.0.1:9473" {
		t.Errorf("GatewayURL = %q", got.Wrapper.State.GatewayURL)
	}
}

func TestClaudeIntegration_StaleHeartbeat(t *testing.T) {
	stateDir := t.TempDir()
	homeDir := t.TempDir()
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	writeStateFile(t, stateDir, state.State{
		Phase:                   state.PhaseActive,
		PID:                     os.Getpid(),
		Updated:                 now.Add(-time.Hour),
		GatewayURL:              "http://127.0.0.1:9473",
		InferenceReachableLocal: true,
	})
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: stateDir, HomeDir: homeDir, BinaryPath: "/p/waired",
		Now: func() time.Time { return now },
	})
	_, got := doClaudeReq(t, srv)
	if got.Wrapper.Reachable {
		t.Errorf("expected unreachable: %+v", got.Wrapper)
	}
	if got.Wrapper.Reason != state.ReasonAgentStopped {
		t.Errorf("reason = %q", got.Wrapper.Reason)
	}
}

func TestClaudeIntegration_InferenceUnavailable(t *testing.T) {
	stateDir := t.TempDir()
	homeDir := t.TempDir()
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	writeStateFile(t, stateDir, state.State{
		Phase:                   state.PhaseActive,
		PID:                     os.Getpid(),
		Updated:                 now,
		GatewayURL:              "http://127.0.0.1:9473",
		InferenceReachableLocal: false,
	})
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: stateDir, HomeDir: homeDir, BinaryPath: "/p/waired",
		Now: func() time.Time { return now },
	})
	_, got := doClaudeReq(t, srv)
	if got.Wrapper.Reachable {
		t.Errorf("expected unreachable: %+v", got.Wrapper)
	}
	if got.Wrapper.Reason != state.ReasonInferenceUnavailable {
		t.Errorf("reason = %q", got.Wrapper.Reason)
	}
}

// TestClaudeIntegration_ManagedSettingsView: the endpoint reports the managed-
// settings status. ManagedSettingsPath points at a temp location (#604 — the
// real per-OS file may exist on a dogfooding host), so the file is absent and
// Present / Configured are false, but Supported and the expected loopback base
// URL (from the configured ClaudeGatewayPort, default 9472) are populated.
func TestClaudeIntegration_ManagedSettingsView(t *testing.T) {
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: t.TempDir(), HomeDir: t.TempDir(), BinaryPath: "/p/waired",
		ManagedSettingsPath: filepath.Join(t.TempDir(), "managed-settings.json"),
	})
	_, got := doClaudeReq(t, srv)
	ms := got.ManagedSettings
	wantSupported := runtime.GOOS == "linux" || runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if ms.Supported != wantSupported {
		t.Errorf("Supported = %v on %s, want %v", ms.Supported, runtime.GOOS, wantSupported)
	}
	if !strings.HasPrefix(ms.ExpectedBaseURL, "http://127.0.0.1:9472") {
		t.Errorf("ExpectedBaseURL = %q, want the loopback gateway on 9472", ms.ExpectedBaseURL)
	}
	if ms.Present {
		t.Errorf("Present = true with no file at the injected path, want false")
	}
	if ms.Configured {
		t.Errorf("Configured = true with no file at the injected path, want false")
	}
}

// TestClaudeIntegration_ManagedSettingsConfigured: with a managed-settings file
// at the injected path carrying the expected loopback base URL, the view
// reports Configured=true. Could not be tested before #604 made the path
// injectable.
func TestClaudeIntegration_ManagedSettingsConfigured(t *testing.T) {
	msPath := filepath.Join(t.TempDir(), "managed-settings.json")
	body := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472"}}`
	if err := os.WriteFile(msPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: t.TempDir(), HomeDir: t.TempDir(), BinaryPath: "/p/waired",
		ManagedSettingsPath: msPath,
	})
	_, got := doClaudeReq(t, srv)
	ms := got.ManagedSettings
	if ms.Path != msPath {
		t.Errorf("Path = %q, want %q", ms.Path, msPath)
	}
	if !ms.Present {
		t.Error("Present = false, want true")
	}
	if ms.BaseURL != "http://127.0.0.1:9472" {
		t.Errorf("BaseURL = %q", ms.BaseURL)
	}
	if !ms.Configured {
		t.Errorf("Configured = false, want true (BaseURL=%q ExpectedBaseURL=%q)", ms.BaseURL, ms.ExpectedBaseURL)
	}
}

func TestClaudeIntegration_RejectsNonLoopback(t *testing.T) {
	srv := newClaudeServer(ClaudeIntegrationConfig{
		StateDir: t.TempDir(), HomeDir: t.TempDir(), BinaryPath: "/p/waired",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/claude", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}
