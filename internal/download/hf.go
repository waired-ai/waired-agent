//go:build linux || darwin

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
	"syscall"
)

// HFRunner is the test seam for hf-cli download. Same shape as
// CommandRunner (HF pulls export HF_TOKEN / HF_HUB_ENABLE_HF_TRANSFER;
// ollama pulls export OLLAMA_HOST); kept separate because the two
// pipelines parse different progress formats.
type HFRunner interface {
	// Run executes binary with args, augmenting the parent's env with
	// every "KEY=VALUE" entry in env. onLine receives every output
	// line (stderr and stdout merged, with both '\n' and '\r' treated
	// as separators because hf-cli's tqdm output uses '\r' to update
	// the same progress line).
	Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error
}

// HFPuller drives `huggingface-cli download` from the vLLM venv. It
// is the sibling of (and shares ProgressUI conventions with) the
// Ollama Puller.
type HFPuller struct {
	binary string
	runner HFRunner
}

// NewHFPuller wires a Puller. The binary is the venv-local
// huggingface-cli (e.g. ~/.local/share/waired/runtimes/vllm/current/.venv/bin/huggingface-cli).
func NewHFPuller(binary string, runner HFRunner) *HFPuller {
	return &HFPuller{binary: binary, runner: runner}
}

// HFPullOpts customises the download.
type HFPullOpts struct {
	// LocalDir is the absolute target directory; the resolved weights
	// land directly inside it (--local-dir-use-symlinks=False).
	LocalDir string

	// Revision pins a commit SHA; empty selects "main".
	Revision string

	// Token, if non-empty, exports HF_TOKEN to the subprocess so gated
	// repos can be pulled. Step 2 catalog only lists public repos so
	// this is normally empty; operators may inject HF_TOKEN via
	// agent.json or env when adding gated entries downstream.
	Token string

	// FastTransfer chooses whether to attempt hf_transfer (the Rust
	// parallel downloader bundled into the venv). True by default;
	// the auto-fallback in Pull retries with FastTransfer=false when
	// the first attempt fails, since older NAT/proxy setups have
	// known compatibility issues with hf_transfer's connection model.
	FastTransfer bool
}

// HFErrorClass is a coarse classification of Pull failures so callers
// (CLI and agent bootstrap) can decide whether to retry or surface
// to the user.
type HFErrorClass string

const (
	HFErrUnknown   HFErrorClass = "unknown"
	HFErrTransport HFErrorClass = "transport" // network blip, hf_transfer incompat
	HFErrAuth      HFErrorClass = "auth"      // 401/403 — likely gated without token
	HFErrNotFound  HFErrorClass = "not_found" // repo or revision missing
)

// HFError is the error returned by Pull on non-zero exit so callers
// can branch on the class without string-matching.
type HFError struct {
	Class HFErrorClass
	Repo  string
	Cause error
	Lines []string // last few lines of output for diagnostics
}

func (e *HFError) Error() string {
	return fmt.Sprintf("download: hf %s pull failed (%s): %v", e.Repo, e.Class, e.Cause)
}

func (e *HFError) Unwrap() error { return e.Cause }

// Pull invokes `huggingface-cli download <repo> --local-dir <dir>`
// (with --local-dir-use-symlinks=False so vLLM can read the files
// directly) and forwards parsed Progress events to onProgress. When
// FastTransfer is true (the default) it sets HF_HUB_ENABLE_HF_TRANSFER=1;
// on failure it retries once with =0 because some networks reject
// hf_transfer's parallel chunked download pattern.
func (p *HFPuller) Pull(ctx context.Context, repo string, opts HFPullOpts, onProgress func(Progress)) error {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}
	if opts.LocalDir == "" {
		return fmt.Errorf("download: HFPullOpts.LocalDir required")
	}
	// huggingface_hub 1.0 renamed `huggingface-cli` → `hf` and dropped
	// the `--local-dir-use-symlinks` flag (the new CLI always copies
	// to --local-dir). Both old and new CLIs accept this arg shape.
	args := []string{
		"download", repo,
		"--local-dir", opts.LocalDir,
	}
	if opts.Revision != "" {
		args = append(args, "--revision", opts.Revision)
	}

	attempt := func(fast bool) error {
		env := []string{}
		if fast {
			env = append(env, "HF_HUB_ENABLE_HF_TRANSFER=1")
		} else {
			env = append(env, "HF_HUB_ENABLE_HF_TRANSFER=0")
		}
		if opts.Token != "" {
			env = append(env, "HF_TOKEN="+opts.Token)
		}
		var lastLines []string
		runErr := p.runner.Run(ctx, p.binary, args, env, func(line string) {
			if line == "" {
				return
			}
			lastLines = appendCapped(lastLines, line, 32)
			onProgress(parseHFProgressLine(line))
		})
		if runErr == nil {
			return nil
		}
		return &HFError{
			Class: classifyHFError(runErr, lastLines),
			Repo:  repo,
			Cause: runErr,
			Lines: lastLines,
		}
	}

	if !opts.FastTransfer {
		return attempt(false)
	}
	first := attempt(true)
	if first == nil {
		return nil
	}
	// Auto-fallback: hf_transfer transport problems are common on
	// proxied / NAT'd networks; retry once with the plain HTTP path
	// before reporting failure. Auth / not-found errors don't benefit
	// from a retry.
	hfErr := &HFError{}
	if errors.As(first, &hfErr) && (hfErr.Class == HFErrAuth || hfErr.Class == HFErrNotFound) {
		return first
	}
	onProgress(Progress{State: StateUnknown, Percent: -1, Message: "hf_transfer fallback: retrying with HF_HUB_ENABLE_HF_TRANSFER=0"})
	return attempt(false)
}

// DefaultHFRunner shells out to a real huggingface-cli binary.
type DefaultHFRunner struct{}

func (DefaultHFRunner) Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	// Inherit parent env, then layer the runner-supplied entries on
	// top so they win on key collision.
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("download: hf stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("download: hf stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("download: start huggingface-cli: %w", err)
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

// hfShardsRE matches lines like:
//
//	Downloading shards: 50%|███▎      | 7/14 [02:13<02:08]
//
// and lets us extract the percent. The regex is intentionally
// permissive about the bar characters.
var hfShardsRE = regexp.MustCompile(`Downloading\s+(?:shards|model[- ]?\d*\.safetensors).*?\b(\d{1,3})%`)

// hfPlainPercentRE matches an unadorned NN% on a line that mentions
// "downloading" or "fetching" — covers older hf_hub output formats.
var hfPlainPercentRE = regexp.MustCompile(`(?:^|[^.\d])(\d{1,3})%`)

// hfVerifyRE recognises the verifying / writing-to-disk phase.
var hfVerifyRE = regexp.MustCompile(`(?i)\b(?:verifying|computing checksums|fetching\s+\d+\s+files)\b`)

// hfDoneRE recognises the success line. hf-cli prints the local path on
// the last line of a successful download, so a path-shaped string
// counts.
var hfDoneRE = regexp.MustCompile(`^/[^\s]+$`)

// parseHFProgressLine classifies one line of huggingface-cli output.
// The classification is best-effort: hf_hub doesn't guarantee a stable
// progress format, so we map noise to StateUnknown.
func parseHFProgressLine(line string) Progress {
	l := strings.TrimSpace(line)
	if l == "" {
		return Progress{State: StateUnknown, Percent: -1}
	}
	if m := hfShardsRE.FindStringSubmatch(l); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= 0 && n <= 100 {
			return Progress{State: StatePulling, Percent: n, Message: line}
		}
	}
	if hfVerifyRE.MatchString(l) {
		return Progress{State: StateVerifying, Percent: -1, Message: line}
	}
	if hfDoneRE.MatchString(l) {
		return Progress{State: StateSuccess, Percent: 100, Message: line}
	}
	if m := hfPlainPercentRE.FindStringSubmatch(l); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= 0 && n <= 100 && (strings.Contains(strings.ToLower(l), "download") || strings.Contains(strings.ToLower(l), "fetch")) {
			return Progress{State: StatePulling, Percent: n, Message: line}
		}
	}
	return Progress{State: StateUnknown, Percent: -1, Message: line}
}

// classifyHFError inspects exit error + last output lines to decide
// what kind of failure happened. Network errors retry under the
// auto-fallback path; auth / not_found errors short-circuit.
func classifyHFError(runErr error, lines []string) HFErrorClass {
	for _, l := range lines {
		ll := strings.ToLower(l)
		switch {
		case strings.Contains(ll, "401") || strings.Contains(ll, "403") ||
			strings.Contains(ll, "unauthorized") || strings.Contains(ll, "forbidden") ||
			strings.Contains(ll, "gated"):
			return HFErrAuth
		case strings.Contains(ll, "404") || strings.Contains(ll, "not found") ||
			strings.Contains(ll, "does not exist"):
			return HFErrNotFound
		case strings.Contains(ll, "connection") || strings.Contains(ll, "timeout") ||
			strings.Contains(ll, "ssl") || strings.Contains(ll, "tls") ||
			strings.Contains(ll, "hf_transfer"):
			return HFErrTransport
		}
	}
	return HFErrUnknown
}

// EstimateRequiredBytes converts a manifest's variant.estimated_weight_gb
// to a bytes-on-disk requirement with the plan's 20% headroom factor.
func EstimateRequiredBytes(estimatedWeightGB float64) int64 {
	const bytesPerGB = 1024 * 1024 * 1024
	return int64(estimatedWeightGB * 1.2 * float64(bytesPerGB))
}

// appendCapped appends s to buf, dropping the oldest entry when buf
// would exceed cap. Used to keep the diagnostic tail bounded.
func appendCapped(buf []string, s string, cap int) []string {
	if cap <= 0 {
		return buf
	}
	if len(buf) >= cap {
		buf = buf[1:]
	}
	return append(buf, s)
}

// ErrHFCLINotInstalled is the analogue of ErrNotInstalled for the HF
// CLI (either the new `hf` binary from huggingface_hub 1.0+ or the
// deprecated `huggingface-cli` from earlier versions).
var ErrHFCLINotInstalled = errors.New("download: HF CLI not found (install vLLM venv first)")

// ResolveHFCLI returns the absolute path to the HF CLI, preferring
// an explicit override (typically the vLLM venv's bin/hf or
// bin/huggingface-cli). When no override is given it looks up `hf`
// first (huggingface_hub 1.0+) and falls back to `huggingface-cli`.
func ResolveHFCLI(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if path, err := exec.LookPath("hf"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("huggingface-cli"); err == nil {
		return path, nil
	}
	return "", ErrHFCLINotInstalled
}
