package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// countingProbe is a hardware.WithEngineVersion-shaped resolver that
// records how many times it was executed. The real one shells out to
// `<engine> --version` with a 5 s timeout, so "how often is this called"
// is the property worth pinning.
type countingProbe struct {
	calls   int
	engines []string
	ok      bool
	version string
}

func (c *countingProbe) fn(_ context.Context, engine string) (bool, string) {
	c.calls++
	c.engines = append(c.engines, engine)
	return c.ok, c.version
}

func versionProbeProvider(t *testing.T, probe *countingProbe) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		engineVersionProbe: probe.fn,
	}
}

// PRODUCT CONTRACT (#361): the version used against MinEngineVersion
// floors is MEASURED from the installed binary when nothing else knows
// it yet — an unknown version excludes every floored variant, so a
// fresh install silently pulled the lower one.
//
// The measurement does not need a running server: engineVersionOnHost
// (#238) resolves the state-dir binary and runs `--version`. What was
// missing is that ollamaEngineVersion only ever read that through the
// profiler's 30 s snapshot, which on a fresh install was taken BEFORE
// the engine existed.
func TestOllamaEngineVersion_MeasuresTheBinaryWhenNothingElseKnows(t *testing.T) {
	probe := &countingProbe{ok: true, version: "0.31.1"}
	p := versionProbeProvider(t, probe)

	if got := p.ollamaEngineVersion(context.Background()); got != "0.31.1" {
		t.Fatalf("ollamaEngineVersion = %q, want 0.31.1", got)
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.calls)
	}
	if len(probe.engines) != 1 || probe.engines[0] != "ollama" {
		t.Fatalf("probed engines = %v, want [ollama]", probe.engines)
	}
}

// The probe is an exec. Status() derives AvailableUpdate on every poll
// and the tray polls continuously, so an un-memoized probe would shell
// out per request on exactly the hosts that cannot answer quickly.
func TestOllamaEngineVersion_MemoizesTheMeasurement(t *testing.T) {
	probe := &countingProbe{ok: true, version: "0.31.1"}
	p := versionProbeProvider(t, probe)
	ctx := context.Background()

	for i := range 5 {
		if got := p.ollamaEngineVersion(ctx); got != "0.31.1" {
			t.Fatalf("call %d = %q, want 0.31.1", i, got)
		}
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (memoized)", probe.calls)
	}

	// Expiry re-measures, so an engine upgraded in place is picked up
	// without a restart — the same reason ollamaUsable is a live
	// resolver rather than a boot snapshot (#188).
	p.engineVerMu.Lock()
	p.engineVerAt = p.engineVerAt.Add(-engineVersionMemoTTL - time.Second)
	p.engineVerMu.Unlock()
	probe.version = "0.32.0"

	if got := p.ollamaEngineVersion(ctx); got != "0.32.0" {
		t.Fatalf("after TTL expiry = %q, want 0.32.0", got)
	}
	if probe.calls != 2 {
		t.Fatalf("probe calls after expiry = %d, want 2", probe.calls)
	}
}

// A host whose engine cannot report a version must not pay one exec per
// caller for the privilege of learning that again. The floors still fail
// closed on the empty answer — this pins only the cost.
func TestOllamaEngineVersion_MemoizesANegativeMeasurement(t *testing.T) {
	probe := &countingProbe{ok: false}
	p := versionProbeProvider(t, probe)
	ctx := context.Background()

	for i := range 3 {
		if got := p.ollamaEngineVersion(ctx); got != "" {
			t.Fatalf("call %d = %q, want \"\" (unknown)", i, got)
		}
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (negative memoized)", probe.calls)
	}
}

// Precedence, cheapest-and-most-authoritative first: the running
// server's own /api/version answers for what will actually load the
// weights, so a live engine spends no exec at all.
func TestOllamaEngineVersion_ProfileWinsOverTheProbe(t *testing.T) {
	probe := &countingProbe{ok: true, version: "0.31.1"}
	p := versionProbeProvider(t, probe)
	p.profiler = hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, engine string) (bool, string) {
			return engine == "ollama", "0.30.5"
		}),
	)

	if got := p.ollamaEngineVersion(context.Background()); got != "0.30.5" {
		t.Fatalf("ollamaEngineVersion = %q, want the profile's 0.30.5", got)
	}
	if probe.calls != 0 {
		t.Fatalf("probe calls = %d, want 0 — the profile already knew", probe.calls)
	}
}

// Records today's behaviour: a provider with no probe wired (every unit
// fixture, and any build that never resolved an engine) degrades to
// exactly the pre-#361 answer rather than panicking.
func TestOllamaEngineVersion_NoProbeWiredStaysUnknown(t *testing.T) {
	p := &agentInferenceProvider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := p.ollamaEngineVersion(context.Background()); got != "" {
		t.Fatalf("ollamaEngineVersion with no probe = %q, want \"\"", got)
	}
}
