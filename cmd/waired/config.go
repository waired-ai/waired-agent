package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

const configLong = `Show or change the background service's settings.

  waired config log-level [debug|info|warn|error]
      With no level, print the current log level. With a level, apply it
      live (no restart) and save it to agent.json so it survives one.
      Raising it to debug is the pre-release debugging switch; the Waired app
      follows the background service, so the two change together.`

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change the background service's settings",
		Long:  configLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newConfigLogLevelCmd())
	return cmd
}

func newConfigLogLevelCmd() *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   "log-level [debug|info|warn|error]",
		Short: "Show or set the log level (applies live and is saved)",
		Long: `Show or set the agent log verbosity.

With no argument it prints the running daemon's current level. With a level
argument it applies the change live (no restart) and persists it to
agent.json. The Waired app follows the daemon, so a single change covers
both the service and the app. When the daemon is not running, the value is
read from / written to agent.json directly and applies on the next start.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runConfigLogLevelGet(mgmt, stateDir)
			}
			return runConfigLogLevelSet(mgmt, stateDir, args[0])
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "agent.json location, used when the daemon is unreachable")
	return cmd
}

// runConfigLogLevelGet prints the daemon's current level, falling back to
// the persisted agent.json value when the daemon is not running.
func runConfigLogLevelGet(mgmt, stateDir string) error {
	body, err := httpGet(mgmt + "/waired/v1/log/level")
	if err == nil {
		var resp management.LogLevelResponse
		if jErr := json.Unmarshal(body, &resp); jErr != nil {
			return fmt.Errorf("waired config log-level: parse: %w", jErr)
		}
		fmt.Fprintf(stdout, "Log level: %s\n", resp.Level)
		return nil
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("waired config log-level: %w", err)
	}
	lvl, rErr := persistedLogLevel(stateDir)
	if rErr != nil {
		return fmt.Errorf("waired config log-level: daemon unreachable AND could not read config: %w", rErr)
	}
	fmt.Fprintf(stdout, "Log level: %s (saved; the background service isn't running)\n", lvl)
	return nil
}

// runConfigLogLevelSet applies a level live via the daemon and persists it;
// if the daemon is unreachable it writes agent.json so the next start picks
// it up (the same dual-path shape as `waired share`).
func runConfigLogLevelSet(mgmt, stateDir, level string) error {
	norm, err := agentconfig.NormalizeLogLevel(level)
	if err != nil {
		return fmt.Errorf("waired config log-level: %w", err)
	}
	payload, _ := json.Marshal(management.LogSettingsRequest{Level: norm})
	body, err := httpPost(mgmt+"/waired/v1/log/settings", payload)
	if err == nil {
		var resp management.LogLevelResponse
		if jErr := json.Unmarshal(body, &resp); jErr == nil && resp.Level != "" {
			norm = resp.Level
		}
		fmt.Fprintf(stdout, "Log level set to %s (applied live).\n", norm)
		return nil
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("waired config log-level: daemon returned: %w", err)
	}
	path := agentconfig.JSONPathFor(stateDir)
	cfg := agentconfig.Defaults()
	if mErr := cfg.MergeJSON(path); mErr != nil {
		return fmt.Errorf("waired config log-level: daemon unreachable AND could not read %s: %w", path, mErr)
	}
	cfg.Logging.Level = norm
	if sErr := cfg.Save(path); sErr != nil {
		return fmt.Errorf("waired config log-level: daemon unreachable AND could not persist to %s: %w", path, sErr)
	}
	fmt.Fprintf(stdout, "The background service isn't running. Log level %s is saved to %s and applies on the next start.\n", norm, path)
	return nil
}

// persistedLogLevel reads logging.level from agent.json (defaulting to
// info), for use when the daemon is not answering.
func persistedLogLevel(stateDir string) (string, error) {
	cfg := agentconfig.Defaults()
	if err := cfg.MergeJSON(agentconfig.JSONPathFor(stateDir)); err != nil {
		return "", err
	}
	if cfg.Logging.Level == "" {
		return agentconfig.LogLevelInfo, nil
	}
	return cfg.Logging.Level, nil
}
