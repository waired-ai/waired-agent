package runtime

import "testing"

// TestServeInputsEqual_NoMmapIsAServeInput pins NoMmap into the bounce
// predicate. It selects a different derived model, so two tunings that differ
// only there are two different engine processes; leaving it out would latch a
// host on the tag it happened to boot with.
func TestServeInputsEqual_NoMmapIsAServeInput(t *testing.T) {
	base := ModelTuning{
		ModelID: "m", VariantID: "v", ContextLength: 200704,
		NumParallel: 1, KVCacheType: "q8_0", FlashAttention: true,
	}
	mapped, unmapped := base, base
	unmapped.NoMmap = true

	if mapped.ServeInputsEqual(unmapped) {
		t.Error("tunings differing only in NoMmap compared equal; the derived tag differs, so the engine process does too")
	}
	if !mapped.ServeInputsEqual(mapped) || !unmapped.ServeInputsEqual(unmapped) {
		t.Error("a tuning must compare equal to itself")
	}
	// The outcome fields this predicate deliberately ignores still must not
	// move it — NoMmap being an input is the claim, not "any new field".
	noisy := unmapped
	noisy.Verified = true
	noisy.Warning = "something happened"
	if !unmapped.ServeInputsEqual(noisy) {
		t.Error("post-spawn outcome fields must not make a tuning look like a different engine")
	}
}
