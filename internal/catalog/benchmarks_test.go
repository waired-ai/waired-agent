package catalog

import (
	"strings"
	"testing"
)

func TestBenchmarks_Loads(t *testing.T) {
	bs, err := Benchmarks()
	if err != nil {
		t.Fatalf("Benchmarks: %v", err)
	}
	if bs.Schema != BenchmarkSchema {
		t.Errorf("schema = %d, want %d", bs.Schema, BenchmarkSchema)
	}
	if len(bs.Models) == 0 {
		t.Fatal("benchmarks.json has no models")
	}
	if len(bs.AcceptedSources) == 0 {
		t.Fatal("benchmarks.json declares no accepted sources, so no override could ever cite one")
	}
	for _, a := range bs.AcceptedSources {
		if a.ID == "" || a.URL == "" || a.LastUpdated == "" {
			t.Errorf("accepted source %+v: id, url and last_updated are all required", a)
		}
	}
}

// Product contract, docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md:
// every score carries where it came from and when it was read, and a score that
// backs a placement names an accepted source.
func TestBenchmarks_ScoreProvenance(t *testing.T) {
	bs, err := Benchmarks()
	if err != nil {
		t.Fatalf("Benchmarks: %v", err)
	}
	for id, mb := range bs.Models {
		for key, sc := range mb.Scores {
			if sc.URL == "" || sc.Retrieved == "" {
				t.Errorf("%s/%s: url and retrieved are required, got %+v", id, key, sc)
			}
			if sc.Value < 0 {
				t.Errorf("%s/%s: value = %v, want >= 0", id, key, sc.Value)
			}
		}
	}
}

// The scoring path looks a row up by EXACT model_id (buildScored), while the
// old provenance test resolved keys with LookupByAlias — so a row keyed by an
// alias passed that test and was invisible to tier assignment. Product
// contract, same decision record.
func TestBenchmarkKeysAreExactModelIDs(t *testing.T) {
	ms, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	exact := map[string]bool{}
	for _, m := range ms {
		exact[m.ModelID] = true
	}
	bs, _ := Benchmarks()
	for id := range bs.Models {
		if exact[id] {
			continue
		}
		if _, ok := LookupByAlias(id, ms); ok {
			t.Errorf("benchmarks key %q is an ALIAS, not a model_id — tier assignment looks rows up by "+
				"exact model_id, so this row is invisible to it", id)
			continue
		}
		t.Errorf("benchmarks key %q does not name a bundled model", id)
	}
}

// The guard, over the shipped store. Every override justifies itself.
func TestBenchmarkEvidence_ShippedStore(t *testing.T) {
	ms, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	bs, _ := Benchmarks()
	for _, iss := range bs.CheckEvidence(ms) {
		t.Errorf("%s: %s", iss.Key, iss.Reason)
	}
}

// The A/B that shows CheckEvidence guards anything. Each case differs from a
// passing one by exactly the property under test.
func TestCheckEvidence_Table(t *testing.T) {
	manifests := []Manifest{{
		ModelID:      "m",
		ModelAliases: []string{"waired/m"},
		Variants:     []Variant{{VariantID: "v"}, {VariantID: "w"}},
	}}
	good := BenchmarkScore{Value: 1, Source: "ok", URL: "u", Retrieved: "2026-01-01"}
	unsourced := BenchmarkScore{Value: 1, URL: "u", Retrieved: "2026-01-01"}
	fromRejected := BenchmarkScore{Value: 1, Source: "stale", URL: "u", Retrieved: "2026-01-01"}

	for _, tc := range []struct {
		name string
		set  BenchmarkSet
		want string // substring; "" = no issue
	}{
		{"override with reason and accepted evidence",
			set(mb(scores{"s": good}, overrides{"v": {TierOverride: 9, Reason: "why", Evidence: []string{"s"}}})), ""},
		{"no override at all",
			set(mb(scores{"s": good}, nil)), ""},
		{"override without a reason",
			set(mb(scores{"s": good}, overrides{"v": {TierOverride: 9, Evidence: []string{"s"}}})), "without a reason"},
		{"override without evidence",
			set(mb(scores{"s": good}, overrides{"v": {TierOverride: 9, Reason: "why"}})), "without evidence"},
		{"evidence names a score that does not exist",
			set(mb(scores{"s": good}, overrides{"v": {TierOverride: 9, Reason: "why", Evidence: []string{"nope"}}})), "not a key"},
		{"evidence names an unsourced score",
			set(mb(scores{"s": unsourced}, overrides{"v": {TierOverride: 9, Reason: "why", Evidence: []string{"s"}}})), "names no source"},
		{"evidence cites a source we do not accept",
			set(mb(scores{"s": fromRejected}, overrides{"v": {TierOverride: 9, Reason: "why", Evidence: []string{"s"}}})), "not an accepted source"},
		{"row for a variant the manifest does not have",
			set(mb(scores{"s": good}, overrides{"x": {TierOverride: 9, Reason: "why", Evidence: []string{"s"}}})), "no such variant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := tc.set.CheckEvidence(manifests)
			joined := ""
			for _, i := range issues {
				joined += i.Key + ": " + i.Reason + "\n"
			}
			switch {
			case tc.want == "" && len(issues) != 0:
				t.Errorf("want no issue, got:\n%s", joined)
			case tc.want != "" && !strings.Contains(joined, tc.want):
				t.Errorf("want an issue containing %q, got:\n%s", tc.want, joined)
			}
		})
	}

	// An alias-keyed row is reported rather than silently ignored — the case
	// the shipped-store test above exists to prevent.
	aliased := set(mb(scores{"s": good}, nil))
	aliased.Models["waired/m"] = aliased.Models["m"]
	delete(aliased.Models, "m")
	if issues := aliased.CheckEvidence(manifests); len(issues) == 0 {
		t.Error("an alias-keyed row must be reported; tier assignment cannot see it")
	}
}

type scores map[string]BenchmarkScore
type overrides map[string]VariantBenchmark

func mb(sc scores, ov overrides) ModelBenchmarks {
	return ModelBenchmarks{Scores: sc, Variants: ov}
}

func set(m ModelBenchmarks) BenchmarkSet {
	return BenchmarkSet{
		Schema:          BenchmarkSchema,
		AcceptedSources: []AcceptedSource{{ID: "ok", URL: "u", LastUpdated: "2026-01-01"}},
		Models:          map[string]ModelBenchmarks{"m": m},
	}
}

// Product contract, same decision record: this PR must not move a tier.
//
// The invariance is structural rather than lucky. assignFreeze consults the
// composite in two places and both are gated on isNew (QualityTier == 0);
// every bundled variant carries a nonzero tier (the minimum is granite4-350m
// at 11), so the new-variant loop never runs and no composite value is read.
// The only benchmark-derived input that can move a bundled tier under freeze
// is tier_override, and each of those equals its committed tier.
//
// Adding a tier-0 variant to proto/catalog/bundled/ would end that argument,
// and this test would then be asserting something weaker than it reads.
func TestAssignTiers_FreezeOnBundledIsNoOp(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()
	res, err := AssignTiers(ms, bs, false)
	if err != nil {
		t.Fatalf("AssignTiers freeze: %v", err)
	}
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

// Rerank is a diagnostic, not a planned step (the decision record above): the
// committed ladder IS the curation, so there is nothing to re-derive it from.
// What is pinned here is that the tool still produces a usable answer —
// unique, in range, and the same one twice.
//
// The directional assertion this test used to carry ("qwen3.6-27b outranks
// qwen2.5-coder-7b because it has SWE-bench 77.2 and the other has 0") is
// gone: it depended on the missing-means-zero defect this PR removes.
func TestAssignTiers_RerankUniqueAndDeterministic(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()

	a, err := AssignTiers(ms, bs, true)
	if err != nil {
		t.Fatalf("AssignTiers rerank: %v", err)
	}
	assertUniqueTiers(t, a)

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
}

func TestAssignTiers_OverrideHonored(t *testing.T) {
	ms, _ := BundledManifests()
	bs, _ := Benchmarks()
	// Pin qwen3.6-27b/awq-int4 to tier 7 via an override.
	mbench := bs.Models["qwen3.6-27b"]
	mbench.Variants = map[string]VariantBenchmark{
		"awq-int4": {TierOverride: 7, Reason: "fixture"},
	}
	bs.Models["qwen3.6-27b"] = mbench

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
			if a.OverrideReason != "fixture" {
				t.Errorf("override reason = %q, want it carried into the report", a.OverrideReason)
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
