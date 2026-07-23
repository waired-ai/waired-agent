package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// resolveLogLevel picks the daemon's boot log level. Precedence, highest
// first:
//
//  1. --log-level flag (flagVal), when non-empty
//  2. $WAIRED_LOG_LEVEL
//  3. $WAIRED_DEBUG (legacy switch: any non-empty value → debug)
//  4. cfgLevel — agent.json logging.level (already overlaid by
//     WAIRED_LOG_LEVEL during MergeEnv; re-checked at step 2 so it also
//     outranks the WAIRED_DEBUG back-compat switch)
//  5. info
//
// getenv is injected (os.Getenv in production) so the precedence is unit
// testable.
func resolveLogLevel(cfgLevel, flagVal string, getenv func(string) string) slog.Level {
	if flagVal != "" {
		if lvl, err := agentconfig.ParseLogLevel(flagVal); err == nil {
			return lvl
		}
	}
	if v := getenv("WAIRED_LOG_LEVEL"); v != "" {
		if lvl, err := agentconfig.ParseLogLevel(v); err == nil {
			return lvl
		}
	}
	if getenv("WAIRED_DEBUG") != "" {
		return slog.LevelDebug
	}
	lvl, _ := agentconfig.ParseLogLevel(cfgLevel)
	return lvl
}

// levelName maps a slog.Level back to its config/API name. Range-based so
// an out-of-band value still resolves to the nearest bucket.
func levelName(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return agentconfig.LogLevelDebug
	case l < slog.LevelWarn:
		return agentconfig.LogLevelInfo
	case l < slog.LevelError:
		return agentconfig.LogLevelWarn
	default:
		return agentconfig.LogLevelError
	}
}

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
	return levelName(c.levelVar.Level()), nil
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
