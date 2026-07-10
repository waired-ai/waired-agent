// Package download wraps the per-runtime model download tools so the
// rest of waired-agent can drive them through a uniform Pull / state
// machine. Phase A only ships the Ollama path (`ollama pull <tag>`);
// Hugging Face fetching (for vLLM weights) is part of Phase B.
package download

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Pull-progress states emitted to the user-supplied callback. They
// roughly map to the ModelState lifecycle in internal/catalog (see
// spec waired_inference_spec.md §9.3).
const (
	StatePulling   = "pulling"
	StateVerifying = "verifying"
	StateSuccess   = "success"
	StateUnknown   = "unknown"
)

// Progress is one update emitted while a pull is in flight.
type Progress struct {
	State   string // one of StatePulling / StateVerifying / StateSuccess / StateUnknown
	Percent int    // 0-100 when known; -1 otherwise
	Message string // raw line for display / logging

	// Digest identifies the layer this update is about (the hex token
	// after "pulling " in the Ollama line), or "" for the manifest /
	// non-layer lines. Used to aggregate progress across the several
	// layers a model pull streams.
	Digest string
	// Completed / Total are the layer's byte counts when Ollama reports
	// them ("2.3 GB/5.0 GB"); 0 when unknown. Bytes, decimal-ish units as
	// Ollama prints them (GB→1e9, GiB→2^30).
	Completed int64
	Total     int64
	// BytesPerSec is the layer's download speed ("40 MB/s"); 0 when
	// unknown.
	BytesPerSec int64
}

// CommandRunner is the seam where tests inject fake `ollama pull`
// behaviour without spawning a real subprocess.
type CommandRunner interface {
	// Run executes `binary args...`, augmenting the parent's env with
	// every "KEY=VALUE" entry in env and calling onLine for every line
	// the command writes (stderr first, stdout merged). It must
	// return after the command exits or the context is cancelled.
	Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error
}

// Puller drives `ollama pull` and parses its progress output.
type Puller struct {
	binary string
	runner CommandRunner
	env    []string
}

// NewPuller wires a Puller with the given ollama binary path and
// command runner. Pass DefaultRunner{} for production. env entries
// ("KEY=VALUE") are exported to every pull subprocess — `ollama pull`
// is a CLIENT of the serving engine, so callers MUST pass
// "OLLAMA_HOST=127.0.0.1:<port>" whenever the engine is not on the
// upstream default 11434 (the waired-owned bundled port 9475 is not),
// or the pull lands on whatever answers 11434 instead.
func NewPuller(binary string, runner CommandRunner, env ...string) *Puller {
	return &Puller{binary: binary, runner: runner, env: env}
}

// Pull runs `ollama pull <tag>` and forwards parsed Progress events
// to onProgress (which may be nil). It returns the runner's error
// verbatim on failure; success is determined by the runner's exit
// status, not by any particular line of output.
func (p *Puller) Pull(ctx context.Context, tag string, onProgress func(Progress)) error {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}
	// Lazily resolve the binary when it was empty at construction time:
	// an agent that booted before ollama was installed can pull as soon
	// as the binary appears, without a restart (#188).
	binary := p.binary
	if binary == "" {
		resolved, err := ResolveBinary("")
		if err != nil {
			return err
		}
		binary = resolved
	}
	return p.runner.Run(ctx, binary, []string{"pull", tag}, p.env, func(line string) {
		if line == "" {
			return
		}
		onProgress(parseProgressLine(line))
	})
}

// DefaultRunner shells out to a real ollama binary and forwards each
// line (split on \n and \r — Ollama uses \r to update the same
// progress line).
type DefaultRunner struct{}

func (DefaultRunner) Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("download: stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("download: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("download: start ollama: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	scan := func(r io.Reader) {
		defer wg.Done()
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		s.Split(splitLinesOrCR)
		for s.Scan() {
			onLine(strings.TrimSpace(s.Text()))
		}
	}
	go scan(stderr)
	go scan(stdout)
	wg.Wait()

	return cmd.Wait()
}

// splitLinesOrCR is a bufio.SplitFunc that treats both '\n' and '\r'
// as record separators. Ollama's TUI progress overwrites the same
// line via '\r', and the default Scanner would otherwise buffer the
// whole stream until EOF.
func splitLinesOrCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseProgressLine classifies one line of `ollama pull` output. The
// known status strings come from observing Ollama 0.x output; new
// status keywords default to StateUnknown so they're harmless.
func parseProgressLine(line string) Progress {
	l := strings.ToLower(strings.TrimSpace(line))
	if l == "" {
		return Progress{State: StateUnknown, Percent: -1}
	}
	switch {
	case l == "success":
		return Progress{State: StateSuccess, Percent: -1, Message: line}
	case strings.HasPrefix(l, "verifying"),
		strings.HasPrefix(l, "writing manifest"),
		strings.HasPrefix(l, "removing any unused"):
		return Progress{State: StateVerifying, Percent: -1, Message: line}
	case strings.HasPrefix(l, "pulling"):
		completed, total := extractSizes(line)
		return Progress{
			State:       StatePulling,
			Percent:     extractPercent(line),
			Message:     line,
			Digest:      extractDigest(line),
			Completed:   completed,
			Total:       total,
			BytesPerSec: extractSpeed(line),
		}
	}
	return Progress{State: StateUnknown, Percent: -1, Message: line}
}

// digestRE captures the layer id after "pulling " up to the colon.
// "pulling manifest" (no colon) yields no match → Digest "".
var digestRE = regexp.MustCompile(`(?i)^pulling\s+([^\s:]+):`)

func extractDigest(s string) string {
	if m := digestRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// sizesRE matches "<completed> <unit>/<total> <unit>" (e.g. "2.3 GB/5.0 GB").
var sizesRE = regexp.MustCompile(`([0-9.]+)\s*([KMGTP]?i?B)\s*/\s*([0-9.]+)\s*([KMGTP]?i?B)`)

// extractSizes parses the "completed/total" byte counts. Returns 0,0 when
// the line has no slash-separated size pair (e.g. the "100% ... 5.0 GB"
// completion line, or "pulling manifest").
func extractSizes(s string) (completed, total int64) {
	m := sizesRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0
	}
	return parseSize(m[1], m[2]), parseSize(m[3], m[4])
}

// speedRE matches "<n> <unit>/s" (e.g. "40 MB/s").
var speedRE = regexp.MustCompile(`([0-9.]+)\s*([KMGTP]?i?B)/s`)

func extractSpeed(s string) int64 {
	if m := speedRE.FindStringSubmatch(s); m != nil {
		return parseSize(m[1], m[2])
	}
	return 0
}

// parseSize converts a value + unit ("2.3", "GB") into bytes. Decimal
// units (KB/MB/GB/TB/PB) use 1000; binary units (KiB/MiB/...) use 1024,
// matching how Ollama formats progress.
func parseSize(val, unit string) int64 {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0
	}
	var mult float64 = 1
	switch strings.ToUpper(unit) {
	case "B":
		mult = 1
	case "KB":
		mult = 1e3
	case "MB":
		mult = 1e6
	case "GB":
		mult = 1e9
	case "TB":
		mult = 1e12
	case "PB":
		mult = 1e15
	case "KIB":
		mult = 1 << 10
	case "MIB":
		mult = 1 << 20
	case "GIB":
		mult = 1 << 30
	case "TIB":
		mult = 1 << 40
	case "PIB":
		mult = 1 << 50
	}
	return int64(f * mult)
}

// percentRE matches a whole-number percent that is NOT preceded by a
// digit or '.' — that excludes fractional readings like "99.9%" from
// being misread as 9%.
var percentRE = regexp.MustCompile(`(?:^|[^.\d])(\d{1,3})%`)

// extractPercent finds the first whole-number NN% token in s and
// returns NN. It returns -1 when no whole-number percent is present
// (or the value is out of range).
func extractPercent(s string) int {
	m := percentRE.FindStringSubmatch(s)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 || n > 100 {
		return -1
	}
	return n
}

// HumanBytes formats a byte count as a short decimal-unit string
// ("2.3 GB", "512 MB", "0 B") for progress display. Mirrors how Ollama
// itself prints sizes so the rendered bar matches the underlying tool.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// ErrNotInstalled is returned by helper helpers when ollama is missing.
var ErrNotInstalled = errors.New("download: ollama binary not found")

// ResolveBinary returns the absolute path to ollama, in this order:
//  1. `override` (e.g. a bundled path or OllamaConfig.Binary)
//  2. `WAIRED_OLLAMA_BINARY` environment variable
//  3. exec.LookPath("ollama") — works on Linux/macOS and on Windows
//     for users whose PATH includes the Ollama installer
//  4. OS-specific well-known install paths (Windows: %ProgramFiles%,
//     %LOCALAPPDATA%\Programs; see ResolveBinary_windows for details)
//
// The third + fourth steps matter for Windows Service mode: when
// waired-agent runs as LocalSystem, the user's PATH is not inherited,
// so a plain LookPath misses Ollama even though it is installed.
func ResolveBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("WAIRED_OLLAMA_BINARY"); env != "" {
		return env, nil
	}
	if path, err := exec.LookPath(ollamaCmdName); err == nil {
		return path, nil
	}
	for _, candidate := range platformOllamaCandidates() {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", ErrNotInstalled
}
