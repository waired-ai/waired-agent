package modelrank

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// ladder is three ollama families on a clean tier ladder, all declaring
// a coding window, so nothing but the thing under test separates them.
func ladder() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "big", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{catalog.RuntimeOllama}, EstimatedWeightGB: 6.0,
				MinRAMGB: 16, QualityTier: 90,
				Source: catalog.VariantSource{Type: "ollama", Tag: "big:9b"},
			}},
		},
		{
			ModelID: "mid", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{catalog.RuntimeOllama}, EstimatedWeightGB: 3.0,
				MinRAMGB: 8, QualityTier: 60,
				Source: catalog.VariantSource{Type: "ollama", Tag: "mid:4b"},
			}},
		},
		{
			ModelID: "small", ContextLength: 262144, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
				RuntimeSupport: []string{catalog.RuntimeOllama}, EstimatedWeightGB: 1.0,
				MinRAMGB: 4, QualityTier: 30,
				Source: catalog.VariantSource{Type: "ollama", Tag: "small:2b"},
			}},
		},
	}
}

func shaFor(t *testing.T, modelID string) string {
	t.Helper()
	for _, m := range ladder() {
		if m.ModelID == modelID {
			return catalog.VariantSHA(m.Variants[0])
		}
	}
	t.Fatalf("no fixture family %q", modelID)
	return ""
}

// roomyHost has room for every fixture family.
func roomyHost() hostfit.Host { return hostfit.Host{RAMTotalGB: 64} }

func input(measured map[string]MeasuredRate, floor float64) PickInput {
	return PickInput{
		Catalog:    ladder(),
		Host:       roomyHost(),
		Engine:     catalog.RuntimeOllama,
		Measured:   measured,
		FloorTokps: floor,
	}
}

func top(t *testing.T, in PickInput) string {
	t.Helper()
	ranked, err := RankModels(in)
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) == 0 {
		t.Fatal("RankModels returned nothing")
	}
	return ranked[0].Manifest.ModelID
}

// PRODUCT CONTRACT: the ladder is quality tier descending, then the
// lighter footprint, then catalog order.
func TestRankModels_Ordering(t *testing.T) {
	ranked, err := RankModels(input(nil, 0))
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	want := []string{"big", "mid", "small"}
	if len(ranked) != len(want) {
		t.Fatalf("ranked %d, want %d", len(ranked), len(want))
	}
	for i, id := range want {
		if ranked[i].Manifest.ModelID != id {
			t.Errorf("rank %d = %q, want %q", i, ranked[i].Manifest.ModelID, id)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#784): a model this host has MEASURED
// below the floor stops being the one it recommends, and the next rung
// down takes the badge — one rung per measurement, so the walk descends.
func TestRankModels_MeasuredSlowLosesTheTop(t *testing.T) {
	if got := top(t, input(nil, 60)); got != "big" {
		t.Fatalf("with nothing measured, top = %q, want big", got)
	}
	one := map[string]MeasuredRate{shaFor(t, "big"): {Tokps: 11}}
	if got := top(t, input(one, 60)); got != "mid" {
		t.Errorf("after big measured 11 tok/s, top = %q, want mid", got)
	}
	two := map[string]MeasuredRate{
		shaFor(t, "big"): {Tokps: 11},
		shaFor(t, "mid"): {Tokps: 26},
	}
	if got := top(t, input(two, 60)); got != "small" {
		t.Errorf("after big and mid both measured slow, top = %q, want small", got)
	}
}

// PRODUCT CONTRACT (waired-ai/waired#1056 decision 1): every pass is a
// narrow() rung, so a host that clears no bar keeps its ranking rather
// than being left with nothing to offer.
func TestRankModels_PassesStandDownRatherThanEmpty(t *testing.T) {
	all := map[string]MeasuredRate{
		shaFor(t, "big"):   {Tokps: 11},
		shaFor(t, "mid"):   {Tokps: 26},
		shaFor(t, "small"): {Tokps: 44},
	}
	ranked, err := RankModels(input(all, 60))
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) != 3 {
		t.Fatalf("ranked %d, want all 3 kept", len(ranked))
	}
	if ranked[0].Manifest.ModelID != "big" {
		t.Errorf("top = %q, want big", ranked[0].Manifest.ModelID)
	}
}

// PRODUCT CONTRACT: the figure belongs to the WEIGHTS, so a rate for
// other weights excludes nothing.
func TestRankModels_MeasurementIsKeyedToTheWeights(t *testing.T) {
	stale := map[string]MeasuredRate{
		catalog.VariantSHA(catalog.Variant{
			VariantID: "q4-gguf", Format: "ollama-tag", Quantization: "Q4_K_M",
			Source: catalog.VariantSource{Type: "ollama", Tag: "big:9b-OLD"},
		}): {Tokps: 11},
	}
	if got := top(t, input(stale, 60)); got != "big" {
		t.Errorf("a figure for other weights excluded big: top = %q", got)
	}
}

// PRODUCT CONTRACT: no floor means no speed claim, and a measurement at
// or above the floor is not evidence against anything.
func TestRankModels_MeasuredPassNeedsAFloorAndAShortfall(t *testing.T) {
	slow := map[string]MeasuredRate{shaFor(t, "big"): {Tokps: 11}}
	if got := top(t, input(slow, 0)); got != "big" {
		t.Errorf("with no floor, top = %q, want big", got)
	}
	at := map[string]MeasuredRate{shaFor(t, "big"): {Tokps: 60}}
	if got := top(t, input(at, 60)); got != "big" {
		t.Errorf("measured exactly at the floor, top = %q, want big", got)
	}
}

// PRODUCT CONTRACT: an explicit pin bypasses every pass. Somebody chose
// that model; being slow is not this package's licence to overrule them.
func TestRankModels_PreferredModelIDBypassesEveryPass(t *testing.T) {
	in := input(map[string]MeasuredRate{shaFor(t, "big"): {Tokps: 11}}, 60)
	in.PreferredModelID = "big"
	if got := top(t, in); got != "big" {
		t.Errorf("top = %q, want the pinned big", got)
	}

	in.PreferredModelID = "nosuch"
	if _, err := RankModels(in); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want ErrModelNotFound", err)
	}
}

// PRODUCT CONTRACT (waired-agent#521): manual_only is honoured at the
// manifest level and is NOT a narrow() rung — falling through would
// resurrect a withheld model on exactly the host where it is the only
// candidate, which is the case the field exists for.
func TestRankModels_ManualOnlyIsWithheldEvenWhenAlone(t *testing.T) {
	in := input(nil, 0)
	in.Catalog = []catalog.Manifest{{
		ModelID: "held", ContextLength: 262144, ManualOnly: "not for automatic choice",
		Variants: ladder()[0].Variants,
	}}
	_, err := RankModels(in)
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Fatalf("err = %v, want ErrHardwareInsufficient", err)
	}

	// A pin reaches it anyway: withholding a model from automatic choice
	// must not break an explicit choice somebody wrote down.
	in.PreferredModelID = "held"
	if got := top(t, in); got != "held" {
		t.Errorf("a pinned manual_only model was withheld: top = %q", got)
	}
}

// PRODUCT CONTRACT (waired-ai/waired#1225): the engine-floor RULE is one
// rule; what an unknown version means is the caller's, because the empty
// string means different things to a caller that serves and one that
// only offers.
func TestRankModels_UnknownEngineVersionIsTheCallersCall(t *testing.T) {
	floored := ladder()
	floored[0].Variants[0].MinEngineVersion = "0.32.0"

	in := input(nil, 0)
	in.Catalog = floored

	// A caller about to SERVE: unknown fails closed, because a variant
	// the engine cannot load fails server-side with no useful sign.
	if got := top(t, in); got != "mid" {
		t.Errorf("fail-closed: top = %q, want mid (big's floor is unmet)", got)
	}

	// A caller that only OFFERS: unknown passes, because its "unknown"
	// means "the device has not told me", and withholding top-tier
	// variants from devices it has no data about is worse.
	in.UnknownEngineVersionPasses = true
	if got := top(t, in); got != "big" {
		t.Errorf("fail-open: top = %q, want big", got)
	}

	// A KNOWN version is judged the same either way — the rule does not
	// change, only the meaning of silence.
	in.EngineVersion = "0.31.0"
	if got := top(t, in); got != "mid" {
		t.Errorf("a known too-old version admitted big: top = %q", got)
	}
	in.UnknownEngineVersionPasses = false
	if got := top(t, in); got != "mid" {
		t.Errorf("a known too-old version admitted big: top = %q", got)
	}
	in.EngineVersion = "0.32.13"
	if got := top(t, in); got != "big" {
		t.Errorf("a known new-enough version excluded big: top = %q", got)
	}
}

// PRODUCT CONTRACT (waired-agent#970): with no per-device detail the
// vLLM budget is exactly Host.EffectiveVRAMMB().
//
// This is the parity guarantee for the control plane: that is the figure
// it compared against before this package existed, so adopting the
// shared ladder must not silently re-price its single-GPU hosts. It is
// also what the agent's own budget returns at tensor-parallel size 1,
// which every single-GPU host is.
func TestVLLMVRAMBudget_NoDeviceDetailIsTheEffectiveVRAM(t *testing.T) {
	host := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24564, UsableVRAMMB: 24564}
	if got, want := VLLMVRAMBudgetMB(host, nil), host.EffectiveVRAMMB(); got != want {
		t.Errorf("budget with no GPU list = %d, want EffectiveVRAMMB %d", got, want)
	}
	single := []signer.HardwareGPUSummary{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24564}}
	if got, want := VLLMVRAMBudgetMB(host, single), host.EffectiveVRAMMB(); got != want {
		t.Errorf("single-GPU budget = %d, want EffectiveVRAMMB %d", got, want)
	}
}

// PRODUCT CONTRACT: tensor parallelism aggregates only IDENTICAL NVIDIA
// devices, at a power of two, and never shrinks the single-device
// budget.
func TestVLLMTensorParallelSize(t *testing.T) {
	l4 := signer.HardwareGPUSummary{Vendor: "nvidia", Model: "L4", VRAMTotalMB: 23034}
	for _, tt := range []struct {
		name string
		gpus []signer.HardwareGPUSummary
		want int
	}{
		{"no gpus", nil, 1},
		{"one", []signer.HardwareGPUSummary{l4}, 1},
		{"two identical", []signer.HardwareGPUSummary{l4, l4}, 2},
		{"three identical round down", []signer.HardwareGPUSummary{l4, l4, l4}, 2},
		{"four identical", []signer.HardwareGPUSummary{l4, l4, l4, l4}, 4},
		{"same name, different VRAM is a mismatch", []signer.HardwareGPUSummary{
			l4, {Vendor: "nvidia", Model: "L4", VRAMTotalMB: 12288}}, 1},
		{"non-nvidia is not counted", []signer.HardwareGPUSummary{
			l4, {Vendor: "amd", Model: "RX 7900", VRAMTotalMB: 23034}}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := VLLMTensorParallelSize(tt.gpus); got != tt.want {
				t.Errorf("= %d, want %d", got, tt.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#676): fp8 KV fails closed — one
// sub-Ada or unknown-capability NVIDIA device is enough to keep the
// whole host on fp16.
func TestVLLMUsesFP8KV_FailsClosed(t *testing.T) {
	ada := signer.HardwareGPUSummary{Vendor: "nvidia", Model: "L4", ComputeCap: "8.9"}
	ampere := signer.HardwareGPUSummary{Vendor: "nvidia", Model: "A100", ComputeCap: "8.0"}
	unknown := signer.HardwareGPUSummary{Vendor: "nvidia", Model: "?"}
	for _, tt := range []struct {
		name string
		gpus []signer.HardwareGPUSummary
		want bool
	}{
		{"no gpus at all", nil, false},
		{"one Ada", []signer.HardwareGPUSummary{ada}, true},
		{"two Ada", []signer.HardwareGPUSummary{ada, ada}, true},
		{"one Ada, one Ampere", []signer.HardwareGPUSummary{ada, ampere}, false},
		{"unknown capability", []signer.HardwareGPUSummary{ada, unknown}, false},
		{"amd only", []signer.HardwareGPUSummary{{Vendor: "amd"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := VLLMUsesFP8KV(tt.gpus); got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
			wantFactor := KVFactorF16
			if tt.want {
				wantFactor = KVFactorFP8
			}
			if got := VLLMKVFactor(tt.gpus); got != wantFactor {
				t.Errorf("KV factor = %v, want %v", got, wantFactor)
			}
		})
	}
}

// PRODUCT CONTRACT: the native context floor is a manifest comparison
// and nothing about the machine changes it.
func TestMeetsNativeContextFloor(t *testing.T) {
	if !MeetsNativeContextFloor(catalog.Manifest{ContextLength: 262144}) {
		t.Error("262144 should clear the native floor")
	}
	if MeetsNativeContextFloor(catalog.Manifest{ContextLength: 131072}) {
		t.Error("131072 should not clear the native floor")
	}
}

// A host that fits nothing is a hardware-shaped answer, not a fault:
// callers turn this into "no pick" rather than into a failed install.
func TestRankModels_NothingFitsIsHardwareInsufficient(t *testing.T) {
	in := input(nil, 0)
	in.Host = hostfit.Host{RAMTotalGB: 1}
	if _, err := RankModels(in); !errors.Is(err, ErrHardwareInsufficient) {
		t.Errorf("err = %v, want ErrHardwareInsufficient", err)
	}
}

func TestRankModels_EngineIsRequired(t *testing.T) {
	in := input(nil, 0)
	in.Engine = ""
	if _, err := RankModels(in); err == nil {
		t.Error("an empty Engine must be an error, not a silent empty ranking")
	}
}
