package mempressure

import (
	"errors"
	"runtime"
	"testing"
)

// TestLevelFrom pins the decision for all three platforms.
//
// The numbers in the linux, darwin and windows cases are not invented: each
// is a reading taken on real hardware on 2026-09-20 and recorded in
// docs/knowledges/20260920/1900-os-memory-pressure-signals-3-os.md. They are
// a record of what those OSes report, not a product contract — but the two
// cases marked below ARE contracts, because they are the whole reason this
// package is shaped the way it is (waired-agent#1453).
func TestLevelFrom(t *testing.T) {
	boom := errors.New("read failed")
	for _, tc := range []struct {
		name  string
		goos  string
		facts Facts
		want  Level
		isErr bool
	}{
		// CONTRACT (waired-agent#1453): a hard ceiling gives no warning.
		// Measured: PSI read exactly 0.00 from the first byte allocated to
		// the SIGKILL. This MUST be Normal — a guard that fires here would
		// be firing on nothing at all.
		{"linux: silent right up to the kill", "linux",
			Facts{LinuxSomeAvg10: 0, LinuxFullAvg10: 0}, LevelNormal, false},
		{"linux: 1.3s into a real thrash", "linux",
			Facts{LinuxSomeAvg10: 8.51, LinuxFullAvg10: 8.51}, LevelCritical, false},
		{"linux: the decay tail after a thrash", "linux",
			Facts{LinuxSomeAvg10: 0.62, LinuxFullAvg10: 0.62}, LevelWarn, false},
		{"linux: some pressure but nothing fully stalled", "linux",
			Facts{LinuxSomeAvg10: 4.0, LinuxFullAvg10: 0}, LevelWarn, false},
		{"linux: unreadable", "linux", Facts{LinuxErr: boom}, LevelUnknown, true},

		{"darwin: idle at 70% free", "darwin", Facts{DarwinLevel: 1}, LevelNormal, false},
		// CONTRACT (waired-agent#1453): macOS level 2 is NOT a stop. The
		// reference mac read 2 with 53% of its memory free.
		{"darwin: advisory at 53% free", "darwin", Facts{DarwinLevel: 2}, LevelWarn, false},
		{"darwin: thrashing at 31% free", "darwin", Facts{DarwinLevel: 4}, LevelCritical, false},
		{"darwin: a value Apple does not document", "darwin", Facts{DarwinLevel: 3}, LevelUnknown, true},
		{"darwin: unreadable", "darwin", Facts{DarwinErr: boom}, LevelUnknown, true},

		{"windows: the OS raised its own low-memory notification", "windows",
			Facts{WindowsLowSignaled: 1, WindowsTotalPhysMB: 32440, WindowsAvailPhysMB: 887},
			LevelCritical, false},
		// The measured flip point on a 31.7 GiB host was 887 MB available.
		// The floor there is the 1 GiB minimum (3% would be 973 MB), so it
		// reaches the same verdict a moment earlier — which is the point.
		{"windows: below the floor before the OS says so", "windows",
			Facts{WindowsLowSignaled: 0, WindowsTotalPhysMB: 32440, WindowsAvailPhysMB: 887},
			LevelCritical, false},
		{"windows: where the high-memory notification switched off", "windows",
			Facts{WindowsLowSignaled: 0, WindowsTotalPhysMB: 32440, WindowsAvailPhysMB: 1769},
			LevelWarn, false},
		// CONTRACT (waired-agent#1453): the worst moment of the reference
		// host's WORST failing load must read Normal. That load failed on
		// the device, not the host, and a guard that stopped it would be
		// stopping loads this product has to keep serving.
		{"windows: the reference host at its worst failing load", "windows",
			Facts{WindowsLowSignaled: 0, WindowsTotalPhysMB: 130199, WindowsAvailPhysMB: 28809},
			LevelNormal, false},
		{"windows: no numbers, notification readable and quiet", "windows",
			Facts{WindowsLowSignaled: 0}, LevelNormal, false},
		{"windows: no numbers and no notification", "windows",
			Facts{WindowsLowSignaled: -1, WindowsErr: boom}, LevelUnknown, true},

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

// TestWindowsFloorMB records the arithmetic on the three host sizes that
// actually appear in this project's issues.
func TestWindowsFloorMB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		totalMB uint64
		want    uint64
	}{
		// A small host is held to the gigabyte minimum: 3% of 8 GB is
		// 245 MB, which is past saving.
		{"8 GB laptop", 8192, 1024},
		// The minimum still wins at this size: 3% is 973 MB. The share only
		// takes over above ~34 GB of RAM.
		{"the 31.7 GiB windows host these were measured on", 32440, 1024},
		{"just above where the share overtakes the minimum", 35000, 1050},
		{"the 127 GiB reference host", 130199, 3905},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsFloorMB(tc.totalMB); got != tc.want {
				t.Errorf("windowsFloorMB(%d) = %d, want %d", tc.totalMB, got, tc.want)
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

// TestSamplerReadsThisHost exercises the REAL per-OS reader, not the seam.
// Without this the platform half of the package is never run by any test,
// and a broken reader would look exactly like a healthy one.
//
// It asserts only that the host answers coherently: a level with no error,
// or LevelUnknown with one. A CI runner's actual pressure is not ours to
// pin — the point is that the syscall, the file read or the sysctl works.
func TestSamplerReadsThisHost(t *testing.T) {
	s := New()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	lvl, err := s.Level()
	switch {
	case err != nil && lvl != LevelUnknown:
		t.Errorf("on %s: error %v came back with level %v, want LevelUnknown", runtime.GOOS, err, lvl)
	case err == nil && lvl == LevelUnknown:
		t.Errorf("on %s: LevelUnknown with no error to explain it", runtime.GOOS)
	case err != nil:
		t.Logf("on %s this host could not be read: %v", runtime.GOOS, err)
	default:
		t.Logf("on %s this host reads %v", runtime.GOOS, lvl)
	}
}

// TestSamplerCloseIsSafeTwice covers the Windows handle, which is the only
// platform holding one.
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
