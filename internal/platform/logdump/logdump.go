// Package logdump collects the running agent's logs into a single stream
// for bug reports and pre-release debugging. It reads the OS service log
// (systemd journal on Linux, the unified log on macOS, the Application
// Event Log on Windows), the plain service log files where the platform
// keeps any (launchd's stdout/stderr capture on macOS and the agent's own
// file on Windows, plus their rotated archives), and the bundled
// inference-engine logs, and writes them to an io.Writer with section
// headers.
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
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// Full collects every rotated generation whole instead of the most
	// recent DefaultBundleBudget bytes of them.
	Full bool
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
	budget := int64(DefaultBundleBudget)
	if opts.Full {
		budget = 0
	}
	collectServiceLogFiles(w, runtime.GOOS, userHome(), opts.StateDir, budget)

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
		// Three things this has to get right, all found by running it on a
		// real Windows host rather than reasoning about it:
		//
		//  1. $ErrorActionPreference, not just -ErrorAction. With no
		//     'waired-agent' provider registered — a half-finished install,
		//     which is exactly when someone runs `waired logs` —
		//     Get-WinEvent raises an EventLogException that the -ErrorAction
		//     PARAMETER does not suppress. The bundle got the exception, its
		//     stack and its localized text where the "no entries" note
		//     belongs.
		//  2. Branch on the result instead of letting an empty pipeline
		//     stand for it, so the absent-provider case says which thing is
		//     absent rather than reading as "the agent logged nothing".
		//  3. exit 0. Suppressing the error still leaves a non-zero exit,
		//     which the caller reports as "could not read the service log" —
		//     true of a missing powershell.exe, misleading here. A launch
		//     failure still surfaces, because Go reports that itself.
		//
		// ASCII only: a redirected PowerShell pipeline decodes child output
		// with the console's ANSI code page, so anything else arrives
		// mangled on a non-UTF-8 console.
		ps := fmt.Sprintf(
			`$ErrorActionPreference='SilentlyContinue'; `+
				`$e = Get-WinEvent -FilterHashtable @{ProviderName='waired-agent'; StartTime=(Get-Date).AddSeconds(-%d)}; `+
				`if ($e) { $e | Format-List TimeCreated,LevelDisplayName,Message } `+
				`else { Write-Output 'no waired-agent events in this window (the Event Log source is registered at install time)' }; `+
				`exit 0`, secs)
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

// DefaultBundleBudget caps how many bytes of service log files land in
// one bundle, across every file and generation together.
//
// A budget rather than the per-file cap this used to be: the rotation
// policy now keeps up to 128 MB per generation and ten generations of
// them (logrotate.PolicyForLevel), so a fixed per-file tail would sample
// a sliver of each and a per-file cap large enough to be useful would
// multiply by however many files the OS has. What a bug report wants is
// "as much of the recent past as will still attach to an issue", which is
// one number spent newest-first.
//
// 16 MB leaves room under the 25 MB GitHub attachment limit for the
// service log and the engine logs collected around it. `waired logs
// --full` sets it to zero, which means no bound.
const DefaultBundleBudget = 16 << 20

// serviceLogFiles returns the plain log files to collect on goos, in the
// (GOOS, facts) -> plan shape the cross-OS parity rule asks for.
//
// Two different reasons a file exists here. On darwin launchd points the
// service's stdout/stderr at files, which is why only macOS could lose
// them to a rotation (#331). On Windows the Event Log that
// serviceLogCommand queries carries Warn and above only, so the agent
// keeps its own INFO/DEBUG file under the state dir (#636) — without it
// this bundle held no agent records at all on Windows, just the bundled
// engine's. Linux has neither: the journal holds everything and
// serviceLogCommand already reads it.
//
// An empty home yields no tray entries, and an empty stateDir no Windows
// entry, rather than a guessed path.
func serviceLogFiles(goos, home, stateDir string) []string {
	switch goos {
	case "darwin":
		files := []string{logrotate.AgentErrPath, logrotate.AgentOutPath}
		if home != "" {
			files = append(files, logrotate.TrayErrPath(home), logrotate.TrayOutPath(home))
		}
		return files
	case "windows":
		if p := logrotate.AgentOwnedLogFile(goos, stateDir); p != "" {
			return []string{p}
		}
		return nil
	default:
		return nil
	}
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
// ask users to run for a bug report. Windows had the same hole for a
// different reason until #636.
func collectServiceLogFiles(w io.Writer, goos, home, stateDir string, budget int64) {
	files := serviceLogFiles(goos, home, stateDir)
	if len(files) == 0 {
		if goos == "windows" {
			// The agent's file lives under the state dir, so without one
			// there is nothing to look for — say which is missing rather
			// than implying Windows keeps no file.
			fprintf(w, "(no --state-dir given; skipping the agent log file)\n")
			return
		}
		fprintf(w, "(no service log files on %s; the service log above is the source)\n", goos)
		return
	}
	appendServiceLogFiles(w, files, budget)
}

// appendServiceLogFiles takes the resolved paths rather than deriving
// them, so the collection itself is testable against a temp directory
// while serviceLogFiles is table-tested as a pure plan.
//
// budget bounds the total decompressed bytes of log content across every
// file and generation; 0 means no bound (`waired logs --full`). It is
// spent newest-first, because the end of a bug report's window is the part
// that explains it, while the output stays oldest-first so the bundle
// still reads forward in time.
//
// Spending newest-first is why the content is buffered rather than
// streamed: what belongs in the bundle cannot be decided until the newer
// generations have been read. The buffer is bounded by budget itself, and
// the unbounded --full path streams as before.
func appendServiceLogFiles(w io.Writer, files []string, budget int64) {
	remaining := budget
	// dropped, not "remaining == 0": a budget that lands exactly on the
	// last byte collected everything, and saying otherwise would send an
	// operator looking on disk for generations that are already in front of
	// them.
	dropped := false
	for _, base := range files {
		archives := archivesFor(base) // oldest first
		if budget <= 0 {
			for _, a := range archives {
				appendLogFile(w, a, true, 0)
			}
			appendLogFile(w, base, false, 0)
			warnLegacyRotationGap(w, base, archives)
			continue
		}

		// Newest first for the spend: the live file, then .0.gz, .1.gz…
		var chunks [][]byte
		for i, path := range newestFirst(base, archives) {
			if remaining <= 0 {
				dropped = dropped || i < len(archives)+1
				break
			}
			var buf bytes.Buffer
			gzipped := path != base
			if appendLogFile(&buf, path, gzipped, remaining) {
				dropped = true // this one was cut short mid-file
			}
			remaining -= int64(buf.Len())
			chunks = append(chunks, buf.Bytes())
		}
		for i := len(chunks) - 1; i >= 0; i-- {
			_, _ = w.Write(chunks[i])
		}
		warnLegacyRotationGap(w, base, archives)
	}
	if dropped {
		fprintf(w, "\n(stopped at the %d MB bundle budget; older log is still on disk. "+
			"Re-run with --full to collect all of it.)\n", budget>>20)
	}
}

// newestFirst orders one base's generations from newest to oldest: the
// live file, then the archives in reverse of archivesFor's reading order.
func newestFirst(base string, archives []string) []string {
	out := make([]string, 0, len(archives)+1)
	out = append(out, base)
	for i := len(archives) - 1; i >= 0; i-- {
		out = append(out, archives[i])
	}
	return out
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
//
// limit caps the decompressed content; 0 means the whole file. The header
// and any note are written whatever the limit, so a generation that ran
// out of budget still says it exists and how big it is.
//
// Reports whether content was left behind, which is what lets the caller
// tell "the budget happened to be exactly enough" from "there is more on
// disk".
func appendLogFile(w io.Writer, path string, gzipped bool, limit int64) (truncated bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return false // absent is the norm: no archives yet, no tray installed
	}
	// "compressed" on archives so the byte count here is not read against
	// the truncation note below it, which counts decompressed bytes.
	unit := "bytes"
	if gzipped {
		unit = "bytes compressed"
	}
	fprintf(w, "\n----- %s (%d %s, modified %s) -----\n",
		path, fi.Size(), unit, fi.ModTime().Format(time.RFC3339))

	f, err := os.Open(path)
	if err != nil {
		fprintf(w, "(could not read: %v)\n", err)
		return false
	}
	defer f.Close()

	var r io.Reader = f
	if gzipped {
		zr, err := gzip.NewReader(f)
		if err != nil {
			fprintf(w, "(could not decompress: %v)\n", err)
			return false
		}
		defer zr.Close()
		r = zr
	}
	lim := -1 // no bound
	if limit > 0 {
		lim = int(limit)
	}
	data, cut, err := readTail(r, lim)
	if err != nil {
		fprintf(w, "(read failed after %d bytes: %v)\n", len(data), err)
	}
	if cut {
		fprintf(w, "(truncated to the last %d bytes)\n", len(data))
	}
	_, _ = w.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		_, _ = io.WriteString(w, "\n")
	}
	return cut
}

// readTail returns at most limit trailing bytes of r, reporting whether
// anything was dropped. A negative limit reads the whole stream
// (`waired logs --full`). The kept region starts at a line boundary so the
// bundle never opens mid-record.
func readTail(r io.Reader, limit int) (data []byte, truncated bool, err error) {
	var buf []byte
	if limit > 0 {
		buf = make([]byte, 0, limit)
	}
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if limit > 0 && len(buf) > limit {
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
// file, and the one rotated generation beside it. Missing directories are
// skipped silently (an engine may not be installed).
//
// The rotated generation is collected because the rotation exists FOR this
// bundle, so that the trace explaining a crash survives the respawn and
// reaches CI, `waired doctor` and a bug report. A `.log` suffix filter
// dropped it again on the way out, which is how a run whose engine
// respawned could show no trace of a download that had in fact been
// dispatched (waired-agent#642).
//
// Both engines produce one, by different routes: internal/runtime's
// ollama openEngineLog renames engine.log to engine.log.1 on every
// spawn, and its vLLM counterpart appends spawns into one file and
// rotates when that file reaches its cap (waired-agent#878). Until then
// the vLLM `.1` could not exist, so half of the loop below was dead.
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
			// "Not there" is the ordinary case — one of these two engines
			// is always absent — but anything else is a directory we were
			// meant to read and could not, and saying nothing about it
			// leaves "(no engine logs found)" standing as a finding. On a
			// service install the state dir belongs to the service user,
			// so an unelevated `waired logs` hits exactly this and ships a
			// bug report with the engine's own account missing
			// (waired-agent#1196). Same treatment the per-file branch
			// below already gives its own failures.
			if !errors.Is(err, fs.ErrNotExist) {
				found = true
				fprintf(w, "\n----- %s -----\n(could not list: %v)\n", logDir, err)
			}
			continue
		}
		// Oldest generation first, so the file reads in the order the
		// engine lived it. os.ReadDir sorts by name, which would put
		// engine.log before engine.log.1 — backwards.
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".log.1") {
				names = append(names, e.Name())
			}
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
				names = append(names, e.Name())
			}
		}
		for _, name := range names {
			found = true
			p := filepath.Join(logDir, name)
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
