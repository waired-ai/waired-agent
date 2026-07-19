// Package catalog manages model manifests and the local cache state.
//
// The manifest schema, the bundled catalog data, and the quality-tier
// resolver moved to the shared proto module (proto/catalog) so the
// private control plane can consume the same single source of truth
// (Public Share matchmaking / usage ingest). This package re-exports
// those types unchanged — existing importers keep compiling against
// catalog.Manifest etc. — and keeps everything runtime-behavioural:
// the local State (state.json, see local.go) tracking which model
// variants are present / downloading / failed, engine discovery, and
// the maintainer-side tier re-ranking tooling (tier.go, benchmarks.go).
package catalog

import (
	"strings"

	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

// Re-exported manifest schema — see proto/catalog for documentation.
type (
	Manifest             = protocatalog.Manifest
	RuntimePolicy        = protocatalog.RuntimePolicy
	Variant              = protocatalog.Variant
	VendorSupportMatrix  = protocatalog.VendorSupportMatrix
	VendorRuntimeSupport = protocatalog.VendorRuntimeSupport
	VariantSource        = protocatalog.VariantSource
	Security             = protocatalog.Security
)

// Re-exported identifier constants — see proto/catalog.
const (
	SourceHuggingFace = protocatalog.SourceHuggingFace
	SourceOllama      = protocatalog.SourceOllama

	RuntimeOllama = protocatalog.RuntimeOllama
	RuntimeVLLM   = protocatalog.RuntimeVLLM

	FormatSafetensors = protocatalog.FormatSafetensors
	FormatGGUF        = protocatalog.FormatGGUF
	FormatOllamaTag   = protocatalog.FormatOllamaTag

	AttentionStandard      = protocatalog.AttentionStandard
	AttentionGQA           = protocatalog.AttentionGQA
	AttentionMLA           = protocatalog.AttentionMLA
	AttentionHybridMamba   = protocatalog.AttentionHybridMamba
	AttentionSlidingWindow = protocatalog.AttentionSlidingWindow

	VendorSupportStable       = protocatalog.VendorSupportStable
	VendorSupportExperimental = protocatalog.VendorSupportExperimental
	VendorSupportCommunity    = protocatalog.VendorSupportCommunity
	VendorSupportUnsupported  = protocatalog.VendorSupportUnsupported
)

// BundledManifests decodes the catalog embedded in proto/catalog/bundled.
func BundledManifests() ([]Manifest, error) {
	return protocatalog.BundledManifests()
}

// LookupByAlias finds the first manifest whose ModelID equals name or
// whose ModelAliases contains name. Empty `name` always misses.
func LookupByAlias(name string, manifests []Manifest) (Manifest, bool) {
	return protocatalog.LookupByAlias(name, manifests)
}

// isAWQ mirrors the (unexported) proto/catalog helper for this
// package's bundled-catalog invariant tests.
func isAWQ(q string) bool {
	return strings.Contains(strings.ToUpper(q), "AWQ")
}

// contains is a small helper used by tests.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
