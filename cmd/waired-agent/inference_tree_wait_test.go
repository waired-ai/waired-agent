package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// A record of today's rule (waired-ai/waired-agent#1443): only a model
// switch falls back to restarting the agent, and not when the start only
// declined to spawn beside a previous engine's processes.
func TestWedgeRestartWarranted(t *testing.T) {
	stuck := fmt.Errorf("ollama: %w (pid 1)", infruntime.ErrPreviousEngineStillExiting)
	cases := []struct {
		name string
		swap bool
		err  error
		want bool
	}{
		{"switch, engine wedged", true, errors.New("not ready within 150s"), true},
		{"switch, previous engine still exiting", true, stuck, false},
		{"no switch, engine wedged", false, errors.New("not ready within 150s"), false},
		{"no switch, previous engine still exiting", false, stuck, false},
	}
	for _, c := range cases {
		if got := wedgeRestartWarranted(c.swap, c.err); got != c.want {
			t.Errorf("%s: wedgeRestartWarranted = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResidencyWarmRetryAfter(t *testing.T) {
	cases := map[int]time.Duration{
		0:   time.Minute,
		1:   time.Minute,
		2:   2 * time.Minute,
		3:   4 * time.Minute,
		4:   8 * time.Minute,
		5:   16 * time.Minute,
		6:   30 * time.Minute,
		7:   30 * time.Minute,
		1e6: 30 * time.Minute,
	}
	for fails, want := range cases {
		if got := residencyWarmRetryAfter(fails); got != want {
			t.Errorf("residencyWarmRetryAfter(%d) = %s, want %s", fails, got, want)
		}
	}
}

// A load that keeps failing stretches the maintainer's pace; the first
// retry is still a minute away, as before (waired-ai/waired-agent#1443).
func TestMaintainResidency_BacksOffAfterConsecutiveFailures(t *testing.T) {
	e := &warmEngine{failLoads: true}
	p := warmProvider(t, e, "model-a", "a:q4")

	for i := 1; i <= 3; i++ {
		p.warmServingModelNow(context.Background())
		if got := p.warmFails.consecutive(); got != i {
			t.Fatalf("after %d failed warm-ups, consecutive = %d", i, got)
		}
	}
	// Three failures: the next automatic warm-up is four minutes out.
	p.warmEndedAt.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	loadsBefore := len(e.recorded())
	p.maintainResidency()
	waitForWarm(t, p)
	if got := len(e.recorded()); got != loadsBefore {
		t.Fatalf("retried %d times inside the stretched pace, want 0", got-loadsBefore)
	}
	p.warmEndedAt.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	p.maintainResidency()
	waitForWarm(t, p)
	if got := len(e.recorded()); got != loadsBefore+1 {
		t.Fatalf("loads past the stretched pace = %d, want 1", got-loadsBefore)
	}
}

// A load that succeeds, or a model found already loaded, clears the count.
func TestWarmServingModel_SuccessClearsTheFailureCount(t *testing.T) {
	e := &warmEngine{failLoads: true}
	p := warmProvider(t, e, "model-a", "a:q4")
	p.warmServingModelNow(context.Background())
	p.warmServingModelNow(context.Background())
	if got := p.warmFails.consecutive(); got != 2 {
		t.Fatalf("consecutive = %d, want 2", got)
	}
	e.setFailLoads(false)
	p.warmServingModelNow(context.Background())
	if got := p.warmFails.consecutive(); got != 0 {
		t.Errorf("consecutive after a successful load = %d, want 0", got)
	}
}

// A different load — here another model — starts the count again: its
// failures say nothing about the one that failed before.
func TestWarmServingModel_ADifferentLoadStartsTheCountAgain(t *testing.T) {
	e := &warmEngine{failLoads: true}
	p := warmProvider(t, e, "model-a", "a:q4")
	p.warmServingModelNow(context.Background())
	p.warmServingModelNow(context.Background())
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["model-b"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "b:q4"}
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "model-b"}
	}); err != nil {
		t.Fatalf("switch active: %v", err)
	}
	p.warmServingModelNow(context.Background())
	if got := p.warmFails.consecutive(); got != 1 {
		t.Errorf("consecutive after the first failure of another model = %d, want 1", got)
	}
}
