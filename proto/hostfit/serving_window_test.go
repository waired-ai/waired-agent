package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestDeclarableNativeWindow_CatalogClasses is the contract about the
// SHIPPED catalog, not about the constants: it asserts that the two
// declarable windows land on real manifest classes and that nothing
// sits between them.
//
// The four native windows we ship are 32768, 131072, 262144 and
// 1048576. If a future manifest arrives at, say, 524288 this test still
// passes — DeclarableNativeWindow floors it to 200k, which is the
// intended answer — but TestCatalogHasNoWindowClassBetweenTheTwo below
// goes red, which is the signal that the two-step design has a case it
// did not anticipate.
func TestDeclarableNativeWindow_CatalogClasses(t *testing.T) {
	for _, tc := range []struct {
		native int
		want   int
	}{
		{1048576, hostfit.ServingWindow1M},
		{262144, hostfit.ServingWindow200k},
		{200704, hostfit.ServingWindow200k}, // the window itself is inclusive
		{200703, 0},
		{131072, 0}, // cannot hold a coding session on any machine
		{32768, 0},
		{0, 0}, // unknown window declares nothing
	} {
		if got := hostfit.DeclarableNativeWindow(catalog.Manifest{ContextLength: tc.native}); got != tc.want {
			t.Errorf("DeclarableNativeWindow(%d) = %d, want %d", tc.native, got, tc.want)
		}
	}
}

// TestCatalogHasNoWindowClassBetweenTheTwo is why there are two serving
// windows and not three. An intermediate step could only ever admit a
// model whose native window lands strictly between them, and the
// catalog has none — so the step would cost hosts (a larger KV cache)
// and buy no models at all.
//
// It also pins that BOTH declarable windows are actually reachable by
// something we ship, so neither constant is decoration.
func TestCatalogHasNoWindowClassBetweenTheTwo(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	var got1M, got200k int
	for _, m := range manifests {
		if m.ContextLength > hostfit.ServingWindow200k && m.ContextLength < hostfit.ServingWindow1M &&
			m.ContextLength != 262144 {
			t.Errorf("%s advertises %d, between the two serving windows: the two-step "+
				"contract cannot express it — decide whether it declares 200k or nothing",
				m.ModelID, m.ContextLength)
		}
		switch hostfit.DeclarableNativeWindow(m) {
		case hostfit.ServingWindow1M:
			got1M++
		case hostfit.ServingWindow200k:
			got200k++
		}
	}
	if got1M == 0 {
		t.Error("no bundled manifest can declare the 1M window; the constant serves nothing")
	}
	if got200k == 0 {
		t.Error("no bundled manifest can declare the 200k window; local coding is unreachable")
	}
}

// TestMeetsServingWindow_ZeroAsksNothing pins the permissive-on-missing
// -inputs convention this package holds everywhere else: a caller that
// asks about no particular window is not rejecting the catalog.
func TestMeetsServingWindow_ZeroAsksNothing(t *testing.T) {
	m := catalog.Manifest{ContextLength: 32768}
	if !hostfit.MeetsServingWindow(m, 0) {
		t.Error("MeetsServingWindow(_, 0) = false, want true (no window asked about)")
	}
	if hostfit.MeetsServingWindow(m, hostfit.ServingWindow200k) {
		t.Error("a 32768-window model must not meet the 200k serving window")
	}
	if !hostfit.MeetsServingWindow(catalog.Manifest{ContextLength: 1048576}, hostfit.ServingWindow1M) {
		t.Error("a 1M-window model must meet the 1M serving window")
	}
}

// TestServingWindowKVMB_DoesNotOverflow is the reason the arithmetic is
// int64. The widest KV annotation we ship is 196608 B/token, which at
// 1M tokens is 2.06e11 bytes — past int32 — and a 32-bit truncation
// would report a NEGATIVE or absurdly small requirement, i.e. would
// silently declare a window the host cannot hold.
func TestServingWindowKVMB_DoesNotOverflow(t *testing.T) {
	v := catalog.Variant{KVBytesPerTokenFP16: 196608}
	got := hostfit.ServingWindowKVMB(v, hostfit.ServingWindow1M)
	// 196608 * 1048576 / 2 bytes = 98304 MiB exactly.
	if got != 98304 {
		t.Errorf("ServingWindowKVMB(196608 B/tok, 1M) = %d MiB, want 98304", got)
	}
	if hostfit.ServingWindowKVMB(catalog.Variant{}, hostfit.ServingWindow200k) != 0 {
		t.Error("an unannotated variant must report 0, not a guess")
	}
	if hostfit.ServingWindowKVMB(v, 0) != 0 {
		t.Error("a zero window must report 0")
	}
}

// TestServingWindowKVMB_IsTheEightBitCache pins the halving against the
// manifest annotation, which is fp16. If the serve tuning ever stops
// exporting an 8-bit KV cache this number is wrong in the dangerous
// direction — under-reporting what the window costs — so it is asserted
// rather than left implicit in the divisor.
func TestServingWindowKVMB_IsTheEightBitCache(t *testing.T) {
	v := catalog.Variant{KVBytesPerTokenFP16: 65536}
	fp16MB := 65536 * hostfit.ServingWindow200k / (1 << 20)
	if got := hostfit.ServingWindowKVMB(v, hostfit.ServingWindow200k); got != fp16MB/2 {
		t.Errorf("ServingWindowKVMB = %d MiB, want %d (half the fp16 annotation)", got, fp16MB/2)
	}
}

// TestOllamaWindowResidentMB_IsNotTheFitReserve keeps the two KV terms
// from being read as the same thing. OllamaResidentMB reserves a fixed
// 16k at fp16 to answer "can this host run the model"; this one prices
// the whole window the node proposes to stand behind. On the 24 GB
// anchor's flagship the difference is most of a card.
func TestOllamaWindowResidentMB_IsNotTheFitReserve(t *testing.T) {
	// qwen3.6-35b-a3b mtp-q4-gguf, the #624/#664 calibration anchor.
	v := catalog.Variant{EstimatedWeightGB: 22.62, KVBytesPerTokenFP16: 20480}

	fitReserve := hostfit.OllamaResidentMB(v, false)
	window := hostfit.OllamaWindowResidentMB(v, hostfit.ServingWindow200k, false)
	if window <= fitReserve {
		t.Errorf("serving 200k (%d MiB) must cost more than the fit reserve (%d MiB)",
			window, fitReserve)
	}

	weights := hostfit.OllamaWeightsResidentMB(v, false)
	if got, want := window-weights, hostfit.ServingWindowKVMB(v, hostfit.ServingWindow200k); got != want {
		t.Errorf("window requirement minus weights = %d MiB, want the KV term %d MiB", got, want)
	}

	if hostfit.OllamaWindowResidentMB(catalog.Variant{KVBytesPerTokenFP16: 20480},
		hostfit.ServingWindow200k, false) != 0 {
		t.Error("a variant with no weight annotation must report 0, matching OllamaResidentMB")
	}
}

// TestOllamaWindowResidentMB_AnchorHostServesTheFloor is the concrete
// claim the 200k window rests on: the smallest catalog variant above
// the install quality floor holds a full 200k KV cache on a small
// laptop GPU, so declaring the window is not a datacenter-only ability.
//
// Deliberately reads the shipped manifest rather than a fixture — the
// claim is about the catalog we actually ship.
func TestOllamaWindowResidentMB_SmallHostCanDeclare200k(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	const budgetMB = 8 * 1024 // an 8 GB card

	var best catalog.Variant
	var bestModel string
	for _, m := range manifests {
		if hostfit.DeclarableNativeWindow(m) < hostfit.ServingWindow200k {
			continue
		}
		for _, v := range m.Variants {
			if v.QualityTier < hostfit.InstallQualityFloorTier {
				continue
			}
			need := hostfit.OllamaWindowResidentMB(v, hostfit.ServingWindow200k, false)
			if need <= 0 || need > budgetMB {
				continue
			}
			if v.QualityTier > best.QualityTier {
				best, bestModel = v, m.ModelID
			}
		}
	}
	if bestModel == "" {
		t.Fatalf("no shipped variant above the install quality floor holds a 200k window "+
			"in %d MiB; the contract would take local inference away from small hosts", budgetMB)
	}
	t.Logf("8 GB card declares 200k with %s/%s (tier %d, %d MiB)",
		bestModel, best.VariantID, best.QualityTier,
		hostfit.OllamaWindowResidentMB(best, hostfit.ServingWindow200k, false))
}
