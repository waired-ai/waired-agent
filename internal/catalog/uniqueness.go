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

// CheckNameUniqueness verifies that no model_id or alias is claimed by
// two manifests.
//
// LookupByAlias is a first-match scan over the manifests in
// bundledFS.ReadDir order — alphabetical by filename — and checks
// ModelID before ModelAliases. A name claimed twice therefore resolves
// to whichever file sorts first, silently, with no diagnostic anywhere:
// the loser is simply unreachable by that name while still appearing in
// every catalog listing. That is the same shape of order-dependent
// mis-selection CheckTierUniqueness exists to prevent, and until #521
// moved a dozen aliases at once nothing checked for it.
//
// Like the tier check this is a CATALOG-LEVEL invariant that
// Manifest.Validate cannot see, and it is shared by the bundled-catalog
// test and `catalog-tool validate`.
func CheckNameUniqueness(manifests []Manifest) error {
	type claim struct{ name, by string }
	seen := map[string]string{}
	var dupes []claim
	for _, m := range manifests {
		for _, name := range append([]string{m.ModelID}, m.ModelAliases...) {
			// A manifest repeating its own id in model_aliases is a
			// self-claim, not a collision — most of the bundled set
			// does it, and LookupByAlias checks ModelID first anyway.
			if prev, ok := seen[name]; ok && prev != m.ModelID {
				dupes = append(dupes, claim{name: name, by: fmt.Sprintf("%s and %s", prev, m.ModelID)})
				continue
			}
			seen[name] = m.ModelID
		}
	}
	if len(dupes) > 0 {
		sort.Slice(dupes, func(i, j int) bool { return dupes[i].name < dupes[j].name })
		return fmt.Errorf("catalog: %q is claimed by two manifests (%s); "+
			"LookupByAlias would answer with whichever file sorts first", dupes[0].name, dupes[0].by)
	}
	return nil
}
