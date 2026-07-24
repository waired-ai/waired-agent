package main

import (
	"context"
	"fmt"
	"io"
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
problem, then run ` + "`waired logs --mask-pii`" + `. --mask-pii redacts your home
directory, username, hostname and account email so the file is safe to attach
to an issue (best-effort — review it before sharing either way).`

func newLogsCmd() *cobra.Command {
	var output, stateDir string
	var since time.Duration
	var maskPII bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Collect the agent's logs into a file for debugging.",
		Long:  logsLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogsCollect(cmd.Context(), output, stateDir, since, maskPII)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "",
		"file to write (default: waired-logs-<timestamp>.txt in the current directory)")
	cmd.Flags().DurationVar(&since, "since", time.Hour,
		"how far back to collect the service log")
	cmd.Flags().BoolVar(&maskPII, "mask-pii", os.Getenv("WAIRED_PII_MASK") != "",
		"mask personal info (home directory, username, hostname, account email) in the collected file — best-effort, for bug reports. Env form: WAIRED_PII_MASK=1.")
	addStateDirFlag(cmd, &stateDir, "agent state dir, for the bundled engine logs")
	return cmd
}

func runLogsCollect(ctx context.Context, output, stateDir string, since time.Duration, maskPII bool) error {
	if output == "" {
		output = fmt.Sprintf("waired-logs-%s.txt", time.Now().Format("20060102-150405"))
	}
	f, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("waired logs: create %s: %w", output, err)
	}
	// When masking, wrap the file so every block logdump writes is redacted.
	// logdump writes complete blocks per Write (whole command output, whole
	// engine-log file, one formatted header line each), so a masked token
	// never straddles two Write calls.
	var w io.Writer = f
	if maskPII {
		w = maskingWriter{w: f, m: newPIIMasker()}
	}
	if err := logdump.Collect(ctx, w, logdump.Options{Since: since, StateDir: stateDir}); err != nil {
		f.Close()
		return fmt.Errorf("waired logs: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("waired logs: close %s: %w", output, err)
	}
	fmt.Printf("Wrote logs to %s\n", output)
	if maskPII {
		fmt.Println("Masked home dir / username / hostname / email (best-effort). Review it before sharing.")
	} else {
		fmt.Println("Review it before sharing — it may contain local file paths or your username. Add --mask-pii to redact them.")
	}
	return nil
}

// maskingWriter applies the PII masker to every block written through it,
// reusing the same replacement set as `waired init --mask-pii` (see
// pii_mask.go). It reports the original byte count as consumed so callers
// (fmt.Fprintf inside logdump) see a complete write despite the substitution
// changing the length.
type maskingWriter struct {
	w io.Writer
	m *piiMasker
}

func (mw maskingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(mw.w, mw.m.mask(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
