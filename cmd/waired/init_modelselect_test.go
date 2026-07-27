package main

import (
	"testing"
)

// bundledVariantQuality / modelWithQuality resolve catalog quality tiers for
// the benchmark display lines (waired#773).
func TestBundledVariantQuality(t *testing.T) {
	t.Run("known-variant", func(t *testing.T) {
		q, ok := bundledVariantQuality("qwen2.5-coder-3b-instruct", "q4-gguf")
		if !ok || q != 30 {
			t.Errorf("quality = %d ok=%v, want 30 true", q, ok)
		}
	})
	t.Run("unknown-variant-falls-back-to-best", func(t *testing.T) {
		q, ok := bundledVariantQuality("qwen2.5-coder-3b-instruct", "no-such-variant")
		if !ok || q != 31 { // best variant (awq-int4) of the 3B
			t.Errorf("quality = %d ok=%v, want best-variant 31 true", q, ok)
		}
	})
	t.Run("empty-variant-falls-back-to-best", func(t *testing.T) {
		q, ok := bundledVariantQuality("qwen2.5-coder-0.5b-instruct", "")
		if !ok || q != 10 {
			t.Errorf("quality = %d ok=%v, want 10 true", q, ok)
		}
	})
	t.Run("unknown-model", func(t *testing.T) {
		if _, ok := bundledVariantQuality("no-such-model", "q4"); ok {
			t.Errorf("unknown model must not resolve a quality tier")
		}
	})
}

func TestModelWithQuality(t *testing.T) {
	t.Run("catalog-model-renders-label-and-tier", func(t *testing.T) {
		got := modelWithQuality("qwen2.5-coder-3b-instruct", "q4-gguf")
		if got != "Qwen2.5 Coder 3B Instruct (quality 30)" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("non-catalog-id-degrades-to-raw-id", func(t *testing.T) {
		if got := modelWithQuality("heavy", "q4"); got != "heavy" {
			t.Errorf("got %q, want bare raw id", got)
		}
	})
}
