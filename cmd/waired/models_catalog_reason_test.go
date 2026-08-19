package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestReasonMirrorsMatchProto pins the two fit codes cmd/waired keeps as
// its own literals against the real ones.
//
// The literals are the convention here — this package decodes the
// management API with its own structs rather than importing the server
// side — and the convention is fine right up until one of them is
// misspelled. Both gate a branch whose OTHER arm says something about
// memory, so a drifted literal does not fail loudly: it silently sends
// every engine-floor row back through the memory sentence, which is the
// defect waired-agent#836 and waired-agent#850 are about.
func TestReasonMirrorsMatchProto(t *testing.T) {
	if reasonNoVariantForEngine != hostfit.ReasonNoVariantForEngine {
		t.Errorf("reasonNoVariantForEngine = %q, want %q",
			reasonNoVariantForEngine, hostfit.ReasonNoVariantForEngine)
	}
	if reasonEngineTooOld != hostfit.ReasonEngineTooOld {
		t.Errorf("reasonEngineTooOld = %q, want %q",
			reasonEngineTooOld, hostfit.ReasonEngineTooOld)
	}
}

// TestWarnModelWillNotRun_EngineFloorNeverBlamesMemory is the wording
// contract for waired-agent#836.
//
// Before it, a host with 121 GB of RAM was told its memory was the
// problem, shown a breakdown of that memory, and pointed at
// `models ls --detail` to find something smaller — for a model that the
// engine simply could not load yet. The engine's own name leaked into
// the sentence too, which is the one word a user can do nothing with.
func TestWarnModelWillNotRun_EngineFloorNeverBlamesMemory(t *testing.T) {
	host := catalogDetailHost{RAMTotalGB: 121, OSReservedGB: 11, VRAMTotalMB: 24576}
	fam := catalogDetailFamily{
		DisplayName:  "Qwen3.8 27B",
		DeficitLabel: "needs AI engine 0.32.13 (this computer has 0.31.1)",
		Fit: &catalogDetailFit{
			Reason:            reasonEngineTooOld,
			NeedEngineVersion: "0.32.13",
			HaveEngineVersion: "0.31.1",
		},
	}

	var b bytes.Buffer
	warnModelWillNotRun(&b, "Qwen3.8 27B", fam, host)
	got := b.String()

	for _, banned := range []string{"memory: ", "does not fit in this computer's memory", "ollama", "what does fit"} {
		if strings.Contains(got, banned) {
			t.Errorf("engine-floor warning contains %q; the wall is the engine, not the memory:\n%s",
				banned, got)
		}
	}
	for _, want := range []string{"0.32.13", "0.31.1", "AI engine", "waired update"} {
		if !strings.Contains(got, want) {
			t.Errorf("engine-floor warning is missing %q:\n%s", want, got)
		}
	}

	t.Run("unknown version says so and does not invent one", func(t *testing.T) {
		fam := fam
		fit := *fam.Fit
		fit.HaveEngineVersion = ""
		fam.Fit = &fit
		var b bytes.Buffer
		warnModelWillNotRun(&b, "Qwen3.8 27B", fam, host)
		got := b.String()
		if !strings.Contains(got, "could not be read") {
			t.Errorf("want the unknown-version sentence:\n%s", got)
		}
		if strings.Contains(got, "this computer has ") {
			t.Errorf("warning claims a version it does not have:\n%s", got)
		}
	})

	// The other arm is untouched: a real memory shortfall still gets the
	// memory sentence, so this change narrowed the branch rather than
	// replacing it.
	t.Run("a memory shortfall still says memory", func(t *testing.T) {
		fam := catalogDetailFamily{
			DisplayName:  "Qwen3.5 122B",
			DeficitLabel: "needs 62 GB — 11 GB allocatable",
			Fit:          &catalogDetailFit{Reason: "insufficient_memory"},
		}
		var b bytes.Buffer
		warnModelWillNotRun(&b, "Qwen3.5 122B", fam, host)
		if got := b.String(); !strings.Contains(got, "does not fit in this computer's memory") {
			t.Errorf("memory shortfall lost its wording:\n%s", got)
		}
	})

	// An older agent sends no code at all. It must keep the behaviour it
	// has always had rather than falling into a branch it cannot reach.
	t.Run("an agent with no fit block keeps the old wording", func(t *testing.T) {
		fam := catalogDetailFamily{
			DisplayName:  "Qwen3.8 27B",
			DeficitLabel: "needs ollama ≥ 0.32.13 (running 0.31.1)",
		}
		var b bytes.Buffer
		warnModelWillNotRun(&b, "Qwen3.8 27B", fam, host)
		if got := b.String(); !strings.Contains(got, "does not fit in this computer's memory") {
			t.Errorf("pre-#836 wire lost its wording:\n%s", got)
		}
	})
}
