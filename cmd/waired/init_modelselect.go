package main

import (
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
)

// bundledModelLabel returns a short human-facing label for a bundled model
// id/alias — the display name with any trailing parenthetical dropped (e.g.
// "Qwen3.5 0.8B (Hybrid Linear+Full Attention)" → "Qwen3.5 0.8B"), else the id.
func bundledModelLabel(manifests []catalog.Manifest, modelID string) string {
	if m, ok := catalog.LookupByAlias(modelID, manifests); ok && m.DisplayName != "" {
		return strings.TrimSpace(strings.SplitN(m.DisplayName, " (", 2)[0])
	}
	return modelID
}

// bundledModelLabelDefault is bundledModelLabel over the embedded catalog,
// falling back to the raw id when the catalog is unreadable.
func bundledModelLabelDefault(modelID string) string {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return modelID
	}
	return bundledModelLabel(manifests, modelID)
}

// bundledVariantQuality resolves the catalog quality tier (1–100) for
// modelID's variantID, falling back to the model's best variant when
// variantID is empty or not found (the recommendation may name a variant the
// local catalog build doesn't carry). ok is false when the embedded catalog
// is unreadable or modelID is unknown.
func bundledVariantQuality(modelID, variantID string) (int, bool) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return 0, false
	}
	m, ok := catalog.LookupByAlias(modelID, manifests)
	if !ok {
		return 0, false
	}
	best := 0
	for _, v := range m.Variants {
		if variantID != "" && v.VariantID == variantID && v.QualityTier > 0 {
			return v.QualityTier, true
		}
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	if best > 0 {
		return best, true
	}
	return 0, false
}

// modelWithQuality renders "<label> (quality N)" for benchmark and
// recommendation lines, so the user can weigh the speed/quality trade-off of
// a switch (waired#773). Degrades to the bare label when the tier is unknown
// and to the raw id when the catalog can't resolve the model.
func modelWithQuality(modelID, variantID string) string {
	label := bundledModelLabelDefault(modelID)
	if q, ok := bundledVariantQuality(modelID, variantID); ok {
		return fmt.Sprintf("%s (quality %d)", label, q)
	}
	return label
}

// isBundledModelBelowFloor reports whether modelID (id or alias) resolves to a
// bundled model whose best variant sits below the install quality floor — the
// "very low quality, not recommended for local use" tier (today the 0.5B).
// Best-effort: false when the catalog is unreadable or the id is unknown.
func isBundledModelBelowFloor(modelID string) bool {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return false
	}
	m, ok := catalog.LookupByAlias(modelID, manifests)
	if !ok {
		return false
	}
	best := 0
	for _, v := range m.Variants {
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	return best > 0 && best < router.InstallQualityFloorTier
}
