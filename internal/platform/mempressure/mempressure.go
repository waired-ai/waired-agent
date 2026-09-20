// Package mempressure reports whether the operating system itself says the
// host is running out of memory.
//
// It exists because a free-memory number cannot answer that question. On the
// reference host of waired-agent#1443 the loads that failed and the loads
// that succeeded are not separated by any amount of free memory: available
// memory bottomed at 33.2 and 34.1 GB on two successes and at 28.8 and
// 31.6 GB on three failures, and the host was never short. What ran out was
// the device, and the runner died at the moment the memory had to become
// resident. So this package is not how #1443 is detected — that is the
// runner's death, classified elsewhere — it is how the OTHER failure is
// detected: the one in waired-agent#837, where the weights transit the
// OS-visible half of a carve-out host, free memory pins near zero, and the
// whole machine starves for ten minutes with nothing to show for it.
//
// What each OS offers, measured on real hardware on 2026-09-20 and recorded
// in docs/knowledges/20260920/1900-os-memory-pressure-signals-3-os.md:
//
//   - linux: /proc/pressure/memory. Against a hard ceiling it is useless —
//     it read exactly 0.00 all the way to the SIGKILL — but once reclaim
//     starts it moves at once: 0.00 to 8.51 within about 1.3 seconds of the
//     first swap-out, while a gigabyte of swap was still free.
//   - darwin: kern.memorystatus_vm_pressure_level. 4 is the only value that
//     means trouble. The same idle host read 1 at 70% free and 2 at 53%
//     free, so 2 is an early advisory and not a reason to stop anything.
//   - windows: CreateMemoryResourceNotification. It works, and it is late:
//     on a 31.7 GiB host it fired at 887 MB available. That is a small
//     absolute value rather than a share of RAM, so on a 128 GB host the
//     same signal lands long after the machine is unusable — hence the
//     second term in windowsFloorMB.
//
// The common shape is that every one of these signals only appears once the
// kernel is actually reclaiming. None of them predicts a ceiling.
package mempressure

import (
	"fmt"
	"runtime"
)

// Level is how bad the operating system says the memory situation is.
type Level int

const (
	// LevelUnknown means the reading failed. It is deliberately distinct
	// from LevelNormal: "we could not look" must never be acted on as
	// "everything is fine", and a caller that stops a load on Critical
	// must not stop one on a failed query either.
	LevelUnknown Level = iota
	// LevelNormal is the OS reporting no memory trouble.
	LevelNormal
	// LevelWarn is reclaim beginning, or the OS's own advisory level. It is
	// information, not a reason to act: on macOS an idle host sits here
	// with half its memory free.
	LevelWarn
	// LevelCritical is the OS saying it is out of memory. This is the only
	// level a caller may stop a load on.
	LevelCritical
)

func (l Level) String() string {
	switch l {
	case LevelNormal:
		return "normal"
	case LevelWarn:
		return "warn"
	case LevelCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Facts is what one sample could read about memory pressure. Each platform
// fills the fields it has and leaves the rest at their zero values, the
// same shape internal/runtime's processTreeFacts uses.
type Facts struct {
	// LinuxSomeAvg10 and LinuxFullAvg10 are the avg10 columns of
	// /proc/pressure/memory (or of this process's own cgroup, which has the
	// same format). LinuxErr is set when neither could be read.
	LinuxSomeAvg10 float64
	LinuxFullAvg10 float64
	LinuxErr       error

	// DarwinLevel is kern.memorystatus_vm_pressure_level: 1 normal, 2 warn,
	// 4 critical.
	DarwinLevel uint32
	DarwinErr   error

	// WindowsLowSignaled is the state of the LowMemoryResourceNotification
	// object: 1 signaled, 0 not signaled, -1 the query itself failed. The
	// three are kept apart because a failed query is not an answer.
	WindowsLowSignaled int
	// WindowsAvailPhysMB and WindowsTotalPhysMB come from
	// GlobalMemoryStatusEx. They back the share-of-RAM term the
	// notification alone does not give.
	WindowsAvailPhysMB uint64
	WindowsTotalPhysMB uint64
	WindowsErr         error
}

// linuxCriticalFullAvg10 is the share of wall time in which EVERY task was
// stalled on memory before this counts as critical.
//
// "full" rather than "some" because some pressure is ordinary on a host
// doing work, and full pressure means nothing ran. 5% is well below the
// 8.51 measured 1.3 seconds into a real thrash and well above the 0.00 that
// a host reads when it is merely busy — the measurement found no middle
// ground between them, so the exact value inside that gap decides nothing.
const linuxCriticalFullAvg10 = 5.0

// darwinCriticalLevel is the value of kern.memorystatus_vm_pressure_level
// that means the system is reclaiming aggressively. Apple's ladder is
// 1 normal, 2 warn, 4 critical, and 2 is where an ordinary desktop sits.
const darwinCriticalLevel = 4

// windowsFloorMB is the available-memory floor below which this package
// calls the situation critical even though Windows has not raised its own
// low-memory notification.
//
// The notification is real but small: on a 31.7 GiB host it fired at 887 MB
// available, which is 2.8% there and would be 0.7% on a 128 GB host. It
// does not scale with RAM, so on the large unified-memory hosts this product
// cares about it arrives after the machine has already stopped responding.
// The floor adds a share of RAM underneath it, with a gigabyte as the
// smallest it may be so that a small host is not stopped while it is merely
// busy.
//
// 3% is below anything a working host was measured at — the reference host's
// worst moment across seven large loads was 22.7% — so this cannot stop a
// load that was going to succeed. The minimum wins below about 34 GB of RAM;
// on the 31.7 GiB host where the notification was measured it puts the floor
// at 1024 MB, a little above the 887 MB at which Windows raised its own.
func windowsFloorMB(totalPhysMB uint64) uint64 {
	const minFloorMB = 1024
	floor := totalPhysMB * 3 / 100
	if floor < minFloorMB {
		return minFloorMB
	}
	return floor
}

// levelFrom is the whole decision, as one untagged function over the facts,
// so all three platforms are decided by code every platform compiles and
// tests (the initStateDirMode shape).
//
// An error means the facts do not answer the question; the Level returned
// with it is LevelUnknown.
func levelFrom(goos string, f Facts) (Level, error) {
	switch goos {
	case "linux":
		if f.LinuxErr != nil {
			return LevelUnknown, f.LinuxErr
		}
		switch {
		case f.LinuxFullAvg10 >= linuxCriticalFullAvg10:
			return LevelCritical, nil
		case f.LinuxSomeAvg10 > 0:
			return LevelWarn, nil
		default:
			return LevelNormal, nil
		}
	case "darwin":
		if f.DarwinErr != nil {
			return LevelUnknown, f.DarwinErr
		}
		switch {
		case f.DarwinLevel >= darwinCriticalLevel:
			return LevelCritical, nil
		case f.DarwinLevel == 2:
			return LevelWarn, nil
		case f.DarwinLevel == 1:
			return LevelNormal, nil
		default:
			return LevelUnknown, fmt.Errorf("mempressure: unknown darwin pressure level %d", f.DarwinLevel)
		}
	case "windows":
		// The notification comes first: when Windows itself says memory is
		// low, that is the answer whatever the numbers say.
		if f.WindowsLowSignaled == 1 {
			return LevelCritical, nil
		}
		if f.WindowsTotalPhysMB == 0 {
			// No numbers. A readable notification still answers half the
			// question; an unreadable one answers none of it.
			if f.WindowsLowSignaled == 0 {
				return LevelNormal, nil
			}
			if f.WindowsErr != nil {
				return LevelUnknown, f.WindowsErr
			}
			return LevelUnknown, fmt.Errorf("mempressure: windows memory status unavailable")
		}
		floor := windowsFloorMB(f.WindowsTotalPhysMB)
		switch {
		case f.WindowsAvailPhysMB < floor:
			return LevelCritical, nil
		case f.WindowsAvailPhysMB < 2*floor:
			return LevelWarn, nil
		default:
			return LevelNormal, nil
		}
	default:
		return LevelUnknown, fmt.Errorf("mempressure: no reader for %s", goos)
	}
}

// Sampler reads this host's memory pressure. It holds whatever the platform
// needs to keep open between samples (on Windows, the notification handle),
// so callers take one and reuse it rather than building one per sample.
//
// The zero value is not usable; call New.
type Sampler struct {
	plat platformSampler
	// factsFn is the seam a test replaces. The real one is exercised by
	// the platform's own test, because a seam nothing calls for real is a
	// test of nothing.
	factsFn func() Facts
	goos    string
}

// New returns a Sampler for this host.
func New() *Sampler {
	s := &Sampler{goos: runtime.GOOS}
	s.plat = newPlatformSampler()
	s.factsFn = s.plat.facts
	return s
}

// Level takes one sample and says how bad the OS calls it.
func (s *Sampler) Level() (Level, error) {
	if s == nil || s.factsFn == nil {
		return LevelUnknown, fmt.Errorf("mempressure: sampler not initialised")
	}
	return levelFrom(s.goos, s.factsFn())
}

// Close releases whatever the platform held. It is safe on a nil Sampler
// and safe to call twice.
func (s *Sampler) Close() error {
	if s == nil || s.plat == nil {
		return nil
	}
	return s.plat.close()
}

// platformSampler is the per-OS half: one sample, and whatever handle that
// needs kept alive.
type platformSampler interface {
	facts() Facts
	close() error
}
