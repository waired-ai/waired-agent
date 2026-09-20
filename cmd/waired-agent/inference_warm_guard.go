package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/mempressure"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// loadWatch is one load's memory guard: the goroutine watching the host, and
// the answer to "did it stop this load".
type loadWatch struct {
	stop func()

	mu  sync.Mutex
	hit *infruntime.LoadMemoryFailure
}

// done ends the watch. Safe on a nil watch and safe to call twice.
func (w *loadWatch) done() {
	if w == nil || w.stop == nil {
		return
	}
	w.stop()
}

// failure reports the guard's verdict, if it reached one.
func (w *loadWatch) failure() (infruntime.LoadMemoryFailure, bool) {
	if w == nil {
		return infruntime.LoadMemoryFailure{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hit == nil {
		return infruntime.LoadMemoryFailure{}, false
	}
	return *w.hit, true
}

// watchLoadMemory starts a guard for the duration of one load. cancelLoad is
// called when the host has been out of memory long enough, and the engine is
// retired behind it.
//
// The ENGINE has to go, not just the request. ollama is loading on the other
// side of that HTTP call and holding everything it has taken, so abandoning
// the call would leave the memory exactly where it is. Stop is also what
// records the process tree, so the next start waits for it instead of
// spawning a second copy beside it (waired-agent#1450).
func (p *agentInferenceProvider) watchLoadMemory(ctx context.Context, cancelLoad context.CancelFunc) *loadWatch {
	w := &loadWatch{stop: func() {}}
	if p == nil || p.ollama == nil {
		return w
	}
	guard := infruntime.NewLoadGuard(p.logger)
	gctx, gcancel := context.WithCancel(ctx)
	var once sync.Once
	w.stop = func() {
		once.Do(func() {
			gcancel()
			_ = guard.Close()
		})
	}
	go guard.Watch(gctx, func(reason string, f mempressure.Facts) {
		w.mu.Lock()
		w.hit = &infruntime.LoadMemoryFailure{
			Reason: reason,
			Detail: mempressureDetail(f),
			At:     time.Now(),
		}
		w.mu.Unlock()
		cancelLoad()
		// A detached context: the load's own is being cancelled right now,
		// and the stop has its own bound (StopTimeout, #316) that nothing
		// here may shorten.
		if err := p.ollama.Stop(p.backgroundCtx()); err != nil && p.logger != nil {
			p.logger.Warn("could not stop the engine after a load ran the computer out of memory",
				"err", err)
		}
	})
	return w
}

// mempressureDetail renders the readings the guard acted on, so the record
// and the log carry the numbers and not just the verdict.
func mempressureDetail(f mempressure.Facts) string {
	return fmt.Sprintf("available %d MB of %d MB, swapping out %.1f MB/s",
		f.AvailMB, f.TotalMB, f.SwapOutMBPerSec)
}
