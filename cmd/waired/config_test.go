package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

func TestConfigLogLevel_GetFromDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/log/level" || r.Method != http.MethodGet {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"level":"debug"}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runConfig([]string{"log-level", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("runConfig get: %v", err)
		}
	})
	if !strings.Contains(out, "Log level: debug") {
		t.Errorf("output = %q, want it to contain 'Log level: debug'", out)
	}
}

func TestConfigLogLevel_SetHitsDaemon(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"level":"debug"}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runConfig([]string{"log-level", "debug", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("runConfig set: %v", err)
		}
	})
	if gotPath != "/waired/v1/log/settings" {
		t.Errorf("daemon path = %q, want /waired/v1/log/settings", gotPath)
	}
	if !strings.Contains(gotBody, `"level":"debug"`) {
		t.Errorf("request body = %q, want it to carry level=debug", gotBody)
	}
	if !strings.Contains(out, "applied live") {
		t.Errorf("output = %q, want 'applied live'", out)
	}
}

func TestConfigLogLevel_InvalidRejectedLocally(t *testing.T) {
	// No daemon needed: NormalizeLogLevel rejects before any network call.
	err := runConfig([]string{"log-level", "loud", "--mgmt", "http://127.0.0.1:0"})
	if err == nil {
		t.Fatal("expected error for invalid level")
	}
	if !strings.Contains(err.Error(), "log level") {
		t.Errorf("error = %v, want it to mention the level", err)
	}
}

func TestConfigLogLevel_SetFallsBackToFileWhenDaemonDown(t *testing.T) {
	dir := t.TempDir()
	// Seed agent.json so the read-modify-write preserves other fields.
	seed := agentconfig.Defaults()
	seed.Inference.MaxCacheGB = 7
	if err := seed.Save(filepath.Join(dir, "agent.json")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A closed server address → connection refused, exercising the fallback.
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close()

	out := captureStdout(t, func() {
		if err := runConfig([]string{"log-level", "warn", "--mgmt", downURL, "--state-dir", dir}); err != nil {
			t.Fatalf("runConfig set fallback: %v", err)
		}
	})
	if !strings.Contains(out, "applies on next start") {
		t.Errorf("output = %q, want the 'applies on next start' note", out)
	}

	reloaded := agentconfig.Defaults()
	if err := reloaded.MergeJSON(filepath.Join(dir, "agent.json")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Logging.Level != agentconfig.LogLevelWarn {
		t.Errorf("persisted level = %q, want warn", reloaded.Logging.Level)
	}
	if reloaded.Inference.MaxCacheGB != 7 {
		t.Errorf("fallback clobbered unrelated field: MaxCacheGB=%d, want 7", reloaded.Inference.MaxCacheGB)
	}
}

func TestConfigLogLevel_GetFallsBackToFileWhenDaemonDown(t *testing.T) {
	dir := t.TempDir()
	seed := agentconfig.Defaults()
	seed.Logging.Level = agentconfig.LogLevelError
	if err := seed.Save(filepath.Join(dir, "agent.json")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close()

	out := captureStdout(t, func() {
		if err := runConfig([]string{"log-level", "--mgmt", downURL, "--state-dir", dir}); err != nil {
			t.Fatalf("runConfig get fallback: %v", err)
		}
	})
	if !strings.Contains(out, "error") && !strings.Contains(out, "not running") {
		t.Errorf("output = %q, want it to note the persisted/not-running state", out)
	}
	if !strings.Contains(out, "Log level: error") {
		t.Errorf("output = %q, want persisted 'Log level: error'", out)
	}
}
