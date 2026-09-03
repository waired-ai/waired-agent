package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// The available-memory measurement (#568): how much of system RAM this
// computer has been seen to have free with nothing loaded. hostfit's OS
// deduction is max(OSMemoryAllowanceGB, RAMTotalGB − this), so the
// figure only ever tightens a verdict, and 0 always means
// "unavailable → the constant".
//
// WHEN IT IS TAKEN: at every daemon start, BEFORE the engine bootstrap,
// and only while nothing is serving — so a resident model is never
// charged against the very host that serves it.
//
// WHAT IS KEPT is the HIGHEST reading, never the latest
// (docs/decisions/20260819/1830-remeasure-each-boot-keep-the-highest.md,
// which revises the once-per-install rule of 20260809/0016). Two
// properties come out of that, and they are the whole design:
//
//   - The published figure moves in ONE direction, up. A verdict can go
//     from "does not fit" to "fits" and never back, so the resample
//     churn 0016 refused — a busy afternoon flipping a fit verdict, a
//     served NetworkMap field moving under consumers — cannot happen.
//   - What it converges on is what this machine offers with the user's
//     work put away, rather than whatever happened to be resident the
//     one minute the installer ran. Measured on a 48 GB M5 Pro
//     (waired-ai/waired-agent#835): 23 GB on file from install time
//     against 33 GB live, and every fit decision was made on the 23.
//
// An operator who needs the figure to come DOWN — a machine that
// genuinely lost memory to a new background service — has
// `waired inference memory remeasure`, which replaces the record
// outright rather than raising it. That is the only way down, and it is
// deliberate: everything automatic here is monotone.

// hostMemoryEnvVar is the operator/CI seam: a positive integer wins
// over both the record and the probe, and nothing is persisted while
// it is set. Same shape as WAIRED_INFERENCE_ENABLED /
// WAIRED_OLLAMA_KV_CACHE_TYPE. Values hostfit finds implausible
// (> RAMTotalGB) fall back to the constant there, so a nonsense value
// degrades to the pre-#568 arithmetic rather than a tighter gate.
const hostMemoryEnvVar = "WAIRED_RAM_AVAILABLE_GB"

// hostMemoryMeasurement is the figure every profiler construction reads
// and, since #699, the timestamp that dates it: the env seam first, else
// the persisted record. It never measures.
//
// The two are returned TOGETHER so they cannot drift. They travel as one
// fact — a value with a date that belongs to some other value would be
// worse than no date at all — and a caller that could reach for either
// separately is a caller that can pair the wrong ones.
//
// The env seam supplies a value and NO date, deliberately. It is an
// operator/CI override, not a measurement, so there is nothing to date;
// borrowing the record's timestamp would attribute the number to a
// measurement that did not produce it. Downstream that reads as "0 means
// unavailable, "" means no claim", which is what both fields already
// mean on the wire.
func hostMemoryMeasurement(stateDir string, getenv func(string) string) (int, string) {
	if v := getenv(hostMemoryEnvVar); v != "" {
		if gb, err := strconv.Atoi(v); err == nil && gb > 0 {
			return gb, ""
		}
	}
	rec, err := state.ReadHostMemory(stateDir)
	if err != nil {
		return 0, ""
	}
	return rec.AvailableGB, rec.MeasuredAt
}

// hostMemoryGB is hostMemoryMeasurement's value half, for the callers
// that only decide with it (hostfit takes the number, not the date).
func hostMemoryGB(stateDir string, getenv func(string) string) int {
	gb, _ := hostMemoryMeasurement(stateDir, getenv)
	return gb
}

// ensureHostMemoryMeasured takes the measurement at daemon start and
// persists it when it beats the recorded one. Returns what hostMemoryGB
// will read afterwards, for the boot log.
//
//   - env seam set → nothing measured, nothing persisted.
//   - something already listening on the engine port → the previous
//     record is kept (measuring now would charge the resident engine);
//     the re-measure waits for the next clean boot.
//   - probe failed → 0 (the constant), record untouched.
//   - probe read no higher than the record → the record stands,
//     untouched. Keeping its own MeasuredAt matters: re-dating a figure
//     to a measurement that did not produce it is the same lie the env
//     seam refuses to tell by returning no date at all.
//   - probe read higher → persisted with this build's version and the
//     measurement time.
func ensureHostMemoryMeasured(
	stateDir, version string,
	getenv func(string) string,
	ramFn func(context.Context) (totalGB, availGB int, err error),
	engineBusy func() bool,
	now func() time.Time,
) (int, error) {
	if v := getenv(hostMemoryEnvVar); v != "" {
		return hostMemoryGB(stateDir, getenv), nil
	}
	rec, err := state.ReadHostMemory(stateDir)
	if err != nil {
		return 0, fmt.Errorf("read host memory record: %w", err)
	}
	if engineBusy() {
		return rec.AvailableGB, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	availGB, err := takeHostMemoryMeasurement(ctx, ramFn)
	if err != nil {
		return 0, err
	}
	if availGB <= rec.AvailableGB {
		return rec.AvailableGB, nil
	}
	return availGB, persistHostMemory(stateDir, version, availGB, now)
}

// takeHostMemoryMeasurement runs the probe and applies the one rule both
// callers share: the floor at 1, where 0 on the wire means "unavailable"
// and a truthfully exhausted host is the one host that must not read as
// unmeasured.
//
// Split out so the boot path and the supported re-measure path (#589)
// take the SAME measurement rather than two implementations of it. What
// they do with the answer differs and stays with each of them: boot
// keeps the higher of it and the record, a re-measure replaces the
// record outright — that is what a re-measure is for.
func takeHostMemoryMeasurement(
	ctx context.Context,
	ramFn func(context.Context) (totalGB, availGB int, err error),
) (int, error) {
	_, availGB, err := ramFn(ctx)
	if err != nil {
		return 0, fmt.Errorf("measure available memory: %w", err)
	}
	return max(availGB, 1), nil
}

// persistHostMemory writes the record for a reading that is taking
// effect. Only called where the figure actually changes, so a boot that
// measures no better than last time leaves the file — and its
// MeasuredAt — alone.
func persistHostMemory(stateDir, version string, availGB int, now func() time.Time) error {
	rec := state.HostMemoryRecord{
		AvailableGB:  availGB,
		MeasuredAt:   now().UTC().Format(time.RFC3339),
		AgentVersion: version,
	}
	if err := state.WriteHostMemory(stateDir, rec); err != nil {
		return fmt.Errorf("persist host memory record: %w", err)
	}
	return nil
}

// measureAndPersistHostMemory is the re-measure path: take the reading
// and make it the record, up or down.
func measureAndPersistHostMemory(
	ctx context.Context,
	stateDir, version string,
	ramFn func(context.Context) (totalGB, availGB int, err error),
	now func() time.Time,
) (int, error) {
	availGB, err := takeHostMemoryMeasurement(ctx, ramFn)
	if err != nil {
		return 0, err
	}
	return availGB, persistHostMemory(stateDir, version, availGB, now)
}

// hostMemoryRemeasurer is the supported way to replace the
// available-memory figure (waired-agent#589), and since #835 it is the
// only way it can go DOWN: it takes the reading and makes it the record
// whether that is larger or smaller, where boot only ever raises it.
//
// Until this existed, an operator whose host was measured during a busy
// moment had exactly one option — delete runtime/host-memory.json and
// restart the daemon — which was folklore rather than a path anyone
// could be pointed at.
//
// It reuses the boot-time engine guard deliberately: the figure means
// "what this computer has free with nothing loaded", so taking it beside
// a resident engine would measure the very thing the rule exists to
// exclude. That refusal is reported rather than silently degraded,
// because "I measured" and "I kept the old number" are different answers
// to the operator's question.
type hostMemoryRemeasurer struct {
	stateDir   string
	version    string
	getenv     func(string) string
	ramFn      func(context.Context) (totalGB, availGB int, err error)
	engineBusy func() bool
	now        func() time.Time
}

func (h hostMemoryRemeasurer) RemeasureHostMemory(ctx context.Context) management.HostMemoryRemeasure {
	gb, at := hostMemoryMeasurement(h.stateDir, h.getenv)
	kept := management.HostMemoryRemeasure{AvailableGB: gb, MeasuredAt: at}

	if v := h.getenv(hostMemoryEnvVar); v != "" {
		kept.Reason = hostMemoryEnvVar + " is set, so the measurement is overridden and nothing was taken"
		return kept
	}
	if h.engineBusy() {
		kept.Reason = "an inference engine is holding memory right now; " +
			"measuring would count it against this host. Stop it with `waired inference engine stop` and try again"
		return kept
	}
	availGB, err := measureAndPersistHostMemory(ctx, h.stateDir, h.version, h.ramFn, h.now)
	if err != nil {
		kept.Reason = err.Error()
		return kept
	}
	_, measuredAt := hostMemoryMeasurement(h.stateDir, h.getenv)
	return management.HostMemoryRemeasure{
		Measured:    true,
		AvailableGB: availGB,
		MeasuredAt:  measuredAt,
	}
}

// engineListening reports whether anything answers on either engine's
// port — waired's own engine is not running this early in boot, so a
// listener is an operator-run engine the daemon does not manage, and a
// measurement taken beside it would charge its resident model.
//
// BOTH ports, and deliberately not "the engine this host serves with"
// (waired-agent#1206). The question is about an engine waired does NOT
// manage, so an answer derived from waired's own choice is the wrong
// question asked confidently: it used to ask probeTargetForActive, which
// on a host with no Active selection said ollama, so a foreign vLLM on
// 9479 was never noticed. Neither is a provider available to ask — the
// first caller runs after the flags and before anything that could start
// an engine, where the logger does not exist yet.
//
// The two resolvers are pure config methods, so this needs no store and
// no state file.
func engineListening(cfg agentconfig.InferenceConfig) func() bool {
	return func() bool {
		for _, port := range []int{cfg.ResolvedOllamaPort(), cfg.ResolvedVLLMPort()} {
			if port <= 0 {
				continue
			}
			conn, err := net.DialTimeout("tcp",
				net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
			if err != nil {
				continue
			}
			_ = conn.Close()
			return true
		}
		return false
	}
}
