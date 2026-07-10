package catalog

import "testing"

func TestBenchmarks_Loads(t *testing.T) {
	bs, err := Benchmarks()
	if err != nil {
		t.Fatalf("Benchmarks: %v", err)
	}
	if bs.Schema != 1 {
		t.Errorf("schema = %d, want 1", bs.Schema)
	}
	if len(bs.Models) == 0 {
		t.Fatal("benchmarks.json has no models")
	}
}

// TestBenchmarks_ResolveAndProvenance enforces that every benchmark entry names
// a real catalog model and carries reviewable provenance — the discipline the
// whole #133 derivation rests on.
func TestBenchmarks_ResolveAndProvenance(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	bs, err := Benchmarks()
	if err != nil {
		t.Fatalf("Benchmarks: %v", err)
	}
	for id, mb := range bs.Models {
		if _, ok := LookupByAlias(id, ms); !ok {
			t.Errorf("benchmarks model_id %q does not resolve to a bundled manifest", id)
		}
		if mb.SWEBenchVerified < 0 || mb.SWEBenchVerified > 100 {
			t.Errorf("%s: swe_bench_verified = %v, want [0,100]", id, mb.SWEBenchVerified)
		}
		switch mb.Confidence {
		case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		default:
			t.Errorf("%s: confidence = %q, want high|medium|low", id, mb.Confidence)
		}
		if len(mb.Sources) == 0 {
			t.Errorf("%s: must carry at least one source", id)
		}
		for i, s := range mb.Sources {
			if s.URL == "" || s.Retrieved == "" {
				t.Errorf("%s: source[%d] missing url/retrieved: %+v", id, i, s)
			}
		}
	}
}

func TestAssignTiers_FreezeOnBundledIsNoOp(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()
	res, err := AssignTiers(ms, bs, false)
	if err != nil {
		t.Fatalf("AssignTiers freeze: %v", err)
	}
	// Every bundled variant already has a tier, so freeze must not move any.
	if changed := res.Changes(); len(changed) != 0 {
		t.Errorf("freeze moved %d existing tiers; want 0: %+v", len(changed), changed)
	}
	assertUniqueTiers(t, res)
}

func TestAssignTiers_FreezeSlotsNewVariant(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()
	existing := tierSet(t, ms)

	// A new model with an unset (0) tier — what `draft` emits before tiering.
	newM := Manifest{
		ModelID: "brand-new-coder-32b", ContextLength: 65536,
		Variants: []Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag",
			RuntimeSupport: []string{"ollama"}, QualityTier: 0,
			ParamCount: 32_000_000_000, QuantizationTier: 4, MinRAMGB: 24,
			Source: VariantSource{Type: "ollama", Tag: "brand-new-coder:32b-q4_K_M"},
		}},
	}
	res, err := AssignTiers(append(ms, newM), bs, false)
	if err != nil {
		t.Fatalf("AssignTiers freeze with new variant: %v", err)
	}
	assertUniqueTiers(t, res)
	var got int
	for _, a := range res.Assignments {
		if a.ModelID == "brand-new-coder-32b" {
			got = a.NewTier
		}
	}
	if got < 1 || got > 100 {
		t.Fatalf("new variant tier = %d, want [1,100]", got)
	}
	if existing[got] {
		t.Errorf("new variant tier %d collides with an existing tier", got)
	}
}

func TestAssignTiers_RerankUniqueDeterministicDirectional(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()

	a, err := AssignTiers(ms, bs, true)
	if err != nil {
		t.Fatalf("AssignTiers rerank: %v", err)
	}
	assertUniqueTiers(t, a)

	// Determinism: a second run yields identical assignments.
	b, _ := AssignTiers(ms, bs, true)
	if len(a.Assignments) != len(b.Assignments) {
		t.Fatal("rerank not deterministic (length)")
	}
	am, bm := tierMap(a), tierMap(b)
	for k, v := range am {
		if bm[k] != v {
			t.Errorf("rerank not deterministic: %s = %d vs %d", k, v, bm[k])
		}
	}

	// Directional: qwen3.6-27b (SWE 77.2) outranks qwen2.5-coder-7b (no SWE).
	if maxTier(a, "qwen3.6-27b") <= maxTier(a, "qwen2.5-coder-7b-instruct") {
		t.Errorf("expected qwen3.6-27b tier (%d) > qwen2.5-coder-7b tier (%d)",
			maxTier(a, "qwen3.6-27b"), maxTier(a, "qwen2.5-coder-7b-instruct"))
	}
}

func TestAssignTiers_OverrideHonored(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()
	// Pin qwen3.6-27b/awq-int4 to tier 7 via an override.
	mb := bs.Models["qwen3.6-27b"]
	mb.Variants = map[string]VariantBenchmrk{"awq-int4": {TierOverride: 7}}
	bs.Models["qwen3.6-27b"] = mb

	res, err := AssignTiers(ms, bs, true)
	if err != nil {
		t.Fatalf("AssignTiers: %v", err)
	}
	assertUniqueTiers(t, res)
	for _, a := range res.Assignments {
		if a.ModelID == "qwen3.6-27b" && a.VariantID == "awq-int4" {
			if a.NewTier != 7 || !a.Overridden {
				t.Errorf("override not honored: tier=%d overridden=%v", a.NewTier, a.Overridden)
			}
			return
		}
	}
	t.Fatal("qwen3.6-27b/awq-int4 not in assignments")
}

// --- helpers ---

func assertUniqueTiers(t *testing.T, r TierResult) {
	t.Helper()
	seen := map[int]string{}
	for _, a := range r.Assignments {
		if a.NewTier < 1 || a.NewTier > 100 {
			t.Errorf("%s: tier %d out of [1,100]", a.Key(), a.NewTier)
		}
		if prev, ok := seen[a.NewTier]; ok {
			t.Errorf("tier %d shared by %s and %s", a.NewTier, prev, a.Key())
		}
		seen[a.NewTier] = a.Key()
	}
}

func tierMap(r TierResult) map[string]int {
	m := map[string]int{}
	for _, a := range r.Assignments {
		m[a.Key()] = a.NewTier
	}
	return m
}

func tierSet(t *testing.T, ms []Manifest) map[int]bool {
	t.Helper()
	s := map[int]bool{}
	for _, m := range ms {
		for _, v := range m.Variants {
			s[v.QualityTier] = true
		}
	}
	return s
}

func maxTier(r TierResult, modelID string) int {
	mt := 0
	for _, a := range r.Assignments {
		if a.ModelID == modelID && a.NewTier > mt {
			mt = a.NewTier
		}
	}
	return mt
}
