package runtime

import (
	"context"
	"log/slog"
	goruntime "runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/mempressure"
)

// A model load is the one thing this product does that can take a computer
// down. The weights are tens of gigabytes, they have to become resident, and
// on a unified-memory host two of them do not fit. waired-agent#837 recorded
// what that looks like from outside: free memory collapsing to a few
// megabytes and staying there for more than ten minutes, the whole box
// starving, and no token ever produced.
//
// LoadGuard watches while a load is in flight and stops it before that.
//
// What it is NOT for: waired-agent#1443. That host had 28 GB free at the
// worst moment of every failing load — it ran out of DEVICE memory, not host
// memory, and no operating system signal moves at all. That failure is
// classified after the fact, in load_memory.go. Keeping the two apart is the
// whole design: this one protects the machine, that one protects the model
// choice.

// loadGuardPoll is how often the host is asked. A second is far finer than
// the thing being watched — a thrash lasts minutes — and the reading is a
// file read, a sysctl or one PDH collection.
const loadGuardPoll = time.Second

// loadGuardSustained is how many consecutive critical readings it takes to
// act.
//
// Ten seconds, and deliberately slow. The owner's direction (2026-09-20) is
// to stop only when it is really dangerous, because stopping a load that was
// going to succeed is the worse mistake. A host that has been under its
// memory floor AND swapping hard for ten unbroken seconds is not having a
// moment; waired-agent#837's host was in that state for over ten minutes.
const loadGuardSustained = 10

// criticalStreak counts consecutive critical readings.
//
// Anything that is not Critical breaks the run, including LevelUnknown. A
// reading nobody could take is not evidence of trouble, and treating it as
// one would stop loads on hosts where the reader simply does not work.
type criticalStreak struct {
	need int
	n    int
}

// observe records one reading and reports whether the run has just reached
// the length that warrants acting. It reports true once per run, not on
// every reading past the threshold: the caller stops a load, and stopping it
// twice is not a thing.
func (s *criticalStreak) observe(l mempressure.Level) bool {
	if l != mempressure.LevelCritical {
		s.n = 0
		return false
	}
	s.n++
	return s.n == s.need
}

// pressureSampler is what LoadGuard needs from mempressure, named here so a
// test can supply a scripted host.
type pressureSampler interface {
	Sample() (mempressure.Facts, error)
	Close() error
}

// LoadGuard watches host memory for the duration of a load.
type LoadGuard struct {
	sampler   pressureSampler
	goos      string
	poll      time.Duration
	sustained int
	log       *slog.Logger
}

// NewLoadGuard returns a guard reading this host. Close it when done.
func NewLoadGuard(log *slog.Logger) *LoadGuard {
	return newLoadGuard(mempressure.New(), goruntime.GOOS, loadGuardPoll, loadGuardSustained, log)
}

func newLoadGuard(s pressureSampler, goos string, poll time.Duration, sustained int, log *slog.Logger) *LoadGuard {
	if log == nil {
		log = slog.Default()
	}
	return &LoadGuard{sampler: s, goos: goos, poll: poll, sustained: sustained, log: log}
}

// Close releases the sampler.
func (g *LoadGuard) Close() error {
	if g == nil || g.sampler == nil {
		return nil
	}
	return g.sampler.Close()
}

// Watch samples until ctx ends, and calls stop once if the host stays
// critical for long enough.
//
// It never returns an error and never fails a load on its own: a reader that
// cannot read leaves the load alone, which is the behaviour before this
// existed. The caller decides what stopping means — cancelling the load's
// context and retiring the engine — because the guard has no view of either.
func (g *LoadGuard) Watch(ctx context.Context, stop func(reason string, f mempressure.Facts)) {
	if g == nil || g.sampler == nil {
		return
	}
	streak := criticalStreak{need: g.sustained}
	tick := time.NewTicker(g.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		f, err := g.sampler.Sample()
		if err != nil {
			// Log once per sample is too much; this is the rare path and a
			// debug line is what a bug report needs.
			g.log.Debug("load guard could not read host memory", "err", err)
			streak.observe(mempressure.LevelUnknown)
			continue
		}
		lvl, err := mempressure.LevelOf(g.goos, f)
		if err != nil {
			g.log.Debug("load guard could not judge host memory", "err", err)
			streak.observe(mempressure.LevelUnknown)
			continue
		}
		if !streak.observe(lvl) {
			continue
		}
		reason := "this computer was out of memory for " +
			(time.Duration(g.sustained) * g.poll).String() + " while loading the model"
		g.log.Warn("stopping a model load: the computer is running out of memory",
			"available_mb", f.AvailMB, "total_mb", f.TotalMB,
			"swap_out_mb_per_sec", f.SwapOutMBPerSec,
			"sustained_for", time.Duration(g.sustained)*g.poll)
		stop(reason, f)
		return
	}
}
