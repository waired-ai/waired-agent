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

// The install-time available-memory measurement (#568): what the
// operating system and everything resident at install time keep of
// system RAM. hostfit's OS deduction is
// max(OSMemoryAllowanceGB, RAMTotalGB − this), so the figure only ever
// tightens a verdict, and 0 always means "unavailable → the constant".
//
// WHEN IT IS TAKEN, and why that is the whole design: once per
// install/upgrade (state.HostMemoryRecord.AgentVersion keys the reuse,
// the host-speed rule), at daemon start BEFORE the engine bootstrap —
// so a resident model is never charged against the very host that
// serves it, and a busy afternoon cannot flip a fit verdict on the
// next resample. The published figure is fixed for the life of the
// install, which is the no-map-churn claim every other
// HardwareSummary field already makes.

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

// ensureHostMemoryMeasured takes the measurement when the persisted
// record is stale (different or absent AgentVersion) and persists it.
// Returns what hostMemoryGB will read afterwards, for the boot log.
//
//   - env seam set → nothing measured, nothing persisted.
//   - record fresh → reused as-is.
//   - something already listening on the engine port → the previous
//     record is kept (measuring now would charge the resident engine);
//     the re-measure waits for the next clean boot.
//   - probe failed → 0 (the constant), record untouched.
//   - probe succeeded → floored at 1 (0 on the wire means
//     "unavailable", and a truthfully exhausted host is the one host
//     that must not read as unmeasured), persisted with this build's
//     version and the measurement time.
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
	if rec.AgentVersion == version && rec.AvailableGB > 0 {
		return rec.AvailableGB, nil
	}
	if engineBusy() {
		return rec.AvailableGB, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return measureAndPersistHostMemory(ctx, stateDir, version, ramFn, now)
}

// measureAndPersistHostMemory takes the probe and writes the record.
//
// Split out of ensureHostMemoryMeasured so the supported re-measure path
// (#589) takes the SAME measurement under the SAME rules rather than a
// second implementation of them — in particular the floor at 1, where 0
// on the wire means "unavailable" and a truthfully exhausted host is the
// one host that must not read as unmeasured.
//
// The guards are deliberately NOT here. They differ between the two
// callers: boot skips a record that is already current for this build,
// and a re-measure exists precisely to overwrite one.
func measureAndPersistHostMemory(
	ctx context.Context,
	stateDir, version string,
	ramFn func(context.Context) (totalGB, availGB int, err error),
	now func() time.Time,
) (int, error) {
	_, availGB, err := ramFn(ctx)
	if err != nil {
		return 0, fmt.Errorf("measure available memory: %w", err)
	}
	availGB = max(availGB, 1)
	rec := state.HostMemoryRecord{
		AvailableGB:  availGB,
		MeasuredAt:   now().UTC().Format(time.RFC3339),
		AgentVersion: version,
	}
	if err := state.WriteHostMemory(stateDir, rec); err != nil {
		return availGB, fmt.Errorf("persist host memory record: %w", err)
	}
	return availGB, nil
}

// hostMemoryRemeasurer is the supported way to retake the install-time
// available-memory figure (waired-agent#589).
//
// Until this existed, an operator whose host was measured during a busy
// moment had exactly one option — delete runtime/host-memory.json and
// restart the daemon — which was folklore rather than a path anyone
// could be pointed at.
//
// It reuses the boot-time guards deliberately: the figure means "what
// the OS and everything resident at install time keep of system RAM", so
// taking it beside a resident engine would measure the very thing the
// install-time rule exists to exclude. That refusal is reported rather
// than silently degraded, because "I measured" and "I kept the old
// number" are different answers to the operator's question.
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

// engineListening reports whether anything answers on the active
// engine's port — waired's own engine is not running this early in
// boot, so a listener is an operator-run engine the daemon does not
// manage, and a measurement taken beside it would charge its resident
// model.
func engineListening(cfg agentconfig.InferenceConfig) func() bool {
	return func() bool {
		_, port := probeTargetForActive(cfg)
		if port <= 0 {
			return false
		}
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
}
