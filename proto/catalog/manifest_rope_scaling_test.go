package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// rope_scaling must serialise away when a model documents none, so a
// consumer built against an older proto tag sees a byte-identical manifest
// (additive-only proto contract, CLAUDE.md §Modules).
func TestRopeScalingIsAbsentWhenUnsetAndNamedWhenSet(t *testing.T) {
	b, err := json.Marshal(Manifest{ModelID: "m", ContextLength: 262144})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"rope_scaling"`) {
		t.Errorf("rope_scaling must be omitempty; got %s", b)
	}
	b, err = json.Marshal(Manifest{ModelID: "m", ContextLength: 262144, RopeScaling: &RopeScaling{
		Type: RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rope_scaling"`, `"type":"yarn"`, `"factor":4`, `"original_context_length":262144`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	// The publisher's ceiling is optional: a model whose card states none
	// must not serialise a 0 that reads as "the publisher said zero".
	if strings.Contains(string(b), `"publisher_max_context_length"`) {
		t.Errorf("publisher_max_context_length must be omitempty; got %s", b)
	}
}

// VariantSHA's payload is frozen (its doc comment). rope_scaling lives on the
// MODEL, so it cannot reach the hash — this test states that on purpose, so
// that anyone who later moves the scaling onto a Variant has to answer for
// every persisted measurement at once.
func TestVariantSHA_CannotSeeRopeScaling(t *testing.T) {
	v := Variant{VariantID: "q4-gguf", Format: FormatOllamaTag, Source: VariantSource{Type: SourceOllama, Tag: "qwen3.5:9b-q4_K_M"}}
	before := VariantSHA(v)
	m := Manifest{ModelID: "m", ContextLength: 262144, Variants: []Variant{v}}
	m.RopeScaling = &RopeScaling{Type: RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144}
	if got := VariantSHA(m.Variants[0]); got != before {
		t.Errorf("VariantSHA changed with the model's rope_scaling set: %s != %s", got, before)
	}
}

func TestValidateRopeScaling(t *testing.T) {
	base := func(r *RopeScaling) Manifest {
		return Manifest{ModelID: "m", ContextLength: 262144, RopeScaling: r,
			Variants: []Variant{{VariantID: "q4-gguf", Format: FormatOllamaTag, RuntimeSupport: []string{RuntimeOllama},
				QualityTier: 50, ParamCount: 9e9, QuantizationTier: 4,
				Source: VariantSource{Type: SourceOllama, Tag: "qwen3.5:9b-q4_K_M"}}}}
	}
	yarn := func(f func(*RopeScaling)) *RopeScaling {
		r := &RopeScaling{Type: RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144, PublisherMaxContextLength: 1010000}
		f(r)
		return r
	}
	for _, tc := range []struct {
		name string
		r    *RopeScaling
		ok   bool
	}{
		{"absent", nil, true},
		{"the Qwen shape", yarn(func(*RopeScaling) {}), true},
		{"no publisher ceiling stated", yarn(func(r *RopeScaling) { r.PublisherMaxContextLength = 0 }), true},
		{"unknown method", yarn(func(r *RopeScaling) { r.Type = "linear" }), false},
		{"no method", yarn(func(r *RopeScaling) { r.Type = "" }), false},
		{"factor of one buys nothing", yarn(func(r *RopeScaling) { r.Factor = 1 }), false},
		{"original above the model's own window", yarn(func(r *RopeScaling) { r.OriginalContextLength = 262145 }), false},
		{"original of zero", yarn(func(r *RopeScaling) { r.OriginalContextLength = 0 }), false},
		// A publisher's ceiling below the model's own window would mean the
		// scaling reaches nothing, and one above the factor's reach would
		// mean we transcribed the card wrong.
		{"ceiling below the model's own window", yarn(func(r *RopeScaling) { r.PublisherMaxContextLength = 200704 }), false},
		{"ceiling past what the factor reaches", yarn(func(r *RopeScaling) { r.PublisherMaxContextLength = 1048577 }), false},
		{"ceiling exactly at the factor's reach", yarn(func(r *RopeScaling) { r.PublisherMaxContextLength = 1048576 }), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base(tc.r)
			err := m.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
}

func TestExtendedContextLength(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    Manifest
		want int
	}{
		{"no scaling", Manifest{ContextLength: 262144}, 0},
		{"the Qwen shape reaches the 1M rung exactly", Manifest{ContextLength: 262144,
			RopeScaling: &RopeScaling{Type: RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144}}, 1048576},
		{"a factor that does not clear the model's own window", Manifest{ContextLength: 1048576,
			RopeScaling: &RopeScaling{Type: RopeScalingYaRN, Factor: 2, OriginalContextLength: 262144}}, 0},
		{"absurd factor", Manifest{ContextLength: 262144,
			RopeScaling: &RopeScaling{Type: RopeScalingYaRN, Factor: 1e12, OriginalContextLength: 262144}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtendedContextLength(tc.m); got != tc.want {
				t.Errorf("ExtendedContextLength = %d, want %d", got, tc.want)
			}
			if got := HasRopeScaling(tc.m); got != (tc.want > 0) {
				t.Errorf("HasRopeScaling = %v", got)
			}
		})
	}
}

// TestBundledRopeScaling is the transcription of the model cards, model by
// model, read on 2026-09-20. Its point is the EXCLUSIONS: a model gets an
// entry only where its publisher documents the extension, and three shipped
// models do not. qwen3.5-0.8b and -2b say "Context Length: 262,144 natively"
// with no "extensible up to" clause and publish no YaRN recipe; granite4-350m
// is the CI-only model at 32,768.
//
// The publisher's ceiling is NOT uniform — Qwen3.8 states 1,000,000 where
// Qwen3.5 and 3.6 state 1,010,000 — which is why it is carried per model
// rather than computed once.
func TestBundledRopeScaling(t *testing.T) {
	want := map[string]int{ // model_id -> publisher_max_context_length
		"qwen3.5-4b":         1010000,
		"qwen3.5-9b":         1010000,
		"qwen3.5-27b":        1010000,
		"qwen3.5-35b-a3b":    1010000,
		"qwen3.5-122b-a10b":  1010000,
		"qwen3.6-27b":        1010000,
		"qwen3.6-35b-a3b":    1010000,
		"qwen3.8-27b":        1000000,
		"qwen3.8-flash-next": 1000000,
	}
	withoutScaling := map[string]bool{"qwen3.5-0.8b": true, "qwen3.5-2b": true, "granite4-350m": true}

	manifests, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range manifests {
		max, documented := want[m.ModelID]
		switch {
		case documented:
			seen++
			if m.RopeScaling == nil {
				t.Errorf("%s: no rope_scaling; its card documents the extension", m.ModelID)
				continue
			}
			if m.RopeScaling.Type != RopeScalingYaRN || m.RopeScaling.Factor != 4 || m.RopeScaling.OriginalContextLength != 262144 {
				t.Errorf("%s: rope_scaling %+v, want yarn / factor 4 / original 262144", m.ModelID, *m.RopeScaling)
			}
			if m.RopeScaling.PublisherMaxContextLength != max {
				t.Errorf("%s: publisher_max_context_length = %d, want %d (its own model card)",
					m.ModelID, m.RopeScaling.PublisherMaxContextLength, max)
			}
			if got := ExtendedContextLength(m); got != 1048576 {
				t.Errorf("%s: reaches %d, want 1048576", m.ModelID, got)
			}
		case withoutScaling[m.ModelID]:
			if m.RopeScaling != nil {
				t.Errorf("%s: carries rope_scaling, but its publisher documents none — cite a card before adding one", m.ModelID)
			}
		default:
			t.Errorf("%s is in the bundled catalog but not in this table: decide whether its publisher documents a context extension, and say so here", m.ModelID)
		}
	}
	if seen != len(want) {
		t.Errorf("checked %d of %d models with documented scaling", seen, len(want))
	}
}
