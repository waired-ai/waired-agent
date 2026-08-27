package main

import (
	"context"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// tuningAdapter is a fakeAdapter that also reports an applied tuning, so the
// `interface{ AppliedTuning() ... }` assertion every vLLM read site makes is
// actually TAKEN. With a plain fakeAdapter the assertion fails and each reader
// returns before touching the tuning, which would leave half the read path
// unexercised.
type tuningAdapter struct {
	fakeAdapter
	tuning infruntime.ModelTuning
}

func (a tuningAdapter) AppliedTuning() infruntime.ModelTuning { return a.tuning }

// The vLLM adapter is written by the engine-startup goroutine (bootstrapVLLM)
// and read from the management handlers and the shutdown goroutine. As a plain
// field that was an unsynchronised write/read of an interface value — two
// words — so a torn read hands a reader one adapter's type descriptor with
// another's data pointer (#337).
//
// The serving-engine field is now written from the same goroutine and read by
// the same handlers, for the same reason: #339 made it a live decision rather
// than a boot-time constant, so it is exercised here alongside the adapter.
//
// This drives all four real read shapes against a concurrent write. It is a
// regression bar for the accessors, and it only FAILS under `-race`: nothing
// here asserts a value, because the defect is not a wrong answer. That is why
// it arrived with the race lane (#506) — before that lane, no CI run would
// have executed it in the mode that can fail.
//
// The provider is a struct literal rather than a real boot: no venv, no
// subprocess, no weights. The subject is the field, and putting a real engine
// underneath it would mean the test could not run on darwin or windows.
func TestVLLMAdapterConcurrentBootAndReads(t *testing.T) {
	ctx := context.Background()
	m := catalog.Manifest{ModelID: "gpt-oss-20b", ContextLength: 131072}

	p := &agentInferenceProvider{
		logger:   discardLogger(),
		registry: infruntime.NewRegistry(),
	}
	p.setServingEngine(catalog.RuntimeVLLM)
	// runtimeStatusFor looks the adapter up in the registry before it reads
	// the field, and calls Health on whatever it finds.
	p.registry.Register(tuningAdapter{fakeAdapter: fakeAdapter{name: "vllm"}})

	newAdapter := func() tuningAdapter {
		return tuningAdapter{
			fakeAdapter: fakeAdapter{name: "vllm"},
			tuning: infruntime.ModelTuning{
				ModelID:       m.ModelID,
				ContextLength: 59392,
				Warning:       "context window clamped so the KV cache fits",
			},
		}
	}

	// Every goroutine that reads the adapter in production, in the shape it
	// reads it: the management handlers via EngineProvenance and
	// runtimeStatusFor, the window computation via appliedContextWindow, and
	// the shutdown goroutine's nil-check-then-Stop.
	readers := []func(){
		func() { _ = p.appliedContextWindow(m) },
		func() { _ = p.runtimeStatusFor(ctx, "vllm", hardware.Profile{}) },
		func() { _ = (&inferenceSubsystem{provider: p}).EngineProvenance() },
		func() {
			if a := p.vllmAdapter(); a != nil {
				_ = a.Stop(ctx)
			}
		},
	}

	const iterations = 200
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			p.setVLLM(newAdapter())
		}
	}()
	// adoptEngine's write, without the store update: every reader above
	// reaches servingEngine(), which is what decides whether they look at
	// the vLLM adapter at all. Re-storing the same value still races a
	// plain string field, so this bar does not need the engine to change.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			p.setServingEngine(catalog.RuntimeVLLM)
		}
	}()
	for _, read := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				read()
			}
		}()
	}
	wg.Wait()
}
