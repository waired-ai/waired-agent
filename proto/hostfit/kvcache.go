package hostfit

import "github.com/waired-ai/waired-agent/proto/catalog"

// OllamaDefaultKVCacheType is the KV-cache type the serve tuning exports
// when nothing pins one. The recommendation and the capacity gate price
// the cache with it, because the recommendation reads the type the tuning
// actually serves (decision 1 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
// It is q8_0 today; waired-ai/waired-agent#1348 moves the default to q4_0,
// and that change belongs here and in the tuning together.
//
// The host is a parameter although nothing reads it yet: the default
// ladder (#1348) depends on the hardware, and the signature is published.
func OllamaDefaultKVCacheType(_ Host) string {
	return catalog.KVCacheQ8_0
}
