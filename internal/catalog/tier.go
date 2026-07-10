package catalog

import (
	"fmt"
	"sort"

	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
)

// VariantTier is one variant's tier assignment plus the inputs behind it, so a
// reviewer can see WHY a tier landed where it did.
type VariantTier struct {
	ModelID    string  `json:"model_id"`
	VariantID  string  `json:"variant_id"`
	OldTier    int     `json:"old_tier"`
	NewTier    int     `json:"new_tier"`
	Composite  float64 `json:"composite"`
	SWEBench   float64 `json:"swe_bench_verified"`
	Confidence string  `json:"confidence,omitempty"`
	Overridden bool    `json:"overridden,omitempty"`
}

// Key is the "model_id/variant_id" identifier.
func (vt VariantTier) Key() string { return vt.ModelID + "/" + vt.VariantID }

// Changed reports whether the assignment moved the tier.
func (vt VariantTier) Changed() bool { return vt.OldTier != vt.NewTier }

// TierResult is the outcome of AssignTiers: per-variant assignments sorted by
// NewTier ascending, and whether a full re-rank was performed.
type TierResult struct {
	Assignments []VariantTier `json:"assignments"`
	Reranked    bool          `json:"reranked"`
}

// Changes returns just the assignments whose tier moved.
func (r TierResult) Changes() []VariantTier {
	var out []VariantTier
	for _, a := range r.Assignments {
		if a.Changed() {
			out = append(out, a)
		}
	}
	return out
}

// variantFootprintMB returns the memory threshold the composite penalty uses:
// min_vram_mb for GPU runtimes, else min_ram_gb×1024, else an estimate from the
// weight, else a 1 GiB floor.
func variantFootprintMB(v Variant) int {
	switch {
	case v.MinVRAMMB > 0:
		return v.MinVRAMMB
	case v.MinRAMGB > 0:
		return v.MinRAMGB * 1024
	case v.EstimatedWeightGB > 0:
		return int(v.EstimatedWeightGB * 1024)
	default:
		return 1024
	}
}

// scored is the internal working record for one variant during assignment.
type scored struct {
	vt        VariantTier
	composite float64
	override  int // 0 = none
	isNew     bool
}

// AssignTiers derives quality_tier for every variant across the catalog from
// benchmark data (#133). The composite is
// scoring.CompositeScore(total_params, swe_bench_verified, footprint).
//
// Modes:
//   - freeze (rerank=false, the default): variants that already carry a tier
//     (>0) keep it; variants with tier 0 (newly drafted) are slotted into a
//     free integer near where their composite ranks them, leaving the curated
//     ladder otherwise untouched (minimal PR diff). Returns an error only if no
//     free integer in [1,100] remains.
//   - rerank (rerank=true): every non-pinned variant is re-ranked by composite
//     and mapped onto spaced unique integers in [1,100]. Larger diff; opt-in.
//
// tier_override in benchmarks.json pins a variant's tier in both modes.
func AssignTiers(manifests []Manifest, bench BenchmarkSet, rerank bool) (TierResult, error) {
	items := buildScored(manifests, bench)
	if len(items) == 0 {
		return TierResult{}, nil
	}

	used := map[int]bool{}
	// 1) Pin overrides first.
	for i := range items {
		if items[i].override > 0 {
			if items[i].override < 1 || items[i].override > 100 {
				return TierResult{}, fmt.Errorf("catalog: %s: tier_override %d out of [1,100]", items[i].vt.Key(), items[i].override)
			}
			if used[items[i].override] {
				return TierResult{}, fmt.Errorf("catalog: tier_override %d used by more than one variant", items[i].override)
			}
			items[i].vt.NewTier = items[i].override
			items[i].vt.Overridden = true
			used[items[i].override] = true
		}
	}

	var err error
	if rerank {
		err = assignRerank(items, used)
	} else {
		err = assignFreeze(items, used)
	}
	if err != nil {
		return TierResult{}, err
	}

	out := make([]VariantTier, 0, len(items))
	for _, it := range items {
		out = append(out, it.vt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NewTier < out[j].NewTier })
	return TierResult{Assignments: out, Reranked: rerank}, nil
}

func buildScored(manifests []Manifest, bench BenchmarkSet) []scored {
	var items []scored
	for _, m := range manifests {
		mb, hasBench := bench.Models[m.ModelID]
		for _, v := range m.Variants {
			swe := 0.0
			conf := ""
			override := 0
			if hasBench {
				swe = mb.SWEBenchVerified
				conf = mb.Confidence
				if vb, ok := mb.Variants[v.VariantID]; ok {
					override = vb.TierOverride
				}
			}
			comp := scoring.CompositeScore(v.ParamCount, swe, variantFootprintMB(v))
			items = append(items, scored{
				vt: VariantTier{
					ModelID: m.ModelID, VariantID: v.VariantID,
					OldTier: v.QualityTier, Composite: comp,
					SWEBench: swe, Confidence: conf,
				},
				composite: comp,
				override:  override,
				isNew:     v.QualityTier == 0,
			})
		}
	}
	return items
}

// assignFreeze keeps existing tiers and slots tier-0 (new) variants in.
func assignFreeze(items []scored, used map[int]bool) error {
	// Anchored = variants with a known final tier (override or existing >0).
	for i := range items {
		if items[i].vt.Overridden {
			continue
		}
		if !items[i].isNew {
			items[i].vt.NewTier = items[i].vt.OldTier
			used[items[i].vt.OldTier] = true
		}
	}
	// Build the anchored list sorted by composite for neighbour lookup.
	type anchor struct {
		composite float64
		tier      int
	}
	var anchors []anchor
	for i := range items {
		if !items[i].isNew {
			anchors = append(anchors, anchor{items[i].composite, items[i].vt.NewTier})
		}
	}
	sort.Slice(anchors, func(a, b int) bool { return anchors[a].composite < anchors[b].composite })

	// Assign new variants in composite order so several inserted together
	// spread out rather than all grabbing the same midpoint.
	var newIdx []int
	for i := range items {
		if items[i].isNew && !items[i].vt.Overridden {
			newIdx = append(newIdx, i)
		}
	}
	sort.Slice(newIdx, func(a, b int) bool { return items[newIdx[a]].composite < items[newIdx[b]].composite })

	for _, i := range newIdx {
		comp := items[i].composite
		lo, hi := 0, 101
		for _, a := range anchors {
			if a.composite <= comp && a.tier > lo {
				lo = a.tier
			}
		}
		for _, a := range anchors {
			if a.composite > comp && a.tier < hi {
				hi = a.tier
			}
		}
		// Normalise an inverted ladder (existing tiers not monotonic in
		// composite): just bound the search by the two neighbour tiers.
		if lo > hi {
			lo, hi = hi, lo
		}
		tier, ok := freeTier(used, lo, hi)
		if !ok {
			return fmt.Errorf("catalog: %s: no free quality_tier between %d and %d; run with rerank to renumber", items[i].vt.Key(), lo, hi)
		}
		items[i].vt.NewTier = tier
		used[tier] = true
		anchors = append(anchors, anchor{comp, tier})
		sort.Slice(anchors, func(a, b int) bool { return anchors[a].composite < anchors[b].composite })
	}
	return nil
}

// freeTier returns a free integer strictly inside (lo, hi), preferring the
// midpoint and searching outward; if the open interval is exhausted it falls
// back to ANY free integer in [1,100]. ok=false only when [1,100] is full.
func freeTier(used map[int]bool, lo, hi int) (int, bool) {
	mid := (lo + hi) / 2
	for d := 0; d <= 100; d++ {
		for _, cand := range []int{mid - d, mid + d} {
			if cand > lo && cand < hi && cand >= 1 && cand <= 100 && !used[cand] {
				return cand, true
			}
		}
	}
	for t := 1; t <= 100; t++ {
		if !used[t] {
			return t, true
		}
	}
	return 0, false
}

// assignRerank re-ranks every non-pinned variant by composite and spreads them
// across the free integers in [1,100].
func assignRerank(items []scored, used map[int]bool) error {
	var idx []int
	for i := range items {
		if !items[i].vt.Overridden {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool { return items[idx[a]].composite < items[idx[b]].composite })

	var free []int
	for t := 1; t <= 100; t++ {
		if !used[t] {
			free = append(free, t)
		}
	}
	m := len(idx)
	if m > len(free) {
		return fmt.Errorf("catalog: %d variants but only %d free quality_tier slots in [1,100]", m, len(free))
	}
	for rank, i := range idx {
		var pos int
		if m == 1 {
			pos = len(free) / 2
		} else {
			// Spread evenly across the free integers (distinct indices since
			// len(free) >= m), so tiers don't cluster at the low end.
			pos = rank * (len(free) - 1) / (m - 1)
		}
		items[i].vt.NewTier = free[pos]
		used[free[pos]] = true
	}
	return nil
}
