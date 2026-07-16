package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// inferenceChoice is what promptInference returns to runInit.
type inferenceChoice struct {
	Enabled       bool
	ShareWithMesh bool
}

// writePrompt / writePromptf wrap fmt.Fprintln/Fprintf so the call
// sites stay terse and we don't repeatedly discard the error returned
// by writing to a terminal Writer — terminal write failures here are
// not actionable.
func writePrompt(out io.Writer, args ...any) {
	_, _ = fmt.Fprintln(out, args...)
}

func writePromptf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// hardwareEnabledDefault returns the install-time Enabled default
// derived from the hardware profile. The rule is intentionally
// permissive: any host that exposes ≥ 8 GB of usable GPU/UMA memory is
// assumed to be a viable inference target. Apple Silicon and AMD Strix
// Halo (UnifiedMemory=true) pass via Profile.UsableVRAMMB. iGPUs and
// CPU-only hosts fall under the 8 GB threshold and default to N so a
// Windows laptop does not accidentally start serving weak inference
// the moment its operator hits enter through the installer.
func hardwareEnabledDefault(p hardware.Profile) bool {
	return p.EffectiveVRAMMB() >= 8192
}

// promptInference resolves the install-time inference choices.
// Precedence:
//
//  1. Explicit flag overrides (enabledOverride / shareOverride non-nil).
//  2. nonInteractive => hardware-derived default (no prompt).
//  3. Existing agent.json => existing values become prompt defaults.
//  4. No existing config => hardware-derived defaults are the prompt
//     defaults.
//
// When Enabled is resolved to false the ShareWithMesh question is
// skipped (sharing a non-existent engine is meaningless) and false is
// returned for it.
func promptInference(
	in io.Reader, out io.Writer,
	existing agentconfig.InferenceConfig, hasExisting bool,
	profile hardware.Profile,
	enabledOverride, shareOverride *bool,
	nonInteractive bool,
) inferenceChoice {
	hwDefault := hardwareEnabledDefault(profile)

	enabledDefault := hwDefault
	shareDefault := hwDefault
	if hasExisting {
		enabledDefault = existing.Enabled
		shareDefault = existing.ShareWithMesh
	}

	// Resolve Enabled.
	var enabled bool
	switch {
	case enabledOverride != nil:
		enabled = *enabledOverride
	case nonInteractive:
		enabled = enabledDefault
	default:
		if hasExisting {
			writePrompt(out, "Existing agent.json found — your previous answers are the defaults below.")
		}
		writePromptf(out, "Detected hardware: %s.\n", describeHardware(profile))
		sc := bufio.NewScanner(in)
		enabled = ynPrompt(out, sc,
			"Run a local inference engine here?", enabledDefault)
		// Resolve ShareWithMesh (only if Enabled).
		if !enabled {
			return inferenceChoice{Enabled: false, ShareWithMesh: false}
		}
		var share bool
		switch {
		case shareOverride != nil:
			share = *shareOverride
		default:
			share = ynPrompt(out, sc,
				"Share this engine with mesh peers?", shareDefault)
		}
		return inferenceChoice{Enabled: true, ShareWithMesh: share}
	}

	if !enabled {
		return inferenceChoice{Enabled: false, ShareWithMesh: false}
	}

	// Enabled resolved via flag or non-interactive path; resolve Share
	// via the same precedence.
	var share bool
	switch {
	case shareOverride != nil:
		share = *shareOverride
	default:
		share = shareDefault
	}
	return inferenceChoice{Enabled: true, ShareWithMesh: share}
}

// promptOllamaSource resolves how Ollama is provided (#188). The default
// is ALWAYS bundled (waired-managed), regardless of whether an existing
// Ollama is present or whether its version is supported — an unsupported
// detected version only adds a warning line. Reuse is strictly opt-in.
//
// Precedence:
//  1. override ("bundled"/"reuse" from --ollama-source) wins.
//  2. Waired-managed install detected (marker file) => bundled, no prompt:
//     waired itself installed that Ollama, so asking "reuse the existing
//     one?" about it only confused first runs.
//  3. No existing Ollama detected, or non-interactive => bundled.
//  4. Existing Ollama detected => prompt, default bundled.
func promptOllamaSource(
	in io.Reader, out io.Writer,
	det setup.OllamaDetection, override string, nonInteractive bool,
) string {
	switch override {
	case agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceReuse:
		return override
	}
	if det.Installed && det.WairedManaged {
		writePromptf(out, "Using the Ollama that Waired installed (%s).\n", det.Path)
		return agentconfig.OllamaSourceBundled
	}
	if !det.Installed || nonInteractive {
		return agentconfig.OllamaSourceBundled
	}

	ver := det.Version
	if ver == "" {
		ver = "unknown version"
	}
	if det.Supported {
		writePromptf(out, "Detected an existing Ollama at %s (%s).\n", det.Path, ver)
	} else {
		writePromptf(out,
			"Detected an existing Ollama at %s (%s) — below waired's supported minimum %s; the bundled engine is recommended.\n",
			det.Path, ver, infruntime.OllamaSupportedMinVersion)
	}
	sc := bufio.NewScanner(in)
	// Default true = bundled (recommended). Answering N opts into reuse.
	useBundled := ynPrompt(out, sc,
		"Use Waired's bundled Ollama? (n = reuse the existing one)", true)
	if useBundled {
		return agentconfig.OllamaSourceBundled
	}
	return agentconfig.OllamaSourceReuse
}

// validateOllamaSourceFlag rejects a non-empty --ollama-source that is not a
// known engine source. Empty means "no flag passed" (prompt / keep existing).
// Without this guard a typo silently falls through to the bundled default
// (enroll) or is dropped (renew, #485) with no feedback.
func validateOllamaSourceFlag(v string) error {
	switch v {
	case "", agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceReuse:
		return nil
	default:
		return fmt.Errorf("--ollama-source must be %q or %q, got %q",
			agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceReuse, v)
	}
}

// effectiveOllamaSource maps an empty ollama_source (pre-#188 agent.json) to the
// bundled default the agent actually uses, so renew comparisons and operator
// messages don't surface a spurious "" source.
func effectiveOllamaSource(s string) string {
	if s == "" {
		return agentconfig.OllamaSourceBundled
	}
	return s
}

// renewOllamaSourceChange decides whether an explicit --ollama-source should
// change the on-disk ollama_source on the renew (re-auth) path (#485). An empty
// override (no flag) or an override that equals the current effective source is a
// no-op; otherwise the override is the value to persist. The caller is expected
// to have validated override via validateOllamaSourceFlag first.
func renewOllamaSourceChange(current, override string) (next string, changed bool) {
	if override == "" || override == effectiveOllamaSource(current) {
		return current, false
	}
	return override, true
}

// describeHardware renders a short, operator-facing summary of the
// detected hardware so the prompt's "default" choice is not magic.
func describeHardware(p hardware.Profile) string {
	if len(p.GPUs) == 0 {
		return fmt.Sprintf("CPU only (RAM %d GB)", p.RAMTotalGB)
	}
	g := p.GPUs[0]
	label := g.Model
	if label == "" {
		label = g.Vendor
	}
	if n := len(p.GPUs); n > 1 && !p.UnifiedMemory {
		// #678: surface additional devices; the figure stays per-device.
		label = fmt.Sprintf("%s ×%d", label, n)
	}
	vram := p.EffectiveVRAMMB()
	suffix := ""
	if p.UnifiedMemory {
		suffix = ", unified memory"
	}
	return fmt.Sprintf("%s (%d MB usable VRAM%s)", label, vram, suffix)
}

// ynPrompt reads one [Y/n] / [y/N] answer. Empty input returns def.
// Unparseable input re-prompts up to 3 times then falls back to def.
// Reads through the supplied scanner (caller owns it).
func ynPrompt(out io.Writer, sc *bufio.Scanner, label string, def bool) bool {
	// Spell out the default ("default: Yes/No") alongside the [Y/n]
	// capitalization so it reads like a conventional interactive installer
	// — the older "(Enter = Yes)" form looked like an instruction to type
	// the word "Yes".
	hint := "[Y/n] (default: Yes)"
	if !def {
		hint = "[y/N] (default: No)"
	}
	for range 3 {
		writePromptf(out, "  %s %s ", label, hint)
		if !sc.Scan() {
			return def
		}
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		switch line {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		writePrompt(out, "  please answer y or n.")
	}
	return def
}

// flagBoolPtr registers a tri-state bool flag on fs. The returned
// pointer is nil if the operator did not pass the flag, &true / &false
// otherwise. Go's stdlib flag package cannot express "unset" directly,
// so we wrap a flag.Value that captures the pointer on Set.
func flagBoolPtr(fs *flag.FlagSet, name, usage string) **bool {
	holder := new(*bool)
	fs.Var(&boolPtrValue{dst: holder}, name, usage)
	return holder
}

type boolPtrValue struct {
	dst **bool
}

func (b *boolPtrValue) Set(s string) error {
	switch strings.ToLower(s) {
	case "true", "1", "t", "yes", "y":
		v := true
		*b.dst = &v
	case "false", "0", "f", "no", "n":
		v := false
		*b.dst = &v
	default:
		return fmt.Errorf("invalid bool %q", s)
	}
	return nil
}

func (b *boolPtrValue) String() string {
	if b == nil || b.dst == nil || *b.dst == nil {
		return ""
	}
	return fmt.Sprintf("%t", **b.dst)
}

// IsBoolFlag tells the flag package that this value accepts the bare
// `--flag` form (= "true"). Without this, `--inference-enabled` (no
// =value) would consume the next positional argument.
func (b *boolPtrValue) IsBoolFlag() bool { return true }

// downloadLineState carries the per-render throttle bookkeeping for
// drawDownloadLine across successive calls. Initialise lastPct to -1 (the
// zero value 0 would suppress the first non-TTY draw at 0%).
type downloadLineState struct {
	lastDraw time.Time
	lastPct  int
}

// drawDownloadLine renders one aggregated model-download progress line, e.g.:
//
//	⬇️  Downloading qwen2.5-coder-7b-instruct: 45%  2.3 GB / 5.0 GB (40.0 MB/s)
//
// speed is the smoothed transfer rate: rendered whenever it is known (>= 0)
// — including "(0 B/s)", which tells a stalled transfer apart from a frozen
// UI — and omitted only while still unknown (< 0, before the first rate
// sample). The byte counts alone tick too coarsely (0.1 GB steps) to prove
// liveness on a slow link. On a TTY the line is rewritten in place (\r),
// time-throttled to ~150 ms so the speed stays lively; off a TTY (piped
// install logs) it emits a fresh line per ~10% — or per ~5 s when the total
// is unknown — so logs stay readable without \r spam. st carries the
// throttle state between calls. Shared by cliPullProgressSink (Deploy's
// foreground pre-pull), waitForBundledModel (init's post-start /status
// wait), and the `runtimes install ollama` tarball download so all render
// identically.
func drawDownloadLine(out io.Writer, tty bool, st *downloadLineState, model string, pct int, completed, total, speed int64) {
	const throttle = 150 * time.Millisecond
	const ttyBucket = 10               // non-tty: redraw per 10%
	const plainEvery = 5 * time.Second // non-tty cadence when pct is unknown
	now := time.Now()
	switch {
	case tty:
		if !st.lastDraw.IsZero() && now.Sub(st.lastDraw) < throttle {
			return
		}
	case pct >= 0:
		if st.lastPct >= 0 && pct/ttyBucket == st.lastPct/ttyBucket {
			return
		}
	default: // non-tty with no percentage to bucket on: fall back to time
		if !st.lastDraw.IsZero() && now.Sub(st.lastDraw) < plainEvery {
			return
		}
	}
	st.lastDraw = now
	st.lastPct = pct
	label := emo("⬇️", "[..]")
	rate := ""
	if speed >= 0 {
		rate = fmt.Sprintf(" (%s/s)", download.HumanBytes(speed))
	}
	var line string
	switch {
	case total > 0:
		line = fmt.Sprintf("%s  Downloading %s: %3d%%  %s / %s%s",
			label, model, pct, download.HumanBytes(completed), download.HumanBytes(total), rate)
	case completed > 0: // length unknown (no Content-Length): bytes so far
		line = fmt.Sprintf("%s  Downloading %s: %s%s",
			label, model, download.HumanBytes(completed), rate)
	default:
		line = fmt.Sprintf("%s  Downloading %s…", label, model)
	}
	if tty {
		// Pad to clear any residue from a longer previous line; no trailing
		// newline so the next draw overwrites in place.
		writePromptf(out, "\r%-78s", line)
	} else {
		writePrompt(out, line)
	}
}

// cliPullProgressSink builds a ProgressSink that renders a single,
// aggregated download bar for the bundled model, e.g.:
//
//	⬇️  Downloading qwen2.5-coder-7b-instruct: 45%  2.3 GB / 5.0 GB (40 MB/s)
//
// Ollama streams progress per layer (each layer climbs 0→100% then
// "verifying"); echoing every event produced the noisy
// "pull: 100% / verifying..." loop. We aggregate completed/total bytes
// across all layers (keyed by digest) so the percentage is one smooth
// number, dedupe the verifying line, and finish with a single
// "✅ ready". On a TTY the line is rewritten in place (\r), time-throttled
// to ~150ms so the speed stays lively; off a TTY (piped install logs) we
// emit a fresh line per ~10% so logs stay readable without \r spam.
func cliPullProgressSink(out io.Writer, tty bool) func(setup.PullEvent) {
	type layer struct{ completed, total int64 }
	var (
		layers    = map[string]layer{}
		line      = downloadLineState{lastPct: -1}
		announced bool
		hinted    bool
		verifying bool
		done      bool
	)

	return func(ev setup.PullEvent) {
		model := ev.ModelName
		if model == "" {
			model = "model"
		}
		switch ev.State {
		case download.StatePulling:
			if ev.Digest != "" && ev.Total > 0 {
				layers[ev.Digest] = layer{ev.Completed, ev.Total}
			}
			var sumC, sumT int64
			for _, l := range layers {
				sumC += l.completed
				sumT += l.total
			}
			pct := ev.Percent
			if sumT > 0 {
				pct = int(sumC * 100 / sumT)
			}
			if !hinted {
				hinted = true
				writePrompt(out, dim("Downloading the model — a multi-GB transfer that can take a few minutes."))
			}
			announced = true
			speed := ev.BytesPerSec
			if speed <= 0 {
				speed = -1 // this stream event carried no rate: omit the segment
			}
			drawDownloadLine(out, tty, &line, model, pct, sumC, sumT, speed)
		case download.StateVerifying:
			if !verifying {
				verifying = true
				if announced && tty {
					writePrompt(out) // close the \r progress line
				}
				writePromptf(out, "%s  Verifying %s…\n", emo("🔍", "[..]"), model)
			}
		case download.StateSuccess:
			if !done {
				done = true
				if announced && tty && !verifying {
					writePrompt(out)
				}
				writePromptf(out, "%s  %s ready\n", emo("✅", "[ok]"), model)
			}
		}
	}
}
