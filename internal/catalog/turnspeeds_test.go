package catalog

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestTurnSpeedsDecodes(t *testing.T) {
	set, err := TurnSpeeds()
	if err != nil {
		t.Fatalf("TurnSpeeds: %v", err)
	}
	if set.Schema != 1 {
		t.Errorf("schema = %d, want 1", set.Schema)
	}
	if set.Notes == "" {
		t.Error("the store should say what a record means and who writes it")
	}
	if !ValidHostClass(set.HostClass) {
		t.Errorf("host_class %q is not a known hardware class", set.HostClass)
	}
}

// A key with no field is dropped on the next import, and a field with no
// key is a promise nothing keeps.
func TestTurnSpeedJSONHasNoUnknownFields(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(turnSpeedJSON))
	dec.DisallowUnknownFields()
	var set TurnSpeedSet
	if err := dec.Decode(&set); err != nil {
		t.Fatalf("turnspeeds.json carries a field the struct does not declare: %v", err)
	}
}

func TestTurnSpeedEntriesCarryProvenance(t *testing.T) {
	set, err := TurnSpeeds()
	if err != nil {
		t.Fatalf("TurnSpeeds: %v", err)
	}
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			where := modelID + "/" + variantID
			if !variantExists(all, modelID, variantID) {
				t.Errorf("%s names no shipped variant", where)
			}
			if rec.VariantSHA == "" || rec.Engine == "" || rec.EngineVersion == "" || rec.TurnSeconds <= 0 {
				t.Errorf("%s: variant_sha, engine, engine_version and turn_seconds are all required", where)
			}
			if !retrievedDate.MatchString(rec.Retrieved) {
				t.Errorf("%s: retrieved %q is not YYYY-MM-DD", where, rec.Retrieved)
			}
			switch rec.Method {
			case TurnSpeedMeasured:
				// Repeated runs, at the product's own depth and window
				// (owner, 2026-09-16).
				if rec.Samples < 3 {
					t.Errorf("%s: a measured record needs at least 3 samples, has %d", where, rec.Samples)
				}
				if rec.DepthTokens < 32768 || rec.AppliedWindow < 200704 {
					t.Errorf("%s: depth %d / window %d is not the product's measurement", where, rec.DepthTokens, rec.AppliedWindow)
				}
				// Record of today's store (#1400): seconds are compared
				// only between records taken under the same engine
				// flags, so a measured ollama record says which ones.
				// num_parallel is the product's request, not what the
				// engine ran (#1423).
				if rec.Engine == RuntimeOllama && rec.EngineFlags == "" {
					t.Errorf("%s: a measured ollama record needs engine_flags", where)
				}
			case TurnSpeedEstimated:
				if rec.Notes == "" {
					t.Errorf("%s: an estimate must say how it was computed", where)
				}
			default:
				t.Errorf("%s: method %q is neither measured nor estimate", where, rec.Method)
			}
		}
	}
}

// Product contract: the lookup order is decision 2 of
// docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md
// (owner, 2026-09-16) — the variant's own measurement, then the model's
// default ollama variant's, then an estimate.
func TestTurnSpeedSet_ForFollowsTheOwnersOrder(t *testing.T) {
	q4 := Variant{VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
		RuntimeSupport: []string{RuntimeOllama}, Source: VariantSource{Type: "ollama", Tag: "m:q4"}}
	fp8 := Variant{VariantID: "fp8", Format: "safetensors", Quantization: "FP8",
		RuntimeSupport: []string{RuntimeVLLM}, Source: VariantSource{Type: "huggingface", RepoID: "org/m-fp8"}}
	q2 := Variant{VariantID: "q2-gguf", Format: "ollama-tag", Quantization: "UD-Q2_K_XL",
		RuntimeSupport: []string{RuntimeOllama}, Source: VariantSource{Type: "ollama", Tag: "m:q2"}}
	m := Manifest{ModelID: "m", Variants: []Variant{q4, fp8, q2},
		DefaultVariant: map[string]string{RuntimeOllama: "q4-gguf", RuntimeVLLM: "fp8"}}
	rec := func(v Variant, method string, s float64) VariantTurnSpeed {
		return VariantTurnSpeed{VariantSHA: VariantSHA(v), Method: method, TurnSeconds: s}
	}
	set := TurnSpeedSet{Models: map[string]ModelTurnSpeeds{"m": {Variants: map[string]VariantTurnSpeed{
		"q4-gguf": rec(q4, TurnSpeedMeasured, 100),
		"fp8":     rec(fp8, TurnSpeedEstimated, 80),
	}}}}

	for _, c := range []struct {
		v    Variant
		want float64
		src  TurnSpeedSource
		ok   bool
	}{
		{q4, 100, FromOwnMeasurement, true},
		// fp8 has only an estimate, and the ollama default is measured:
		// the measured sibling comes before the estimate.
		{fp8, 100, FromOllamaDefault, true},
		{q2, 100, FromOllamaDefault, true},
	} {
		got, src, ok := set.For(m, c.v)
		if got != c.want || src != c.src || ok != c.ok {
			t.Errorf("%s: For = (%v, %q, %v), want (%v, %q, %v)", c.v.VariantID, got, src, ok, c.want, c.src, c.ok)
		}
	}

	// Without the measured default, the estimate answers.
	delete(set.Models["m"].Variants, "q4-gguf")
	if got, src, ok := set.For(m, fp8); got != 80 || src != FromEstimate || !ok {
		t.Errorf("fp8 without a measured sibling: For = (%v, %q, %v), want the estimate", got, src, ok)
	}
	// A record for another build of the same ids answers nothing.
	stale := q2
	stale.Source.Tag = "m:q2-new"
	set.Models["m"].Variants["q2-gguf"] = rec(q2, TurnSpeedMeasured, 60)
	if _, _, ok := set.For(m, stale); ok {
		t.Error("a record whose variant_sha no longer matches answered for the new build")
	}
}

func TestTurnSpeedGaps_ListsOllamaVariantsOnly(t *testing.T) {
	q4 := Variant{VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{RuntimeOllama},
		Source: VariantSource{Type: "ollama", Tag: "m:q4"}}
	fp8 := Variant{VariantID: "fp8", Format: "safetensors", RuntimeSupport: []string{RuntimeVLLM},
		Source: VariantSource{Type: "huggingface", RepoID: "org/m"}}
	ms := []Manifest{{ModelID: "m", Variants: []Variant{q4, fp8}}}
	if got := TurnSpeedGaps(TurnSpeedSet{}, ms); len(got) != 1 || got[0] != "m/q4-gguf" {
		t.Errorf("gaps = %v, want [m/q4-gguf]", got)
	}
}
