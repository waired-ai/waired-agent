// Package logdump collects the running agent's logs into a single stream
// for bug reports and pre-release debugging. It reads the OS service log
// (systemd journal on Linux, the unified log on macOS, the Application
// Event Log on Windows), the plain service log files where the platform
// keeps any (launchd's stdout/stderr capture on macOS, plus its rotated
// archives), and the bundled inference-engine logs, and writes them to an
// io.Writer with section headers.
//
// The OS service log lives with the platform's service manager, not in a
// file the agent owns, so collection shells out to the native tool
// (journalctl / log / Get-WinEvent). The tool + arguments are chosen by a
// pure runtime.GOOS switch (serviceLogCommand) so the decision is unit
// testable for every OS on any host, per the repo's cross-OS parity rule.
package logdump

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
	"github.com/waired-ai/waired-agent/internal/platform/pwsh"
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

	fprintf(w, "\n===== service log files =====\n")
	collectServiceLogFiles(w, runtime.GOOS, userHome())

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
	cmd := exec.CommandContext(ctx, name, args...)
	// The Windows branch spawns Windows PowerShell 5.1, which must not
	// inherit a PowerShell 7 PSModulePath (#178) — see
	// internal/platform/pwsh. Harmless on the journalctl / log branches.
	cmd.Env = pwsh.Env()
	out, err := cmd.CombinedOutput()
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

// tailLimit caps how much of each service log file lands in the bundle.
// The rotation policy keeps these at 1 MB apiece, so this is a tail only
// on a legacy host whose rotation never took (#331) — exactly the host
// whose file can be arbitrarily large. Unlike the engine logs below,
// these are read incrementally rather than with os.ReadFile, so an
// unrotated multi-gigabyte file cannot be pulled into memory.
const tailLimit = 512 << 10

// serviceLogFiles returns the plain files the OS service manager writes
// this process's stdout/stderr to on goos, in the (GOOS, facts) -> plan
// shape the cross-OS parity rule asks for.
//
// Only darwin has any. On Linux the unit's streams go to the journal and
// on Windows to the Event Log, both already covered by
// serviceLogCommand above; only launchd points them at files, which is
// why only macOS could lose them to a rotation (#331). An empty home
// yields no tray entries rather than a guessed path.
func serviceLogFiles(goos, home string) []string {
	if goos != "darwin" {
		return nil
	}
	files := []string{logrotate.AgentErrPath, logrotate.AgentOutPath}
	if home != "" {
		files = append(files, logrotate.TrayErrPath(home), logrotate.TrayOutPath(home))
	}
	return files
}

func userHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// collectServiceLogFiles appends each service log file and its rotated
// archives, oldest first.
//
// Before #331 none of this was collected at all: on macOS the bundle
// held the unified log and the engine logs, so the daemon's own stderr —
// the stream carrying every slog record — was invisible to the tool we
// ask users to run for a bug report.
func collectServiceLogFiles(w io.Writer, goos, home string) {
	files := serviceLogFiles(goos, home)
	if len(files) == 0 {
		fprintf(w, "(no service log files on %s; the service log above is the source)\n", goos)
		return
	}
	appendServiceLogFiles(w, files)
}

// appendServiceLogFiles takes the resolved paths rather than deriving
// them, so the collection itself is testable against a temp directory
// while serviceLogFiles is table-tested as a pure plan.
func appendServiceLogFiles(w io.Writer, files []string) {
	for _, base := range files {
		archives := archivesFor(base)
		for _, a := range archives {
			appendLogFile(w, a, true)
		}
		appendLogFile(w, base, false)
		warnLegacyRotationGap(w, base, archives)
	}
}

// archivesFor lists base's rotated archives oldest first. The names are
// <base>.<n>.gz with a higher n being older, so a descending sort by n
// puts them in reading order.
func archivesFor(base string) []string {
	matches, err := filepath.Glob(base + ".*.gz")
	if err != nil || len(matches) == 0 {
		return nil
	}
	index := func(p string) int {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, base+"."), ".gz"))
		if err != nil {
			return -1
		}
		return n
	}
	sort.Slice(matches, func(i, j int) bool { return index(matches[i]) > index(matches[j]) })
	return matches
}

// appendLogFile writes one file's tail under a header carrying its size
// and mtime — a bug report often turns on whether a file stopped being
// written to, which the content alone does not say.
func appendLogFile(w io.Writer, path string, gzipped bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return // absent is the norm: no archives yet, no tray installed
	}
	fprintf(w, "\n----- %s (%d bytes, modified %s) -----\n",
		path, fi.Size(), fi.ModTime().Format(time.RFC3339))

	f, err := os.Open(path)
	if err != nil {
		fprintf(w, "(could not read: %v)\n", err)
		return
	}
	defer f.Close()

	var r io.Reader = f
	if gzipped {
		zr, err := gzip.NewReader(f)
		if err != nil {
			fprintf(w, "(could not decompress: %v)\n", err)
			return
		}
		defer zr.Close()
		r = zr
	}
	data, truncated, err := readTail(r, tailLimit)
	if err != nil {
		fprintf(w, "(read failed after %d bytes: %v)\n", len(data), err)
	}
	if truncated {
		fprintf(w, "(truncated to the last %d bytes)\n", len(data))
	}
	_, _ = w.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		_, _ = io.WriteString(w, "\n")
	}
}

// readTail returns at most limit trailing bytes of r, reporting whether
// anything was dropped. The kept region starts at a line boundary so the
// bundle never opens mid-record.
func readTail(r io.Reader, limit int) (data []byte, truncated bool, err error) {
	buf := make([]byte, 0, limit)
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > limit {
				buf = append(buf[:0], buf[len(buf)-limit:]...)
				truncated = true
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return buf, truncated, rerr
		}
	}
	if truncated {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 && i+1 < len(buf) {
			buf = buf[i+1:]
		}
	}
	return buf, truncated, nil
}

// warnLegacyRotationGap says so, in the bundle, when the live file looks
// like one an external rotator emptied: nothing in it but a newsyslog
// turn-over banner, with archives beside it.
//
// That is the pre-#331 state — newsyslog renamed the file, launchd's
// descriptor stayed on the renamed inode, and every line the daemon
// wrote afterwards went nowhere. Without this note the bundle simply
// shows an almost-empty log, which reads as "the daemon said nothing"
// rather than "the daemon's output was lost from here on".
func warnLegacyRotationGap(w io.Writer, base string, archives []string) {
	if len(archives) == 0 {
		return
	}
	data, err := os.ReadFile(base)
	if err != nil {
		return
	}
	banner := newsyslogBanner(string(data))
	if banner == "" {
		return
	}
	fprintf(w, "\n(!) %s holds nothing but a rotation banner: %q\n", base, banner)
	fprintf(w, "    This host still rotates with the retired newsyslog drop-in, so everything\n")
	fprintf(w, "    written after that rotation was lost rather than logged (#331). Update the\n")
	fprintf(w, "    agent, or restart it, to reattach the stream. The archives above hold what\n")
	fprintf(w, "    survived.\n")
}

// newsyslogBanner returns the turn-over line when content is nothing
// but such banners (plus blank lines), and "" otherwise.
func newsyslogBanner(content string) string {
	found := ""
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "logfile turned over") {
			if found == "" {
				found = trimmed
			}
			continue
		}
		return "" // real log content is present; no gap to report
	}
	return found
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
