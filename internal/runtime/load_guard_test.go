package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/mempressure"
)

// Facts that read as each level on linux, so a scripted host can be written
// in levels rather than in numbers.
var (
	factsNormal   = mempressure.Facts{AvailMB: 100_000, TotalMB: 124_000}
	factsWarn     = mempressure.Facts{AvailMB: 100_000, TotalMB: 124_000, LinuxSomeAvg10: 4}
	factsCritical = mempressure.Facts{AvailMB: 700, TotalMB: 124_000, SwapOutMBPerSec: 640}
)

// scriptedHost hands out one reading per sample and then repeats the last.
type scriptedHost struct {
	mu      sync.Mutex
	script  []mempressure.Facts
	errs    []error
	n       int
	samples int
}

func (h *scriptedHost) Sample() (mempressure.Facts, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.n
	if i >= len(h.script) {
		i = len(h.script) - 1
	}
	h.n++
	h.samples++
	if i < len(h.errs) && h.errs[i] != nil {
		return mempressure.Facts{}, h.errs[i]
	}
	return h.script[i], nil
}

func (h *scriptedHost) Close() error { return nil }

func (h *scriptedHost) taken() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.samples
}

// TestCriticalStreak is the whole of the "sustained" rule.
//
// PRODUCT CONTRACT (waired-agent#1453, owner direction 2026-09-20): stopping
// a load that was going to succeed is the worse mistake, so a run of
// critical readings has to be unbroken. Anything else resets it, including a
// reading that could not be taken — a host whose reader does not work must
// not have its loads stopped.
func TestCriticalStreak(t *testing.T) {
	const need = 3
	c := mempressure.LevelCritical
	n := mempressure.LevelNormal
	w := mempressure.LevelWarn
	u := mempressure.LevelUnknown

	for _, tc := range []struct {
		name string
		seq  []mempressure.Level
		want int // index of the reading that fires, -1 for none
	}{
		{"three in a row", []mempressure.Level{c, c, c}, 2},
		{"not yet", []mempressure.Level{c, c}, -1},
		{"a normal reading breaks it", []mempressure.Level{c, c, n, c, c}, -1},
		{"a warn reading breaks it", []mempressure.Level{c, c, w, c, c}, -1},
		{"CONTRACT: a reading nobody could take breaks it",
			[]mempressure.Level{c, c, u, c, c}, -1},
		{"it starts again after a break", []mempressure.Level{c, n, c, c, c}, 4},
		{"it fires once, not on every reading past the line",
			[]mempressure.Level{c, c, c, c, c}, 2},
		{"never critical", []mempressure.Level{n, w, n, w}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := criticalStreak{need: need}
			fired := -1
			for i, lvl := range tc.seq {
				if s.observe(lvl) {
					if fired >= 0 {
						t.Fatalf("fired twice, at %d and %d", fired, i)
					}
					fired = i
				}
			}
			if fired != tc.want {
				t.Errorf("fired at index %d, want %d", fired, tc.want)
			}
		})
	}
}

// TestLoadGuard_StopsOnASustainedRun is the detection half.
func TestLoadGuard_StopsOnASustainedRun(t *testing.T) {
	host := &scriptedHost{script: []mempressure.Facts{factsCritical}}
	g := newLoadGuard(host, "linux", time.Millisecond, 3, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		stops   int
		reason  string
		stopped = make(chan struct{})
	)
	g.Watch(ctx, func(r string, _ mempressure.Facts) {
		mu.Lock()
		stops++
		reason = r
		mu.Unlock()
		close(stopped)
	})

	select {
	case <-stopped:
	default:
		t.Fatal("the guard returned without stopping a load on a host that was critical throughout")
	}
	mu.Lock()
	defer mu.Unlock()
	if stops != 1 {
		t.Errorf("stop called %d times, want 1", stops)
	}
	if reason == "" {
		t.Error("stopped with no reason to show an operator")
	}
}

// TestLoadGuard_LeavesAHealthyHostAlone is the misfire half, and the one
// that matters: this guard sits in the path of every model load.
func TestLoadGuard_LeavesAHealthyHostAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts mempressure.Facts
		err   error
	}{
		{name: "idle", facts: factsNormal},
		{name: "under some pressure", facts: factsWarn},
		// A host swapping hard with plenty of memory free: another program's
		// business, not ours.
		{name: "swapping hard with memory to spare",
			facts: mempressure.Facts{AvailMB: 100_000, TotalMB: 124_000, SwapOutMBPerSec: 640}},
		// Short of memory but not swapping: a tight host, not a dying one.
		{name: "short but quiet",
			facts: mempressure.Facts{AvailMB: 700, TotalMB: 124_000, SwapOutMBPerSec: 0}},
		{name: "a reader that does not work", err: errors.New("no /proc/pressure/memory")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &scriptedHost{script: []mempressure.Facts{tc.facts}, errs: []error{tc.err}}
			g := newLoadGuard(host, "linux", time.Millisecond, 3, slog.New(slog.DiscardHandler))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()

			stopped := false
			g.Watch(ctx, func(string, mempressure.Facts) { stopped = true })

			if stopped {
				t.Error("stopped a model load on a host that was not in trouble")
			}
			if host.taken() < 3 {
				t.Errorf("only %d samples taken; the test did not exercise the streak", host.taken())
			}
		})
	}
}

// TestLoadGuard_StopsWatchingWhenTheLoadEnds: the guard is scoped to one
// load, so a finished load must not leave a goroutine sampling forever.
func TestLoadGuard_StopsWatchingWhenTheLoadEnds(t *testing.T) {
	host := &scriptedHost{script: []mempressure.Facts{factsNormal}}
	g := newLoadGuard(host, "linux", time.Millisecond, 3, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		g.Watch(ctx, func(string, mempressure.Facts) {})
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not return when the load's context was cancelled")
	}
}
