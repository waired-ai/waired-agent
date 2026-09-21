package mempressure

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

// The facts in these tests are readings taken on real hosts on 2026-09-20
// and recorded in
// docs/knowledges/20260920/1900-os-memory-pressure-signals-3-os.md. Cases
// marked CONTRACT are product contracts of waired-agent#1453 (owner
// direction, same date: stop only when it is really dangerous). The rest are
// records of what those OSes report.
func TestLevelFrom(t *testing.T) {
	boom := errors.New("read failed")

	// The three hosts, as they actually read.
	const (
		linuxTotal = 124000 // linux, idle
		linuxAvail = 113548
		macTotal   = 16384 // macOS, idle, 3.28 GB already in swap
		macAvail   = 11468 // = total * kern.memorystatus_level(70) / 100
		winTotal   = 32440 // windows, 84% used - the busiest host measured
		winAvail   = 5072
		refTotal   = 130199 // the #1443 reference host
		refAvail   = 28809  // its worst moment across seven large loads
	)

	for _, tc := range []struct {
		name  string
		goos  string
		facts Facts
		want  Level
		isErr bool
	}{
		// ---- nothing wrong ----
		{"linux: idle", "linux",
			Facts{AvailMB: linuxAvail, TotalMB: linuxTotal, SwapOutMBPerSec: 0}, LevelNormal, false},
		// CONTRACT: the reference host's WORST failing load must read Normal.
		// It failed on the device with 28.8 GB of host memory to spare, and a
		// guard that stopped it would stop loads this product has to serve.
		{"CONTRACT: windows, the reference host at its worst failing load", "windows",
			Facts{AvailMB: refAvail, TotalMB: refTotal, WindowsLowSignaled: 0, SwapOutMBPerSec: 0},
			LevelNormal, false},
		// CONTRACT: an idle mac with gigabytes ALREADY in swap. This is why
		// the surge term is a rate and not a level - that host sat at
		// 3,282 MB of swap in use and swapped exactly nothing for five
		// minutes.
		{"CONTRACT: darwin, idle with 3.28 GB sitting in swap", "darwin",
			Facts{AvailMB: macAvail, TotalMB: macTotal, DarwinLevel: 1,
				SwapOutTotalMB: 3282.8, SwapOutMBPerSec: 0},
			LevelNormal, false},
		// CONTRACT, and the case that isolates the rate from the level: a
		// host that IS short, and that swapped heavily at some point, but is
		// not swapping now. Only the rate may decide. A trigger on swap in
		// use would stop a load here on the strength of history.
		{"CONTRACT: darwin, short, swap written long ago, quiet now", "darwin",
			Facts{AvailMB: 900, TotalMB: macTotal, DarwinLevel: 2,
				SwapOutTotalMB: 3282.8, SwapOutMBPerSec: 0},
			LevelWarn, false},
		// Measured on 2026-09-20: a 16 GiB mac mini 40 GB into a squeeze,
		// swapping at 300-500 MB/s, still reporting 36% available. The
		// availability figure cannot gate anything on macOS, so the gate
		// there is the system's own WARN.
		{"darwin: 40 GB allocated on a 16 GiB host, swapping hard", "darwin",
			Facts{AvailMB: 5898, TotalMB: macTotal, DarwinLevel: 2, SwapOutMBPerSec: 430},
			LevelCritical, false},
		// And the same rate at rest, where macOS says level 1, must not fire.
		{"CONTRACT: darwin, swapping hard but the system says it is fine", "darwin",
			Facts{AvailMB: macAvail, TotalMB: macTotal, DarwinLevel: 1, SwapOutMBPerSec: 430},
			LevelNormal, false},
		// CONTRACT: the busiest healthy host measured - 84% of memory in use,
		// and the highest page-out rate any healthy host reached.
		{"CONTRACT: windows, 84% used and paging a little", "windows",
			Facts{AvailMB: winAvail, TotalMB: winTotal, WindowsLowSignaled: 0, SwapOutMBPerSec: 1.15},
			LevelNormal, false},
		// CONTRACT: swap alone is never enough. Another program or the OS may
		// swap hard while there is plenty of memory, and that is its business.
		{"CONTRACT: linux, swapping hard with 113 GB free", "linux",
			Facts{AvailMB: linuxAvail, TotalMB: linuxTotal, SwapOutMBPerSec: 640},
			LevelNormal, false},

		// ---- short, but not yet dangerous ----
		{"linux: under the floor with no surge is a warning, not a stop", "linux",
			Facts{AvailMB: 900, TotalMB: linuxTotal, SwapOutMBPerSec: 0}, LevelWarn, false},
		{"windows: under the floor with a trickle is still a warning", "windows",
			Facts{AvailMB: 800, TotalMB: winTotal, WindowsLowSignaled: 0, SwapOutMBPerSec: 4},
			LevelWarn, false},
		{"linux: some pressure, nothing fully stalled", "linux",
			Facts{AvailMB: linuxAvail, TotalMB: linuxTotal, LinuxSomeAvg10: 4.0}, LevelWarn, false},
		{"darwin: the advisory level, reached at 53% free", "darwin",
			Facts{AvailMB: macAvail, TotalMB: macTotal, DarwinLevel: 2}, LevelWarn, false},

		// ---- dangerous ----
		{"nothing left AND swapping hard", "linux",
			Facts{AvailMB: 700, TotalMB: linuxTotal, SwapOutMBPerSec: 640}, LevelCritical, false},
		{"windows: nothing left AND swapping hard", "windows",
			Facts{AvailMB: 500, TotalMB: winTotal, WindowsLowSignaled: 0, SwapOutMBPerSec: 64},
			LevelCritical, false},
		// The backstops: each OS's own word for "I am in trouble", which is
		// enough on its own and is what catches a thrash too slow to surge.
		{"linux: everything stalled on memory", "linux",
			Facts{AvailMB: linuxAvail, TotalMB: linuxTotal, LinuxSomeAvg10: 8.51, LinuxFullAvg10: 8.51},
			LevelCritical, false},
		{"darwin: the critical level", "darwin",
			Facts{AvailMB: macAvail, TotalMB: macTotal, DarwinLevel: 4}, LevelCritical, false},
		{"windows: the OS raised its low-memory notification", "windows",
			Facts{AvailMB: 887, TotalMB: winTotal, WindowsLowSignaled: 1}, LevelCritical, false},

		// ---- could not tell ----
		{"linux: unreadable", "linux", Facts{LinuxErr: boom}, LevelUnknown, true},
		{"darwin: unreadable", "darwin", Facts{DarwinErr: boom}, LevelUnknown, true},
		{"darwin: a level Apple does not document", "darwin", Facts{DarwinLevel: 3}, LevelUnknown, true},
		{"windows: neither the notification nor the numbers", "windows",
			Facts{WindowsLowSignaled: -1, WindowsErr: boom}, LevelUnknown, true},
		{"windows: no notification but numbers to go on", "windows",
			Facts{WindowsLowSignaled: -1, AvailMB: winAvail, TotalMB: winTotal}, LevelNormal, false},
		{"an OS with no reader", "plan9", Facts{}, LevelUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := levelFrom(tc.goos, tc.facts)
			if got != tc.want {
				t.Errorf("levelFrom(%q, %+v) = %v, want %v", tc.goos, tc.facts, got, tc.want)
			}
			if (err != nil) != tc.isErr {
				t.Errorf("levelFrom(%q, ...) err = %v, want error: %v", tc.goos, err, tc.isErr)
			}
		})
	}
}

// TestAvailFloorMB records the arithmetic on the host sizes that appear in
// this project's issues.
func TestAvailFloorMB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		totalMB uint64
		want    uint64
	}{
		// 3% of 8 GB is 245 MB, which is past saving, so the minimum wins.
		{"8 GB laptop", 8192, 1024},
		{"the 16 GiB mac mini", 16384, 1024},
		{"the 31.7 GiB windows host", 32440, 1024},
		{"just above where the share overtakes the minimum", 35000, 1050},
		{"the 127 GiB reference host", 130199, 3905},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := availFloorMB(tc.totalMB); got != tc.want {
				t.Errorf("availFloorMB(%d) = %d, want %d", tc.totalMB, got, tc.want)
			}
		})
	}
}

// TestRateFrom covers the three answers that are not arithmetic.
func TestRateFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		prev float64
		cur  float64
		dt   time.Duration
		want float64
	}{
		{"the first sample has nothing to difference", -1, 100, time.Second, -1},
		{"a platform that cannot count", 100, -1, time.Second, -1},
		{"no interval to divide by", 100, 200, 0, -1},
		{"steady", 100, 100, time.Second, 0},
		// macOS reports swap IN USE, which falls when swap is released.
		// Releasing swap is not swapping backwards.
		{"the counter fell", 3282, 2000, time.Second, 0},
		{"64 MB over two seconds", 100, 164, 2 * time.Second, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateFrom(tc.prev, tc.cur, tc.dt); got != tc.want {
				t.Errorf("rateFrom(%v, %v, %v) = %v, want %v", tc.prev, tc.cur, tc.dt, got, tc.want)
			}
		})
	}
}

func TestLevelString(t *testing.T) {
	for lvl, want := range map[Level]string{
		LevelUnknown: "unknown", LevelNormal: "normal",
		LevelWarn: "warn", LevelCritical: "critical",
		Level(99): "unknown",
	} {
		if got := lvl.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", int(lvl), got, want)
		}
	}
}

// TestSamplerReadsThisHost exercises the REAL per-OS reader. Without it the
// platform half of the package is never run by any test, and a broken reader
// would look exactly like a healthy one.
//
// It asserts coherence rather than a particular reading: a CI runner's own
// pressure is not ours to pin. What it does pin is that the first sample
// reports no rate and the second one does — the differencing is the part a
// platform change is most likely to break silently.
func TestSamplerReadsThisHost(t *testing.T) {
	s := New()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	first, err := s.Sample()
	if err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	if first.TotalMB == 0 {
		t.Errorf("on %s: total memory read as 0", runtime.GOOS)
	}
	if first.SwapOutTotalMB >= 0 && first.SwapOutMBPerSec != -1 {
		t.Errorf("on %s: first sample reported a rate of %v; it has nothing to difference against",
			runtime.GOOS, first.SwapOutMBPerSec)
	}

	second, err := s.Sample()
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if second.SwapOutTotalMB >= 0 && second.SwapOutMBPerSec < 0 {
		t.Errorf("on %s: second sample still has no swap rate (total %v)",
			runtime.GOOS, second.SwapOutTotalMB)
	}
	t.Logf("on %s: avail %d/%d MB, swap out %.2f MB/s",
		runtime.GOOS, second.AvailMB, second.TotalMB, second.SwapOutMBPerSec)

	lvl, err := s.Level()
	switch {
	case err != nil && lvl != LevelUnknown:
		t.Errorf("on %s: error %v came back with level %v, want LevelUnknown", runtime.GOOS, err, lvl)
	case err == nil && lvl == LevelUnknown:
		t.Errorf("on %s: LevelUnknown with no error to explain it", runtime.GOOS)
	default:
		t.Logf("on %s this host reads %v", runtime.GOOS, lvl)
	}
}

// TestSamplerCloseIsSafeTwice covers the Windows handle, the only one held.
func TestSamplerCloseIsSafeTwice(t *testing.T) {
	s := New()
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	var nilSampler *Sampler
	if err := nilSampler.Close(); err != nil {
		t.Errorf("Close on a nil Sampler: %v", err)
	}
	if lvl, err := nilSampler.Level(); err == nil || lvl != LevelUnknown {
		t.Errorf("Level on a nil Sampler = %v, %v; want LevelUnknown and an error", lvl, err)
	}
}
