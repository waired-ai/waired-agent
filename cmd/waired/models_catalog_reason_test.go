package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestReasonMirrorsMatchProto pins the fit codes cmd/waired keeps as its
// own literals against the real ones.
//
// The literals are the convention here — this package decodes the
// management API with its own structs rather than importing the server
// side — and the convention is fine right up until one of them is
// misspelled. A drifted literal does not fail loudly: it silently drops
// a row into whichever arm is left, which is the defect
// waired-agent#836, waired-agent#850 and waired-agent#862 are about. The
// three capacity codes matter for the same reason from the other side —
// since #862 they are the ALLOWLIST that reaches the memory sentence, so
// a misspelling there sends a real shortfall to the neutral arm and
// stops explaining the one wall this CLI can explain.
func TestReasonMirrorsMatchProto(t *testing.T) {
	for _, tc := range []struct{ got, want, name string }{
		{reasonNoVariantForEngine, hostfit.ReasonNoVariantForEngine, "reasonNoVariantForEngine"},
		{reasonEngineTooOld, hostfit.ReasonEngineTooOld, "reasonEngineTooOld"},
		{reasonInsufficientMemory, hostfit.ReasonInsufficientMemory, "reasonInsufficientMemory"},
		{reasonInsufficientRAM, hostfit.ReasonInsufficientRAM, "reasonInsufficientRAM"},
		{reasonInsufficientVRAM, hostfit.ReasonInsufficientVRAM, "reasonInsufficientVRAM"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestWarnModelWillNotRun_ShapeComesFromTheVerdict is the wording
// contract for waired-agent#862, in the shape the tray's
// TestUpdate_CatalogUnfitKindComesFromTheVerdict settled for #850.
//
// PRODUCT CONTRACT: the warning's shape is chosen from the machine code,
// and MEMORY IS THE ALLOWLIST. Before this, one special case sat in
// front of a memory paragraph and everything else fell into it — so a
// 16 GB host switching to a vLLM-only family was told the model "does
// not fit in this computer's memory: no Ollama variant", shown
// a breakdown of that memory, and pointed at a download that does not
// exist, for a verdict taken before the capacity check ever ran.
//
// The row that matters most here is the invented future code. It is not
// hypothetical: proto/hostfit's vocabulary has grown twice already
// (ReasonInsufficientMemory, then ReasonEngineTooOld), and each time
// this function was the one reader that inherited a sentence instead of
// being taught the case.
func TestWarnModelWillNotRun_ShapeComesFromTheVerdict(t *testing.T) {
	host := catalogDetailHost{RAMTotalGB: 16, OSReservedGB: 3}

	for _, tc := range []struct {
		name     string
		fam      catalogDetailFamily
		want     []string
		wantNone []string
	}{
		{
			name: "a capacity refusal still says memory",
			fam: catalogDetailFamily{
				DeficitLabel: "needs 62 GB — 11 GB allocatable",
				Fit:          &catalogDetailFit{Reason: reasonInsufficientMemory},
			},
			want: []string{
				"doesn't fit in this computer's memory: needs 62 GB — 11 GB allocatable",
				"This computer has 16 GB",
				"after the download completes",
			},
		},
		{
			// The two older capacity codes reach the same sentence:
			// which pool fell short is the deficit label's business,
			// not this branch's.
			name: "a RAM shortfall says memory too",
			fam: catalogDetailFamily{
				DeficitLabel: "needs 32 GB RAM (have 16 GB)",
				Fit:          &catalogDetailFit{Reason: reasonInsufficientRAM},
			},
			want: []string{"doesn't fit in this computer's memory: needs 32 GB RAM (have 16 GB)"},
		},
		{
			name: "a graphics-memory shortfall says memory too",
			fam: catalogDetailFamily{
				DeficitLabel: "needs 24 GB of VRAM (have 8 GB)",
				Fit:          &catalogDetailFit{Reason: reasonInsufficientVRAM},
			},
			want: []string{"doesn't fit in this computer's memory: needs 24 GB of VRAM (have 8 GB)"},
		},
		{
			// The reported case. The label the router leaves here is
			// "no Ollama variant"; since waired-ai/waired#1272 the arm
			// repeats it and names the engine (decision 20260819/1910
			// item 3 is superseded on this point).
			name: "no variant for the inference engine here",
			fam: catalogDetailFamily{
				DeficitLabel: "no Ollama variant",
				Fit:          &catalogDetailFit{Reason: reasonNoVariantForEngine},
			},
			want: []string{
				"has no Ollama variant, so the inference engine on this computer cannot run it",
				"what does run here",
			},
			wantNone: []string{
				"doesn't fit in this computer's memory",
				"This computer has ",
				"after the download completes",
				"what does fit",
			},
		},
		{
			name: "a verdict this CLI has not learned yet is echoed, not explained",
			fam: catalogDetailFamily{
				DeficitLabel: "needs 24 GB VRAM (no GPU)",
				Fit:          &catalogDetailFit{Reason: "some_reason_from_a_newer_agent"},
			},
			want: []string{"won't run on this computer: needs 24 GB VRAM (no GPU)"},
			wantNone: []string{
				"doesn't fit in this computer's memory",
				"This computer has ",
				"what does fit",
			},
		},
		{
			// Nothing to echo and nothing checked. The sentence stops
			// rather than reaching for a cause.
			name: "an unknown verdict with no label invents nothing",
			fam:  catalogDetailFamily{Fit: &catalogDetailFit{Reason: "some_reason_from_a_newer_agent"}},
			want: []string{"won't run on this computer."},
			wantNone: []string{
				"doesn't fit in this computer's memory",
				"there isn't enough memory on this computer",
			},
		},
		{
			// An agent older than #836 sends no code at all, and
			// DeficitLabel is the only thing it ever had. It keeps the
			// arm it has always had — deliberately not the neutral one,
			// which is for a code NEWER than this binary.
			name: "an agent with no fit block keeps the memory arm",
			fam:  catalogDetailFamily{DeficitLabel: "needs 32 GB RAM (have 16 GB)"},
			want: []string{"doesn't fit in this computer's memory: needs 32 GB RAM (have 16 GB)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			warnModelWillNotRun(&b, "Qwen3.5 122B", tc.fam, host)
			got := b.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			for _, banned := range tc.wantNone {
				if strings.Contains(got, banned) {
					t.Errorf("contains %q, which this verdict never checked:\n%s", banned, got)
				}
			}
		})
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
		DeficitLabel: "needs Ollama 0.32.13 (this computer has 0.31.1)",
		Fit: &catalogDetailFit{
			Reason:            reasonEngineTooOld,
			NeedEngineVersion: "0.32.13",
			HaveEngineVersion: "0.31.1",
		},
	}

	var b bytes.Buffer
	warnModelWillNotRun(&b, "Qwen3.8 27B", fam, host)
	got := b.String()

	for _, banned := range []string{"memory: ", "doesn't fit in this computer's memory", "ollama", "what does fit"} {
		if strings.Contains(got, banned) {
			t.Errorf("engine-floor warning contains %q; the wall is the engine, not the memory:\n%s",
				banned, got)
		}
	}
	for _, want := range []string{"0.32.13", "0.31.1", "Ollama", "waired update"} {
		if !strings.Contains(got, want) {
			t.Errorf("engine-floor warning is missing %q:\n%s", want, got)
		}
	}

	t.Run("unknown version says so and does not invent one", func(t *testing.T) {
		fam := fam
		fit := *fam.Fit
		fit.HaveEngineVersion = ""
		fam.Fit = &fit
		// The router words the unreadable-version arm itself; the CLI
		// repeats its label (waired-ai/waired#1272).
		fam.DeficitLabel = "needs Ollama 0.32.13 (this computer's version could not be read)"
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
		if got := b.String(); !strings.Contains(got, "doesn't fit in this computer's memory") {
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
		if got := b.String(); !strings.Contains(got, "doesn't fit in this computer's memory") {
			t.Errorf("pre-#836 wire lost its wording:\n%s", got)
		}
	})
}
