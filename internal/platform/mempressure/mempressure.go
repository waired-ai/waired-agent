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
	"sync"
	"time"
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
	// AvailMB and TotalMB are this host's memory, read the same way on
	// every platform. 0 means the reading failed.
	//
	// Cross-OS rather than per-OS because the rule they feed is cross-OS:
	// a swap surge only counts as trouble when there is also nothing left,
	// and that second half is the same question everywhere.
	AvailMB uint64
	TotalMB uint64

	// SwapOutMBPerSec is how fast the host is pushing memory out to swap,
	// averaged over the interval between the last two samples. Negative
	// means it is not known yet — the first sample has nothing to difference
	// against, and that is not a rate of zero.
	SwapOutMBPerSec float64
	// SwapOutTotalMB is what SwapOutMBPerSec is differenced from: MB sent to
	// swap, counted from whenever the platform started counting. -1 means
	// the platform cannot say. Carried on Facts rather than kept private so
	// the differencing is one untagged function with its own test, and so a
	// caller debugging a host can see the raw figure.
	//
	// Not necessarily monotonic: on macOS this is the swap file's used
	// bytes, which falls when swap is released. rateFrom treats a fall as
	// no swapping rather than as a negative rate.
	SwapOutTotalMB float64

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

// availFloorMB is the available-memory floor below which this host counts as
// short of memory.
//
// Short is not the same as critical. Crossing this alone is a Warn: a host
// can dip here briefly and recover, and stopping a load on a dip would stop
// loads that were going to succeed. It becomes Critical only together with a
// swap surge — see levelFrom.
//
// The share exists because Windows' own low-memory notification does not
// scale: it fired at 887 MB available on a 31.7 GiB host, which would be
// 0.7% on a 128 GB host and therefore long after the machine stopped
// answering. The minimum exists because a share alone is meaningless on a
// small host, where 3% of 8 GB is 245 MB and past saving.
//
// 3% is far below anything a working host was measured at: the reference
// host's worst moment across seven large loads — including three that failed
// — was 22.7% available. The minimum wins below about 34 GB of RAM.
func availFloorMB(totalMB uint64) uint64 {
	const minFloorMB = 1024
	floor := totalMB * 3 / 100
	if floor < minFloorMB {
		return minFloorMB
	}
	return floor
}

// shortOfMemory reports whether what is left has fallen under availFloorMB.
// Unknown numbers are not a shortage: a reading nobody has cannot be low.
func shortOfMemory(f Facts) bool {
	if f.TotalMB == 0 || f.AvailMB == 0 {
		return false
	}
	return f.AvailMB < availFloorMB(f.TotalMB)
}

// swapSurgeMBPerSec is how fast memory has to be going out to swap before
// that counts as a surge.
//
// Set high on purpose, and affordable because this is the EARLY exit and not
// the only one: a thrash too slow to reach it is still caught by the
// operating system's own critical signal a little later (see levelFrom).
// Missing a surge costs latency; firing on a healthy host costs a load that
// was going to succeed.
//
// Measured on 2026-09-20, five idle or busy minutes per host, one sample a
// second:
//
//	linux, 124 GB, idle             pswpout moved 0 pages in 300 samples
//	macOS, 16 GB, 3.28 GB in swap   swapouts moved 0 in 292 samples
//	windows, 31.7 GB, 84% used      pages out/sec: mean 4.4, max 294
//
// The worst any healthy host reached was 294 pages a second, about
// 1.15 MB/s, on the Windows host that was already 84% full. A real thrash on
// the Linux host moved 32,748 pages in 0.2 s — about 640 MB/s. So there are
// nearly three orders of magnitude between the two, and 32 MB/s sits 28x
// above the worst normal reading and 20x below the measured thrash.
//
// The macOS figure is the one that decided the SHAPE of this term rather
// than its value: that host had 3.28 GB sitting in swap and was swapping at
// exactly zero. A trigger on swap in USE would have fired on an idle desktop.
const swapSurgeMBPerSec = 32

// swapSurging reports whether swap-out is running at swapSurgeMBPerSec or
// more. A rate nobody could compute yet (the first sample) is not a surge.
func swapSurging(f Facts) bool {
	return f.SwapOutMBPerSec >= swapSurgeMBPerSec && f.SwapOutMBPerSec > 0
}

// rateFrom turns two cumulative readings into MB per second.
//
// It answers -1 — "not known" — for the first sample, for an interval too
// short to divide by, and for a platform that cannot count. A fall in the
// counter is 0 rather than a negative rate: macOS reports swap in use rather
// than swap written, and releasing swap is not swapping backwards.
func rateFrom(prev, cur float64, dt time.Duration) float64 {
	if prev < 0 || cur < 0 || dt <= 0 {
		return -1
	}
	if cur <= prev {
		return 0
	}
	return (cur - prev) / dt.Seconds()
}

// levelFrom is the whole decision, as one untagged function over the facts,
// so all three platforms are decided by code every platform compiles and
// tests (the initStateDirMode shape).
//
// Two ways to reach Critical, and both are deliberately hard to reach:
//
//  1. The operating system says so itself. Linux with everything stalled on
//     memory, macOS at its CRITICAL level, Windows raising its low-memory
//     notification — each of those is the OS reporting that it is already in
//     trouble, and none of them is reached by a host with memory to spare.
//
//  2. There is nothing left AND memory is going out to swap fast. Neither
//     half is enough on its own, and that is the point of the rule rather
//     than a refinement of it. Hosts swap when they are perfectly healthy —
//     an idle mac mini in this fleet sits with gigabytes swapped out and a
//     compressor running — so a swap-rate trigger alone would fire on a
//     machine with a hundred gigabytes free. And a dip below the floor
//     alone is a moment, not a problem.
//
// An error means the facts do not answer the question; the Level returned
// with it is LevelUnknown.
func levelFrom(goos string, f Facts) (Level, error) {
	reported, err := osReportedLevel(goos, f)
	if err != nil {
		return LevelUnknown, err
	}
	if reported == LevelCritical {
		return LevelCritical, nil
	}
	if shortOfMemory(f) {
		if swapSurging(f) {
			return LevelCritical, nil
		}
		if reported < LevelWarn {
			return LevelWarn, nil
		}
	}
	return reported, nil
}

// osReportedLevel is what the operating system says on its own terms,
// before the shortage-and-surge rule is applied on top.
func osReportedLevel(goos string, f Facts) (Level, error) {
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
		switch f.WindowsLowSignaled {
		case 1:
			return LevelCritical, nil
		case 0:
			return LevelNormal, nil
		}
		// The notification could not be read. The memory numbers can still
		// carry the shortage half of the rule; without them there is nothing.
		if f.TotalMB == 0 {
			if f.WindowsErr != nil {
				return LevelUnknown, f.WindowsErr
			}
			return LevelUnknown, fmt.Errorf("mempressure: windows memory status unavailable")
		}
		return LevelNormal, nil
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
	now     func() time.Time

	mu       sync.Mutex
	prevSwap float64 // -1 until the first sample
	prevAt   time.Time
}

// New returns a Sampler for this host.
func New() *Sampler {
	s := &Sampler{goos: runtime.GOOS, now: time.Now, prevSwap: -1}
	s.plat = newPlatformSampler()
	s.factsFn = s.plat.facts
	return s
}

// Level takes one sample and says how bad the OS calls it.
//
// The swap rate is differenced against the previous call, so the FIRST call
// on a Sampler never reports a surge and a caller that wants one has to have
// sampled at least twice. That is the right shape for the guard this serves:
// it runs on a ticker for the length of a load, so the second tick is the
// first that can say anything about a rate.
func (s *Sampler) Level() (Level, error) {
	f, err := s.Sample()
	if err != nil {
		return LevelUnknown, err
	}
	return levelFrom(s.goos, f)
}

// Sample takes one reading, including the differenced swap rate. Exposed
// beside Level so a caller that wants to log WHY it stopped a load has the
// figures rather than just the verdict.
func (s *Sampler) Sample() (Facts, error) {
	if s == nil || s.factsFn == nil {
		return Facts{}, fmt.Errorf("mempressure: sampler not initialised")
	}
	f := s.factsFn()
	// A platform that counts cumulative bytes is differenced here, so the
	// rate maths lives in one untagged place. Windows is the exception: PDH
	// hands out a rate already, and its reader fills SwapOutMBPerSec itself.
	if f.SwapOutTotalMB >= 0 {
		at := s.now()
		s.mu.Lock()
		f.SwapOutMBPerSec = rateFrom(s.prevSwap, f.SwapOutTotalMB, at.Sub(s.prevAt))
		s.prevSwap, s.prevAt = f.SwapOutTotalMB, at
		s.mu.Unlock()
	}
	return f, nil
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
