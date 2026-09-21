package router

import (
	"errors"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// LocalActiveOnly is what the overlay listener sets for a request from
// another computer (waired-ai/waired#1477, found 2026-09-22): only the model
// this computer is serving may answer it. Before it, a request that named a
// model which was merely on disk was served — ollama swapped the engine
// onto it, evicting what the owner runs, and a Public Share guest could
// reach a model the owner imported for their account only
// (waired-ai/waired#1473, owner ruling 4). Product contract: ruling 4 and
// docs/decisions/20260922/0300-custom-model-rulings.md in the private
// control-plane repository.

func onDisk() catalog.Manifest {
	m := qwen()
	m.ModelID = "gemma-on-disk"
	m.ModelAliases = nil
	m.Variants[0].VariantID = "q4-gguf"
	m.Variants[0].Source.Tag = "gemma:on-disk"
	return m
}

func twoReadyOneActive() catalog.State {
	st := readyState()
	st.Models["gemma-on-disk"] = catalog.ModelState{
		VariantID: "q4-gguf", OllamaTag: "gemma:on-disk", State: catalog.ModelStateReady, PulledAt: time.Now(),
	}
	st.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b-instruct", VariantID: "q4-gguf"}
	return st
}

func activeOnlySelector(activeOnly bool) *Selector {
	return NewSelector(Inputs{
		Manifests:       []catalog.Manifest{qwen(), onDisk()},
		LocalState:      twoReadyOneActive(),
		Hardware:        goodHardware(),
		Runtimes:        registryWithOllama(),
		LocalActiveOnly: activeOnly,
	})
}

// The hole, reproduced: without the gate, a model that is only on disk
// is served locally.
func TestLocalActiveOnly_WithoutItAnOnDiskModelIsServed(t *testing.T) {
	sel, err := activeOnlySelector(false).Select(t.Context(), Request{Model: "gemma-on-disk"})
	if err != nil || sel.ExecutionMode != "local" {
		t.Fatalf("tripwire: the fixture no longer reproduces the hole (sel=%+v err=%v)", sel, err)
	}
}

func TestLocalActiveOnly_AnOnDiskModelIsRefused(t *testing.T) {
	sel, err := activeOnlySelector(true).Select(t.Context(), Request{Model: "gemma-on-disk"})
	if err == nil {
		t.Fatalf("a request from another computer reached a model this one is not serving: %+v", sel)
	}
	if !errors.Is(err, ErrModelNotActive) {
		t.Errorf("err = %v, want ErrModelNotActive", err)
	}
}

func TestLocalActiveOnly_TheActiveModelStillServes(t *testing.T) {
	for _, model := range []string{"qwen3-8b-instruct", "qwen3:8b-q4_K_M", "waired/default"} {
		sel, err := activeOnlySelector(true).Select(t.Context(), Request{Model: model})
		if err != nil || sel.ExecutionMode != "local" || sel.ModelID != "qwen3-8b-instruct" {
			t.Errorf("%s: sel=%+v err=%v, want the active model served locally", model, sel, err)
		}
	}
}

// No active model — the engine is between models — means nothing serves a
// request from another computer.
func TestLocalActiveOnly_NothingActiveServesNothing(t *testing.T) {
	st := twoReadyOneActive()
	st.Active = nil
	s := NewSelector(Inputs{
		Manifests: []catalog.Manifest{qwen(), onDisk()}, LocalState: st,
		Hardware: goodHardware(), Runtimes: registryWithOllama(), LocalActiveOnly: true,
	})
	if sel, err := s.Select(t.Context(), Request{Model: "qwen3-8b-instruct"}); err == nil {
		t.Fatalf("served with no active model: %+v", sel)
	}
}
