package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
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
	_, availGB, err := ramFn(ctx)
	if err != nil {
		return 0, fmt.Errorf("measure available memory: %w", err)
	}
	availGB = max(availGB, 1)
	rec = state.HostMemoryRecord{
		AvailableGB:  availGB,
		MeasuredAt:   now().UTC().Format(time.RFC3339),
		AgentVersion: version,
	}
	if err := state.WriteHostMemory(stateDir, rec); err != nil {
		return availGB, fmt.Errorf("persist host memory record: %w", err)
	}
	return availGB, nil
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
