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
