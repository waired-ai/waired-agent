package catalog

import (
	"fmt"
	"sort"
)

// CheckTierUniqueness verifies that every variant across the whole set of
// manifests carries a quality_tier in [1, 100] and that no two variants share
// the same tier. quality_tier is the model picker's primary, unambiguous
// ordering key (manifest.go: RankModels sorts by it descending), so a
// collision makes the ranking order-dependent — exactly the silent
// mis-selection this guards against.
//
// This is a CATALOG-LEVEL invariant (Manifest.Validate only sees one manifest
// at a time and so cannot detect cross-manifest collisions). It is the shared
// implementation behind the bundled-catalog test and `catalog-tool validate`.
func CheckTierUniqueness(manifests []Manifest) error {
	type owner struct {
		key  string
		tier int
	}
	seen := map[int]string{}
	var dupes []owner
	for _, m := range manifests {
		for _, v := range m.Variants {
			key := m.ModelID + "/" + v.VariantID
			if v.QualityTier < 1 || v.QualityTier > 100 {
				return fmt.Errorf("catalog: %s: quality_tier = %d, want in [1, 100]", key, v.QualityTier)
			}
			if prev, ok := seen[v.QualityTier]; ok {
				dupes = append(dupes, owner{key: fmt.Sprintf("%s and %s", prev, key), tier: v.QualityTier})
				continue
			}
			seen[v.QualityTier] = key
		}
	}
	if len(dupes) > 0 {
		// Deterministic message: sort by tier so the error is stable.
		sort.Slice(dupes, func(i, j int) bool { return dupes[i].tier < dupes[j].tier })
		return fmt.Errorf("catalog: duplicate quality_tier %d (%s)", dupes[0].tier, dupes[0].key)
	}
	return nil
}
