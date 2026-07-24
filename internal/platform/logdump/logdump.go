// Package logdump collects the running agent's logs into a single stream
// for bug reports and pre-release debugging. It reads the OS service log
// (systemd journal on Linux, the unified log on macOS, the Application
// Event Log on Windows) plus the bundled inference-engine logs, and writes
// them to an io.Writer with section headers.
//
// The OS service log lives with the platform's service manager, not in a
// file the agent owns, so collection shells out to the native tool
// (journalctl / log / Get-WinEvent). The tool + arguments are chosen by a
// pure runtime.GOOS switch (serviceLogCommand) so the decision is unit
// testable for every OS on any host, per the repo's cross-OS parity rule.
package logdump

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
)

// Options controls a collection run.
type Options struct {
	// Since is how far back to pull the OS service log. Zero means a
	// sensible default (one hour).
	Since time.Duration
	// StateDir is the agent state directory; bundled engine logs live
	// under <StateDir>/runtimes/<engine>/logs. Empty skips engine logs.
	StateDir string
}

// Collect writes a consolidated log bundle to w: a header, the OS service
// log for the running platform, and the bundled inference-engine logs.
// A failure of any single source is noted inline rather than aborting the
// whole bundle, so a partial dump is still useful.
func Collect(ctx context.Context, w io.Writer, opts Options) error {
	if opts.Since <= 0 {
		opts.Since = time.Hour
	}
	writeHeader(w, opts)

	fprintf(w, "\n===== service log (%s, last %s) =====\n", runtime.GOOS, opts.Since)
	name, args := serviceLogCommand(runtime.GOOS, opts.Since, time.Now())
	if name == "" {
		fprintf(w, "(no service-log source is known for %s)\n", runtime.GOOS)
	} else if err := runServiceLog(ctx, w, name, args); err != nil {
		fprintf(w, "(could not read the service log via %q: %v)\n", name, err)
	}

	fprintf(w, "\n===== engine logs =====\n")
	collectEngineLogs(w, opts.StateDir)
	return nil
}

// fprintf / fprintln write to the bundle writer, dropping the write error:
// a log bundle destined for a file or stdout has nowhere useful to report a
// mid-write failure, and the caller sees the truncated file regardless.
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func fprintln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }

func writeHeader(w io.Writer, opts Options) {
	fprintln(w, "===== waired log bundle =====")
	fprintf(w, "generated: %s\n", time.Now().Format(time.RFC3339))
	fprintf(w, "os/arch:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fprintf(w, "agent:     %s (%s)\n", buildinfo.Version, buildinfo.BuildSHA)
	fprintf(w, "state-dir: %s\n", opts.StateDir)
}

// serviceLogCommand returns the executable name and arguments that dump the
// agent's service log on goos, bounded to the last `since`. now is injected
// so the Linux --since timestamp is deterministic in tests. An unknown goos
// yields an empty name.
func serviceLogCommand(goos string, since time.Duration, now time.Time) (name string, args []string) {
	switch goos {
	case "linux":
		start := now.Add(-since).Format("2006-01-02 15:04:05")
		return "journalctl", []string{"-u", "waired-agent", "--no-pager", "--since", start}
	case "darwin":
		mins := max(int(since.Minutes()), 1)
		return "log", []string{
			"show", "--predicate", `process == "waired-agent"`,
			"--style", "syslog", "--last", fmt.Sprintf("%dm", mins),
		}
	case "windows":
		secs := max(int(since.Seconds()), 1)
		ps := fmt.Sprintf(
			`Get-WinEvent -FilterHashtable @{ProviderName='waired-agent'; StartTime=(Get-Date).AddSeconds(-%d)} `+
				`-ErrorAction SilentlyContinue | Format-List TimeCreated,LevelDisplayName,Message`, secs)
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-Command", ps}
	default:
		return "", nil
	}
}

func runServiceLog(ctx context.Context, w io.Writer, name string, args []string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if len(out) > 0 {
		_, _ = w.Write(out)
		if out[len(out)-1] != '\n' {
			_, _ = io.WriteString(w, "\n")
		}
		return nil // output present: journalctl/log/Get-WinEvent may still exit non-zero
	}
	if err != nil {
		return err
	}
	_, _ = io.WriteString(w, "(no log entries in the window)\n")
	return nil
}

// collectEngineLogs appends every <stateDir>/runtimes/<engine>/logs/*.log
// file. Missing directories are skipped silently (an engine may not be
// installed).
func collectEngineLogs(w io.Writer, stateDir string) {
	if stateDir == "" {
		fprintln(w, "(no --state-dir given; skipping engine logs)")
		return
	}
	found := false
	for _, engine := range []string{"ollama", "vllm"} {
		logDir := filepath.Join(stateDir, "runtimes", engine, "logs")
		entries, err := os.ReadDir(logDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			found = true
			p := filepath.Join(logDir, e.Name())
			fprintf(w, "\n----- %s -----\n", p)
			data, err := os.ReadFile(p)
			if err != nil {
				fprintf(w, "(could not read: %v)\n", err)
				continue
			}
			_, _ = w.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				_, _ = io.WriteString(w, "\n")
			}
		}
	}
	if !found {
		fprintln(w, "(no engine logs found)")
	}
}
