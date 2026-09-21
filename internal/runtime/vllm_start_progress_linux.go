//go:build linux

package runtime

import (
	"io"
	"sync/atomic"
	"time"
)

// A vLLM start is judged by whether the engine is still working, not by a
// fixed number of failed health probes (waired-agent#1508).
//
// The fixed count gave up at about 120 s. A first start from a venv built
// minutes earlier also compiles flashinfer's kernels with nvcc, since
// 0.28 no longer ships them prebuilt: Qwen3.5-4B on an RTX PRO 4000 took
// longer than 133 s that way and read engine_failed until the next attempt,
// and the GPU e2e lane measured about 4 minutes cold on an L4. A 35B
// model's weight load alone approaches the old budget. Every vLLM version
// move builds a new venv, so the first start after each one is cold.
//
// The engine is working while it writes output or its process group uses
// CPU. A compile can run for minutes without logging, which is what the
// CPU signal is for. The shape is the one the peer leg's wait takes
// (docs/decisions/20260828/0143) and the download stall guard's (#189):
// a stall window for "nothing is happening", and a ceiling for a start
// that keeps doing something without ever becoming ready.
const (
	// DefaultVLLMStartStallTimeout is how long a start may show no output
	// and no CPU work before it is given up. More than twice the cold 4B
	// start above, and more than the L4's cold start; a healthy start is
	// silent AND idle only while it waits on something short.
	DefaultVLLMStartStallTimeout = 5 * time.Minute
	// DefaultVLLMStartTimeout is the ceiling on the whole wait, progress
	// or not: a start that is busy without end (a spin-wait in NCCL, a
	// log loop) is still ended. Several times the largest cold start seen.
	DefaultVLLMStartTimeout = 30 * time.Minute

	// vllmStartFirstNote and vllmStartNoteEvery space the INFO lines a long
	// start writes, so the log explains a wait past where the old budget
	// would have cut it.
	vllmStartFirstNote = 2 * time.Minute
	vllmStartNoteEvery = 5 * time.Minute
)

// startVerdict is what one observation of a start in progress concludes.
type startVerdict int

const (
	startWaiting startVerdict = iota
	// startStalled: no output and no CPU work for the stall window.
	startStalled
	// startTimedOut: the ceiling passed while the engine kept working.
	startTimedOut
)

// startProgress follows one start. Pure: every reading is handed in, so the
// rule is table-testable with a made-up clock.
type startProgress struct {
	stall, ceiling time.Duration

	start        time.Time
	lastProgress time.Time
	lastObserve  time.Time
	nextNote     time.Time

	out     int64
	cpu     time.Duration
	cpuSeen bool
}

func newStartProgress(now time.Time, stall, ceiling time.Duration) *startProgress {
	return &startProgress{
		stall: stall, ceiling: ceiling,
		start: now, lastProgress: now, lastObserve: now,
		nextNote: now.Add(vllmStartFirstNote),
	}
}

// observe takes one reading: the bytes the child has written so far, and
// its process group's CPU time when the platform can read it. CPU counts as
// work when at least half a core was busy on average since the previous
// reading; a count that went DOWN (a member exited without a group member
// reaping it) only resets the baseline.
func (s *startProgress) observe(now time.Time, out int64, cpu time.Duration, cpuOK bool) startVerdict {
	if out > s.out {
		s.out = out
		s.lastProgress = now
	}
	if cpuOK {
		if s.cpuSeen && cpu-s.cpu >= now.Sub(s.lastObserve)/2 && now.After(s.lastObserve) {
			s.lastProgress = now
		}
		s.cpu, s.cpuSeen = cpu, true
	}
	s.lastObserve = now
	switch {
	case now.Sub(s.start) >= s.ceiling:
		return startTimedOut
	case now.Sub(s.lastProgress) >= s.stall:
		return startStalled
	}
	return startWaiting
}

// answered records a health probe the engine answered: a server that
// answers is working, even while it has not answered enough times in a row
// to be called ready.
func (s *startProgress) answered(now time.Time) { s.lastProgress = now }

// dueNote reports whether a "still starting" line is due at now, and moves
// the next one along.
func (s *startProgress) dueNote(now time.Time) bool {
	if now.Before(s.nextNote) {
		return false
	}
	s.nextNote = now.Add(vllmStartNoteEvery)
	return true
}

// countingWriter counts what the child writes before the engine.log cap
// sees it, so output past the cap still counts as work.
type countingWriter struct {
	w io.Writer
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.w.Write(p)
}

// treeCPUTimer is implemented by a RunningProcess that can read the CPU
// time its process group has used (Linux, where vLLM runs).
type treeCPUTimer interface {
	TreeCPUTime() (time.Duration, error)
}

// treeCPU reads proc's group CPU time when it can.
func treeCPU(proc RunningProcess) (time.Duration, bool) {
	t, ok := proc.(treeCPUTimer)
	if !ok {
		return 0, false
	}
	d, err := t.TreeCPUTime()
	return d, err == nil
}
