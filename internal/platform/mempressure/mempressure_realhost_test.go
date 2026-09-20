//go:build presshw

// Real-host checks for the memory-pressure reader. Never compiled by CI: it
// reads whatever the machine it runs on happens to be doing, and the squeeze
// half deliberately takes memory away from it.
//
// This is the shape internal/runtime/process_tree_realengine_test.go uses for
// the same reason — some claims can only be checked against a real machine,
// and a test that cannot run on a CI runner must not be able to fail there.
//
// Run the harmless half on any host:
//
//	go test -tags presshw -run TestRealHost_IdleIsNeverCritical \
//	    -timeout 20m ./internal/platform/mempressure/
//
// Run the squeeze, which will make the host slow and may make it
// unresponsive for a while:
//
//	WAIRED_PRESSHW_SQUEEZE=1 go test -tags presshw \
//	    -run TestRealHost_SqueezeReachesCritical \
//	    -timeout 20m ./internal/platform/mempressure/
package mempressure

import (
	"encoding/binary"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", name, v, err)
	}
	return d
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", name, v, err)
	}
	return n
}

// TestRealHost_IdleIsNeverCritical is the misfire check.
//
// It watches a host going about its business and fails if the reader ever
// calls that critical. This is the property the whole design is built around
// (waired-agent#1453, owner direction 2026-09-20): stopping a load that was
// going to succeed is worse than stopping one late, so a machine with memory
// to spare must never read Critical however much it swaps.
//
// Warn is allowed and is not a misfire: nothing stops a load on Warn.
func TestRealHost_IdleIsNeverCritical(t *testing.T) {
	watch := envDuration(t, "WAIRED_PRESSHW_IDLE", 2*time.Minute)
	s := New()
	defer func() { _ = s.Close() }()

	var (
		samples            int
		worstRate          float64
		lowestAvail        uint64 = 1<<64 - 1
		counts                    = map[Level]int{}
		firstCriticalFacts Facts
		sawCritical        bool
	)
	deadline := time.Now().Add(watch)
	for time.Now().Before(deadline) {
		f, err := s.Sample()
		if err != nil {
			t.Fatalf("Sample: %v", err)
		}
		lvl, err := levelFrom(runtime.GOOS, f)
		if err != nil {
			t.Fatalf("levelFrom: %v (facts %+v)", err, f)
		}
		samples++
		counts[lvl]++
		if f.SwapOutMBPerSec > worstRate {
			worstRate = f.SwapOutMBPerSec
		}
		if f.AvailMB > 0 && f.AvailMB < lowestAvail {
			lowestAvail = f.AvailMB
		}
		if lvl == LevelCritical && !sawCritical {
			sawCritical, firstCriticalFacts = true, f
		}
		time.Sleep(time.Second)
	}

	t.Logf("%s: %d samples over %s — normal %d, warn %d, critical %d",
		runtime.GOOS, samples, watch, counts[LevelNormal], counts[LevelWarn], counts[LevelCritical])
	t.Logf("  lowest available %d MB (floor %d MB), highest swap-out %.2f MB/s (surge at %d)",
		lowestAvail, availFloorMB(mustTotal(t, s)), worstRate, swapSurgeMBPerSec)
	if sawCritical {
		t.Errorf("a host at rest read Critical, which would have stopped a model load: %+v",
			firstCriticalFacts)
	}
}

func mustTotal(t *testing.T, s *Sampler) uint64 {
	t.Helper()
	f, err := s.Sample()
	if err != nil {
		return 0
	}
	return f.TotalMB
}

// TestRealHost_SqueezeReachesCritical is the other half: the reader has to
// notice a host that really is running out.
//
// Opt-in through WAIRED_PRESSHW_SQUEEZE because it takes the host's memory
// away on purpose. It stops the moment it reads Critical and frees
// everything, so it does not push further than the thing it is measuring.
func TestRealHost_SqueezeReachesCritical(t *testing.T) {
	if os.Getenv("WAIRED_PRESSHW_SQUEEZE") == "" {
		t.Skip("set WAIRED_PRESSHW_SQUEEZE=1 — this test deliberately exhausts host memory")
	}
	stepMB := envInt(t, "WAIRED_PRESSHW_STEP_MB", 256)
	maxMB := envInt(t, "WAIRED_PRESSHW_MAX_MB", 200_000)
	budget := envDuration(t, "WAIRED_PRESSHW_BUDGET", 10*time.Minute)

	s := New()
	defer func() { _ = s.Close() }()
	if _, err := s.Sample(); err != nil { // prime the rate
		t.Fatalf("Sample: %v", err)
	}

	var blocks [][]byte
	defer func() {
		blocks = nil
		runtime.GC()
	}()

	deadline := time.Now().Add(budget)
	allocated := 0
	for allocated < maxMB && time.Now().Before(deadline) {
		blocks = append(blocks, incompressible(stepMB))
		allocated += stepMB
		time.Sleep(500 * time.Millisecond)

		f, err := s.Sample()
		if err != nil {
			t.Fatalf("Sample: %v", err)
		}
		lvl, err := levelFrom(runtime.GOOS, f)
		if err != nil {
			t.Fatalf("levelFrom: %v", err)
		}
		if lvl >= LevelWarn {
			t.Logf("at %d MB: %v (avail %d/%d MB, swap out %.1f MB/s)",
				allocated, lvl, f.AvailMB, f.TotalMB, f.SwapOutMBPerSec)
		}
		if lvl == LevelCritical {
			t.Logf("%s: Critical at %d MB allocated — avail %d/%d MB (floor %d), "+
				"swap out %.1f MB/s", runtime.GOOS, allocated, f.AvailMB, f.TotalMB,
				availFloorMB(f.TotalMB), f.SwapOutMBPerSec)
			return
		}
	}
	t.Errorf("%s: allocated %d MB without ever reading Critical; a host this far gone "+
		"must be noticed", runtime.GOOS, allocated)
}

// incompressible fills a block with bytes the OS cannot squash.
//
// macOS compresses anonymous memory before it swaps, so a sparse or
// repetitive fill measures nothing at all there: a first attempt wrote
// 20 GiB on a 16 GiB host in 2.3 seconds without moving the pressure level.
// A cheap xorshift defeats the compressor and is far faster than
// crypto/rand.
func incompressible(mb int) []byte {
	b := make([]byte, mb<<20)
	x := uint64(0x9E3779B97F4A7C15) ^ uint64(len(b))
	for i := 0; i+8 <= len(b); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(b[i:], x)
	}
	return b
}
