package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// logController implements management.LogController. It flips the process's
// live slog level (a *slog.LevelVar shared with the JSON handler) and
// persists the choice to agent.json so a restart keeps it.
type logController struct {
	levelVar *slog.LevelVar
	jsonPath string

	mu sync.Mutex // serializes the read-modify-write of agent.json
}

func newLogController(levelVar *slog.LevelVar, jsonPath string) *logController {
	return &logController{levelVar: levelVar, jsonPath: jsonPath}
}

func (c *logController) LogLevel(context.Context) (string, error) {
	return agentconfig.LogLevelName(c.levelVar.Level()), nil
}

func (c *logController) SetLogLevel(_ context.Context, level string) (string, error) {
	norm, err := agentconfig.NormalizeLogLevel(level)
	if err != nil {
		return "", fmt.Errorf("%w: %v", management.ErrInvalidLogLevel, err)
	}
	parsed, _ := agentconfig.ParseLogLevel(norm)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Apply live first so verbosity changes even if persistence fails.
	c.levelVar.Set(parsed)
	slog.Info("log level changed via management API", "level", norm)

	if err := c.persist(norm); err != nil {
		// The live change already took effect; surface the persistence
		// failure so the operator knows it will not survive a restart.
		return norm, fmt.Errorf("log level set to %s (live) but persisting to agent.json failed: %w", norm, err)
	}
	return norm, nil
}

// persist writes logging.level back to agent.json, preserving every other
// field (read-modify-write via Defaults()+MergeJSON, then Save). Mirrors
// the maybeSelectBundledModelForFreshInstall persistence path.
func (c *logController) persist(level string) error {
	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(c.jsonPath); err != nil {
		return err
	}
	cfg.Logging.Level = level
	return cfg.Save(c.jsonPath)
}
