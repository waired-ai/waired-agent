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
	"path/filepath"
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
	binary  string
	resolve func() (string, error)
	runner  CommandRunner
	env     []string
}

// NewPuller wires a Puller with the given ollama binary path and
// command runner. Pass DefaultRunner{} for production. env entries
// ("KEY=VALUE") are exported to every pull subprocess — `ollama pull`
// is a CLIENT of the serving engine, so callers MUST pass
// "OLLAMA_HOST=127.0.0.1:<port>" whenever the engine is not on the
// upstream default 11434 (the waired-owned bundled port 9475 is not),
// or the pull lands on whatever answers 11434 instead.
//
// Production builds a NewResolvingPuller instead; this constructor is for
// callers that already hold the path.
func NewPuller(binary string, runner CommandRunner, env ...string) *Puller {
	return &Puller{binary: binary, runner: runner, env: env}
}

// NewResolvingPuller is NewPuller for a caller whose ollama binary may
// not exist yet. waired's own install lives under the agent state dir,
// deliberately off $PATH (see cmd/waired-agent/engine_resolve.go), and
// resolving it is the daemon's rule, not this package's. resolve is
// consulted on every Pull, so an engine installed — or re-installed —
// after boot is picked up without an agent restart (#304).
func NewResolvingPuller(resolve func() (string, error), runner CommandRunner, env ...string) *Puller {
	return &Puller{resolve: resolve, runner: runner, env: env}
}

// Rendering names the engine-side prompt renderer and response parser a
// pulled tag must be stamped with. The zero value means "the tag as
// published already names its own", which is the usual case.
type Rendering struct {
	Renderer string
	Parser   string
}

// Wanted reports whether anything needs stamping.
func (r Rendering) Wanted() bool {
	return strings.TrimSpace(r.Renderer) != "" || strings.TrimSpace(r.Parser) != ""
}

// Pull runs `ollama pull <tag>`, stamps want onto the result, and
// forwards parsed Progress events to onProgress (which may be nil). It
// returns the runner's error verbatim on a failed pull; success is
// determined by the runner's exit status, not by any particular line of
// output.
//
// The stamp is INSIDE Pull rather than beside it, and the reason is
// measured: `ollama pull` on a tag that is already present takes about
// two seconds, moves no weights, and REWRITES the local manifest back to
// the published config — clearing the renderer. Verified on sv-evox2,
// where a re-pull of an already-stamped 78.87 GB tag returned
// `renderer=” parser=”` in 2 s with a 0.00 GB disk delta. A caller
// that pulls without re-stamping therefore does not leave the model
// merely unimproved; it leaves it refusing three of the six shapes a
// coding agent sends, having done so cheaply enough that it can happen
// on any update check. Making the two separable would make forgetting
// them separable too (waired-agent#1192).
func (p *Puller) Pull(ctx context.Context, tag string, want Rendering, onProgress func(Progress)) error {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}
	// Lazily resolve the binary when it was empty at construction time:
	// an agent that booted before ollama was installed can pull as soon
	// as the binary appears, without a restart (#188). Nothing is cached
	// back into p.binary: Puller has no mutex and Pull runs on many
	// goroutines.
	//
	// There is no fallback below this. Until #493 an unresolved binary
	// fell through to a $PATH / well-known-paths walk, which is how a pull
	// could land on an engine waired never installed (#139).
	binary := p.binary
	if binary == "" {
		if p.resolve == nil {
			return errors.New("download: no ollama binary and no resolver")
		}
		resolved, err := p.resolve()
		if err != nil {
			return err
		}
		binary = resolved
	}
	if err := p.runner.Run(ctx, binary, []string{"pull", tag}, p.env, func(line string) {
		if line == "" {
			return
		}
		onProgress(parseProgressLine(line))
	}); err != nil {
		return err
	}
	return p.stamp(ctx, binary, tag, want)
}

// Remove runs `ollama rm <tag>`, deleting the weights the matching Pull
// fetched. It lives on Puller rather than in a type of its own because
// the two need exactly the same three things — the resolved binary, the
// OLLAMA_HOST env that points the client at waired's own engine rather
// than whatever answers 11434, and the injectable runner — and a second
// type carrying that trio would be a second place to get it wrong.
//
// Removing weights the engine has resident is safe: ollama unloads the
// model rather than refusing, so a caller does not have to stop serving
// first.
//
// The runner's error is returned verbatim, and a tag that is already
// gone reports one — `ollama rm` exits non-zero for a name it does not
// know. Callers that treat deletion as idempotent must decide that for
// themselves rather than have it decided here, because "it was already
// gone" and "the engine could not be reached" arrive the same way and
// only the caller knows which one it can live with.
func (p *Puller) Remove(ctx context.Context, tag string) error {
	binary := p.binary
	if binary == "" {
		if p.resolve == nil {
			return errors.New("download: no ollama binary and no resolver")
		}
		resolved, err := p.resolve()
		if err != nil {
			return err
		}
		binary = resolved
	}
	return p.runner.Run(ctx, binary, []string{"rm", tag}, p.env, func(string) {})
}

// stamp rewrites tag's LOCAL manifest so the engine renders prompts with
// the named renderer instead of falling through to the GGUF's embedded
// chat template. Unexported on purpose: it is the tail of Pull, and a
// caller able to skip it is a caller able to leave a model refusing
// shapes this project records as accepted.
//
// This exists because the two paths that produce an ollama tag disagree.
// Converting safetensors stamps a renderer automatically, so a family's
// MLX tags carry one; packaging a GGUF does not, and no community
// publisher types it by hand — of the 24 GGUF tags on ollama.com
// carrying Qwen3.8-Flash-Next, not one declares a renderer, while every
// safetensors tag declares "qwen3.8". A tag with neither a renderer nor
// a template layer falls through to the GGUF's Jinja, and Qwen's Jinja
// raises "System message must be at the beginning." on three of the six
// shapes a coding agent sends (waired-agent#1192).
//
// The rewrite is cheap and in place. `ollama create` on the SAME tag
// name reuses every existing layer — weights, projector and the license
// blob are not copied, only the small config object is rewritten — so
// the model keeps the identity every caller downstream already holds.
// Measured on sv-evox2: 0.00 GB of additional disk, and the three
// refused shapes went 500 -> 200 with nothing else changed.
func (p *Puller) stamp(ctx context.Context, binary, tag string, want Rendering) error {
	if !want.Wanted() {
		return nil
	}
	renderer, parser := want.Renderer, want.Parser

	var mf strings.Builder
	fmt.Fprintf(&mf, "FROM %s\n", tag)
	if r := strings.TrimSpace(renderer); r != "" {
		fmt.Fprintf(&mf, "RENDERER %s\n", r)
	}
	if pa := strings.TrimSpace(parser); pa != "" {
		fmt.Fprintf(&mf, "PARSER %s\n", pa)
	}

	// A file, not stdin: `ollama create -f -` is not a documented
	// spelling, and a Modelfile whose FROM names a local tag resolves
	// relative to nothing on disk, so the file's location does not
	// matter.
	dir, err := os.MkdirTemp("", "waired-stamp-")
	if err != nil {
		return fmt.Errorf("download: stamp %s: %w", tag, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "Modelfile")
	if err := os.WriteFile(path, []byte(mf.String()), 0o600); err != nil {
		return fmt.Errorf("download: stamp %s: %w", tag, err)
	}

	// Same name in, same name out: this is a rewrite of the local
	// manifest, not a derived model. A second name would have to be
	// threaded through every caller that holds the tag.
	if err := p.runner.Run(ctx, binary, []string{"create", tag, "-f", path}, p.env, func(string) {}); err != nil {
		return fmt.Errorf("download: stamp %s with renderer %q: %w", tag, renderer, err)
	}
	return nil
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
	return ParseSize(m[1], m[2]), ParseSize(m[3], m[4])
}

// speedRE matches "<n> <unit>/s" (e.g. "40 MB/s").
var speedRE = regexp.MustCompile(`([0-9.]+)\s*([KMGTP]?i?B)/s`)

func extractSpeed(s string) int64 {
	if m := speedRE.FindStringSubmatch(s); m != nil {
		return ParseSize(m[1], m[2])
	}
	return 0
}

// ParseSize converts a value + unit ("2.3", "GB") into bytes. Decimal
// units (KB/MB/GB/TB/PB) use 1000; binary units (KiB/MiB/...) use 1024,
// matching how Ollama formats progress.
//
// Exported because uv denominates its own download announcements the
// same way ("506.1MiB") and internal/runtime's reader for them
// (uv_progress.go, waired-agent#255) must agree with this table rather
// than grow a second one.
func ParseSize(val, unit string) int64 {
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
