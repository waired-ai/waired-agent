package main

import (
	"os"
	"strings"
	"testing"
)

// TestBundledModelLabel_ResolvesAgainstTheCompleteSet: the benchmark
// lines name a model somebody already has, so resolution takes EVERY
// shipped manifest — a withheld model an operator can still pin has to
// print its own name, not its raw id.
//
// These used to assert bundledVariantQuality / modelWithQuality, which
// rendered "<label> (quality 30)". #537 removed the figure: the tier is
// arithmetic over two catalog fields (#518) and a number labelled
// "quality" claimed a measurement. The label is what is left, so it is
// what is pinned.
func TestBundledModelLabel_ResolvesAgainstTheCompleteSet(t *testing.T) {
	t.Run("offered model", func(t *testing.T) {
		if got := bundledModelLabelDefault("qwen3.5-2b"); got != "Qwen3.5 2B" {
			t.Errorf("label = %q, want the display name with the parenthetical dropped", got)
		}
	})
	// A WITHHELD model, deliberately: internal_only keeps it off every
	// offer surface, and resolving a name somebody hands us is a
	// different question from choosing one for them.
	t.Run("withheld model still names itself", func(t *testing.T) {
		if got := bundledModelLabelDefault("granite4-350m"); got == "granite4-350m" {
			t.Errorf("label = %q, want the display name — a model this build ships "+
				"must not degrade to its raw id", got)
		}
	})
	t.Run("non-catalog id degrades to the raw id", func(t *testing.T) {
		if got := bundledModelLabelDefault("heavy"); got != "heavy" {
			t.Errorf("label = %q, want the raw id back unchanged", got)
		}
	})
}

// TestBenchmarkLinesCarryNoQualityFigure is the guard on the removal
// itself: nothing in the benchmark flow may put a bare quality number
// beside a model name again (#537). It reads the rendered prompts rather
// than the helpers, because the helpers are exactly what a reinstatement
// would route around.
func TestBenchmarkLinesCarryNoQualityFigure(t *testing.T) {
	for _, fn := range []string{"init_benchmark.go", "init_modelselect.go"} {
		src, err := os.ReadFile(fn)
		if err != nil {
			t.Fatalf("read %s: %v", fn, err)
		}
		for _, banned := range []string{"(quality %d)", "quality %d", "tier %d"} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s renders %q — the quality number is off every user-facing "+
					"surface (#537, docs/decisions/20260808/0452-model-size-class-replaces-the-quality-number.md)",
					fn, banned)
			}
		}
	}
}

// TestCanonicalBundledModelIDResolvesAnInternalModel: resolution takes
// the COMPLETE catalog, per the rule stated in init_modelselect.go — the
// agent keys its model state off the including-internal set, so an id
// resolved against the offered subset cannot be compared against
// /inference/status.
//
// It matters because a withheld model stays pinnable and resolvable by
// id or alias (see granite4-350m's internal_only note): an operator or
// the routing sentinel can pin `waired/tiny`, and the unresolved alias
// would then be waited for as a string that never appears — the exact
// failure canonicalBundledModelID's own comment says it prevents.
func TestCanonicalBundledModelIDResolvesAnInternalModel(t *testing.T) {
	const alias, canonical = "waired/tiny", "granite4-350m"
	if got := canonicalBundledModelID(alias); got != canonical {
		t.Errorf("canonicalBundledModelID(%q) = %q, want %q", alias, got, canonical)
	}
	// The offered set still resolves, and an unknown name is still kept.
	if got := canonicalBundledModelID("qwen3.6-35b"); got != "qwen3.6-35b-a3b" {
		t.Errorf("canonicalBundledModelID(qwen3.6-35b) = %q", got)
	}
	// A RETIRED alias resolves to its successor rather than being kept
	// verbatim, which is catalog.ResolveModel doing its job: a pin written
	// against an older release outlives the catalog it was written for.
	if got := canonicalBundledModelID("qwen2.5-coder-14b"); got != "qwen3.5-9b" {
		t.Errorf("canonicalBundledModelID(qwen2.5-coder-14b) = %q, want the successor qwen3.5-9b", got)
	}
	if got := canonicalBundledModelID("model-from-a-newer-catalog"); got != "model-from-a-newer-catalog" {
		t.Errorf("canonicalBundledModelID kept nothing for an unknown id: %q", got)
	}
}
