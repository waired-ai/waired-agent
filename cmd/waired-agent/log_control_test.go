package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveLogLevel_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		cfgLevel string
		flagVal  string
		env      map[string]string
		want     slog.Level
	}{
		{"default info", "", "", nil, slog.LevelInfo},
		{"config debug", "debug", "", nil, slog.LevelDebug},
		{"flag beats config", "warn", "debug", nil, slog.LevelDebug},
		{"flag beats env", "", "error", map[string]string{"WAIRED_LOG_LEVEL": "warn"}, slog.LevelError},
		{"env beats config", "warn", "", map[string]string{"WAIRED_LOG_LEVEL": "error"}, slog.LevelError},
		{"env beats WAIRED_DEBUG", "", "", map[string]string{"WAIRED_LOG_LEVEL": "warn", "WAIRED_DEBUG": "1"}, slog.LevelWarn},
		{"WAIRED_DEBUG legacy", "", "", map[string]string{"WAIRED_DEBUG": "1"}, slog.LevelDebug},
		{"WAIRED_DEBUG beats config", "info", "", map[string]string{"WAIRED_DEBUG": "yes"}, slog.LevelDebug},
		{"invalid flag falls through to config", "warn", "bogus", nil, slog.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLogLevel(tc.cfgLevel, tc.flagVal, envFrom(tc.env))
			if got != tc.want {
				t.Errorf("resolveLogLevel(%q, %q, %v) = %v, want %v",
					tc.cfgLevel, tc.flagVal, tc.env, got, tc.want)
			}
		})
	}
}

func TestLevelName_RoundTrip(t *testing.T) {
	for _, name := range []string{
		agentconfig.LogLevelDebug, agentconfig.LogLevelInfo,
		agentconfig.LogLevelWarn, agentconfig.LogLevelError,
	} {
		lvl, err := agentconfig.ParseLogLevel(name)
		if err != nil {
			t.Fatalf("ParseLogLevel(%q): %v", name, err)
		}
		if got := levelName(lvl); got != name {
			t.Errorf("levelName(%v) = %q, want %q", lvl, got, name)
		}
	}
}

func TestLogController_SetLogLevel_LiveAndPersisted(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "agent.json")

	// Seed agent.json with a non-default field so we can assert the
	// read-modify-write preserves it.
	seed := agentconfig.Defaults()
	seed.Inference.MaxCacheGB = 42
	seed.Logging.Level = agentconfig.LogLevelInfo
	if err := seed.Save(jsonPath); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	c := newLogController(lv, jsonPath)

	got, err := c.SetLogLevel(context.Background(), "DEBUG")
	if err != nil {
		t.Fatalf("SetLogLevel: %v", err)
	}
	if got != agentconfig.LogLevelDebug {
		t.Fatalf("SetLogLevel returned %q, want debug", got)
	}
	// Live: the LevelVar changed immediately.
	if lv.Level() != slog.LevelDebug {
		t.Fatalf("live level = %v, want Debug", lv.Level())
	}
	// Persisted: agent.json now carries logging.level=debug and kept the
	// unrelated field.
	reloaded := agentconfig.Defaults()
	if err := reloaded.MergeJSON(jsonPath); err != nil {
		t.Fatalf("reload MergeJSON: %v", err)
	}
	if reloaded.Logging.Level != agentconfig.LogLevelDebug {
		t.Errorf("persisted level = %q, want debug", reloaded.Logging.Level)
	}
	if reloaded.Inference.MaxCacheGB != 42 {
		t.Errorf("read-modify-write clobbered unrelated field: MaxCacheGB=%d, want 42", reloaded.Inference.MaxCacheGB)
	}

	// Reading back through the controller reflects the live value.
	cur, err := c.LogLevel(context.Background())
	if err != nil {
		t.Fatalf("LogLevel: %v", err)
	}
	if cur != agentconfig.LogLevelDebug {
		t.Errorf("LogLevel() = %q, want debug", cur)
	}
}

func TestLogController_SetLogLevel_InvalidRejected(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "agent.json")
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	c := newLogController(lv, jsonPath)

	_, err := c.SetLogLevel(context.Background(), "loud")
	if err == nil {
		t.Fatal("SetLogLevel(loud) = nil error, want ErrInvalidLogLevel")
	}
	if !errors.Is(err, management.ErrInvalidLogLevel) {
		t.Fatalf("error = %v, want ErrInvalidLogLevel", err)
	}
	// The live level must be unchanged on a rejected input.
	if lv.Level() != slog.LevelInfo {
		t.Errorf("live level changed on invalid input: %v", lv.Level())
	}
}
