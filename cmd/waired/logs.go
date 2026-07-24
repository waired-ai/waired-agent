package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/platform/logdump"
)

const logsLong = `Collect the agent's logs into a single file for debugging or a bug report.

It gathers the service log for this OS (journalctl on Linux, ` + "`log show`" + ` on
macOS, Get-WinEvent on Windows) plus the bundled inference-engine logs under
the state dir, and writes them to one file.

Tip: raise verbosity first with ` + "`waired config log-level debug`" + `, reproduce the
problem, then run ` + "`waired logs`" + `. Review the file before sharing it — it may
contain local file paths or your username.`

func newLogsCmd() *cobra.Command {
	var output, stateDir string
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Collect the agent's logs into a file for debugging.",
		Long:  logsLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogsCollect(cmd.Context(), output, stateDir, since)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "",
		"file to write (default: waired-logs-<timestamp>.txt in the current directory)")
	cmd.Flags().DurationVar(&since, "since", time.Hour,
		"how far back to collect the service log")
	addStateDirFlag(cmd, &stateDir, "agent state dir, for the bundled engine logs")
	return cmd
}

func runLogsCollect(ctx context.Context, output, stateDir string, since time.Duration) error {
	if output == "" {
		output = fmt.Sprintf("waired-logs-%s.txt", time.Now().Format("20060102-150405"))
	}
	f, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("waired logs: create %s: %w", output, err)
	}
	if err := logdump.Collect(ctx, f, logdump.Options{Since: since, StateDir: stateDir}); err != nil {
		f.Close()
		return fmt.Errorf("waired logs: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("waired logs: close %s: %w", output, err)
	}
	fmt.Printf("Wrote logs to %s\n", output)
	fmt.Println("Review it before sharing — it may contain local file paths or your username.")
	return nil
}
