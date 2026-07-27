package hostfit_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The host shapes below are the bytes an agent actually publishes,
// decoded the way a consumer decodes them. Writing them as wire strings
// rather than as structs is the discipline from waired-ai/waired#950:
// hand-written fixtures asserted vendor / unified_memory / usable_vram_mb
// for weeks while no shipped agent sent them.
const (
	wireRTX4090 = `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
		`"compute_cap":"8.9","vendor":"nvidia"}],"ram_total_gb":64}`
	// The waired#942 host: plenty of system RAM, a card that cannot hold
	// what that RAM figure suggests.
	wireBigRAMSmallGPU = `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
		`"compute_cap":"8.9","vendor":"nvidia"}],"ram_total_gb":128}`
	// A 16 GB Mac: ram_total_gb and the raw vram_total_mb both overstate
	// the budget; only usable_vram_mb is the number a fit may use.
	wireMac16 = `{"gpus":[{"model":"Apple M3","vram_total_mb":16384,"vendor":"apple"}],` +
		`"ram_total_gb":16,"unified_memory":true,"usable_vram_mb":12288}`
	wireCPUOnly = `{"ram_total_gb":128}`
	// A pre-v0.2.4 agent: a GPU, but none of the host-fit facts.
	wireLegacyGPU = `{"gpus":[{"model":"NVIDIA GeForce RTX 4090","vram_total_mb":24564,` +
		`"compute_cap":"8.9"}],"ram_total_gb":64}`
)

func hostFromWire(t *testing.T, payload string) hostfit.Host {
	t.Helper()
	var hw signer.HardwareSummary
	if err := json.Unmarshal([]byte(payload), &hw); err != nil {
		t.Fatalf("fixture is not valid agent wire form: %v", err)
	}
	return hostfit.FromHardwareSummary(&hw)
}

// TestFromHardwareSummary pins the adapter, including the two shapes
// defined by what they lack. A nil summary must not be confused with a
// CPU-only host by anything downstream — both yield no GPU here, which
// is why the doc comment tells callers to distinguish them first.
func TestFromHardwareSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
		want hostfit.Host
	}{
		{"discrete nvidia", wireRTX4090, hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24564}},
		{"big ram, small gpu", wireBigRAMSmallGPU, hostfit.Host{RAMTotalGB: 128, GPUCount: 1, VRAM0MB: 24564}},
		{
			"unified memory",
			wireMac16,
			hostfit.Host{RAMTotalGB: 16, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 12288, VRAM0MB: 16384},
		},
		{"cpu only", wireCPUOnly, hostfit.Host{RAMTotalGB: 128}},
		{"pre-v0.2.4 agent", wireLegacyGPU, hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24564}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostFromWire(t, tc.wire); got != tc.want {
				t.Errorf("FromHardwareSummary() = %+v, want %+v", got, tc.want)
			}
		})
	}

	if got := hostfit.FromHardwareSummary(nil); got != (hostfit.Host{}) {
		t.Errorf("FromHardwareSummary(nil) = %+v, want the zero Host", got)
	}
}

// TestEffectiveVRAMMB: unified memory reports the usable budget,
// everyone else the first GPU's raw figure, CPU-only nothing.
func TestEffectiveVRAMMB(t *testing.T) {
	for _, tc := range []struct {
		name string
		host hostfit.Host
		want int
	}{
		{"discrete gpu uses the raw figure", hostFromWire(t, wireRTX4090), 24564},
		{"uma uses the usable budget", hostFromWire(t, wireMac16), 12288},
		{"cpu-only has no budget", hostFromWire(t, wireCPUOnly), 0},
		{"pre-v0.2.4 agent degrades to the raw figure", hostFromWire(t, wireLegacyGPU), 24564},
		{
			// UnifiedMemory set but UsableVRAMMB unknown: fall back rather
			// than reading 0 as "no GPU".
			"uma without a usable figure falls back",
			hostfit.Host{UnifiedMemory: true, GPUCount: 1, VRAM0MB: 8192},
			8192,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.host.EffectiveVRAMMB(); got != tc.want {
				t.Errorf("EffectiveVRAMMB() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOllamaVRAMOverheadMB pins the three arms of the overhead model.
// The discrete slope is what makes a 22.6 GB model fit a 24 GB card;
// the old flat 4096 did not.
func TestOllamaVRAMOverheadMB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unified  bool
		weightGB float64
		want     int
	}{
		{"uma is flat", true, 22.6, 1024},
		{"uma is flat even with no weight", true, 0, 1024},
		{"discrete scales with weight", false, 22.6, 1024 + 904},
		{"unknown weight keeps the conservative flat reservation", false, 0, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostfit.OllamaVRAMOverheadMB(tc.unified, tc.weightGB); got != tc.want {
				t.Errorf("OllamaVRAMOverheadMB(%v, %v) = %d, want %d",
					tc.unified, tc.weightGB, got, tc.want)
			}
		})
	}
}

// TestOllamaResidentMB: decimal-GB weights against a binary-MiB budget,
// rounded UP — a variant must not fit by a rounding artefact.
func TestOllamaResidentMB(t *testing.T) {
	v := catalog.Variant{EstimatedWeightGB: 22.6, KVBytesPerTokenFP16: 20480}
	// 22.6e9 B → 21554 MiB; 20480 B/token × 16384 tokens → 320 MiB;
	// overhead 1024 + 40×22.6 → 1928.
	if got, want := hostfit.OllamaResidentMB(v, false), 21554+320+1928; got != want {
		t.Errorf("OllamaResidentMB() = %d, want %d", got, want)
	}
	if got := hostfit.OllamaResidentMB(catalog.Variant{}, false); got != 0 {
		t.Errorf("OllamaResidentMB(unannotated) = %d, want 0 — an unknown weight is "+
			"not a model that fits in nothing", got)
	}
}

func TestOllamaFit(t *testing.T) {
	// A 7B-class q4 gguf, and a 120B-class one: the pair the waired#942
	// host disagreed with the agent about.
	small := catalog.Variant{
		VariantID: "q4-gguf", RuntimeSupport: []string{"ollama"},
		MinRAMGB: 8, EstimatedWeightGB: 4.7, KVBytesPerTokenFP16: 28672,
	}
	big := catalog.Variant{
		VariantID: "mxfp4-gguf", RuntimeSupport: []string{"ollama"},
		MinRAMGB: 96, EstimatedWeightGB: 62.0, KVBytesPerTokenFP16: 98304,
	}

	for _, tc := range []struct {
		name       string
		v          catalog.Variant
		host       hostfit.Host
		wantFits   bool
		wantReason string
	}{
		{"small fits a 64 GB box with a 24 GB card", small, hostFromWire(t, wireRTX4090), true, hostfit.ReasonOK},
		{
			// #229, and an inverted pin: this used to be rejected on
			// residency, which meant a graphics card REMOVED a model the
			// same host served without one. Capacity now says yes to both
			// — the card holds what fits and the rest runs from the same
			// system RAM — and speed, asserted separately, is what keeps a
			// genuinely slow combination out of an auto-selection.
			"big fits a 24 GB card because the host has the RAM behind it",
			big, hostFromWire(t, wireBigRAMSmallGPU), true, hostfit.ReasonOK,
		},
		{
			// Same variant, same RAM, no GPU: spilling to system RAM is how
			// a CPU host is meant to run, so the RAM gate is the only bound.
			"big fits the same RAM with no GPU at all",
			big, hostFromWire(t, wireCPUOnly), true, hostfit.ReasonOK,
		},
		{
			"the ram gate reports first when both would fail",
			big, hostfit.Host{RAMTotalGB: 8, GPUCount: 1, VRAM0MB: 4096}, false, hostfit.ReasonInsufficientRAM,
		},
		{"small fits a 16 GB mac", small, hostFromWire(t, wireMac16), true, hostfit.ReasonOK},
		{
			// UMA rejects on residency, never on the RAM gate: 16 GB of
			// shared RAM clears MinRAMGB=96 only because the gate is
			// skipped, and the 12 GB usable budget is the real wall.
			"a 16 GB mac rejects the big model on residency",
			big, hostFromWire(t, wireMac16), false, hostfit.ReasonInsufficientVRAM,
		},
		{
			"a variant with no declared minimum and no weight fits anything",
			catalog.Variant{RuntimeSupport: []string{"ollama"}},
			hostFromWire(t, wireRTX4090), true, hostfit.ReasonOK,
		},
		{
			// A GPU we cannot size is not a GPU that fits nothing.
			"an unknown vram budget does not reject the catalog",
			big, hostfit.Host{RAMTotalGB: 128, GPUCount: 1}, true, hostfit.ReasonOK,
		},
		{
			// Detection failure, not a 0 GB machine.
			"an unknown ram figure skips the ram gate",
			big, hostfit.Host{GPUCount: 0}, true, hostfit.ReasonOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.OllamaFit(tc.v, tc.host)
			if got.Fits != tc.wantFits || got.Reason != tc.wantReason {
				t.Fatalf("OllamaFit() = %+v, want {Fits:%v Reason:%q}", got, tc.wantFits, tc.wantReason)
			}
			if got.Fits && (got.NeedMB != 0 || got.HaveMB != 0) {
				t.Errorf("a fitting verdict carries numbers (%+v); they are documented as "+
					"shortfall-only, and a consumer would render them as a warning", got)
			}
			if !got.Fits && got.NeedMB <= 0 {
				t.Errorf("verdict %+v states no requirement; the UI cannot say how far short "+
					"the machine falls", got)
			}
		})
	}
}

// TestOllamaResident_IgnoresTheRAMGate: the residency half must answer
// only the GPU question. A caller explaining a rejection needs to know
// which gate bound — naming the RAM figure when the card was the wall
// sends the operator to buy the wrong hardware.
func TestOllamaResident_IgnoresTheRAMGate(t *testing.T) {
	// Fails the RAM gate outright (needs 96, host has 8) but is small
	// enough to live in the card.
	v := catalog.Variant{MinRAMGB: 96, EstimatedWeightGB: 4.7, KVBytesPerTokenFP16: 28672}
	host := hostfit.Host{RAMTotalGB: 8, GPUCount: 1, VRAM0MB: 24564}

	if got := hostfit.OllamaResident(v, host); !got.Fits {
		t.Errorf("OllamaResident() = %+v, want a fitting verdict — the RAM gate is not its question", got)
	}
	if got := hostfit.OllamaFit(v, host); got.Fits || got.Reason != hostfit.ReasonInsufficientRAM {
		t.Errorf("OllamaFit() = %+v, want the RAM shortfall", got)
	}
}

// TestOllamaFit_ShortfallNumbers: the numbers have to be the ones the
// decision was actually made on, or the sentence the UI builds from them
// is a different claim than the verdict.
func TestOllamaFit_ShortfallNumbers(t *testing.T) {
	big := catalog.Variant{MinRAMGB: 96, EstimatedWeightGB: 62.0, KVBytesPerTokenFP16: 98304}

	// The VRAM shortfall is still what a deficit label reports, but on a
	// discrete card it no longer decides the capacity verdict (#229), so
	// it is read from the residency half directly.
	vram := hostfit.OllamaResident(big, hostFromWire(t, wireBigRAMSmallGPU))
	if want := hostfit.OllamaResidentMB(big, false); vram.NeedMB != want || vram.HaveMB != 24564 {
		t.Errorf("vram shortfall = need %d have %d, want %d / 24564", vram.NeedMB, vram.HaveMB, want)
	}

	ram := hostfit.OllamaFit(big, hostfit.Host{RAMTotalGB: 64})
	if ram.NeedMB != 96*1024 || ram.HaveMB != 64*1024 {
		t.Errorf("ram shortfall = need %d have %d, want %d / %d",
			ram.NeedMB, ram.HaveMB, 96*1024, 64*1024)
	}

	// Unified memory still rejects on residency — one pool has nowhere to
	// spill to — so there the shortfall IS the capacity verdict.
	uma := hostfit.OllamaFit(big, hostFromWire(t, wireMac16))
	if uma.Fits || uma.Reason != hostfit.ReasonInsufficientVRAM || uma.NeedMB <= 0 {
		t.Errorf("uma verdict = %+v, want an insufficient_vram shortfall", uma)
	}
}

// TestOllamaFitIsMonotoneInHardware is the #229 regression test, and the
// reason the discrete capacity gate dropped its residency requirement.
//
// Adding a graphics card cannot make a machine slower, so it must not
// make it able to serve FEWER models. The old rule broke that: a 128 GB
// host served a 62 GB model and the same host with a 24 GB card did not.
// A property over the real catalog rather than a case list, because the
// invariant is the point — any future rule change that reintroduces the
// inversion fails here regardless of which model exposes it.
func TestOllamaFitIsMonotoneInHardware(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, ram := range []int{8, 16, 32, 64, 128, 512} {
		for _, vram := range []int{4096, 8192, 12288, 16384, 24564, 49152} {
			bare := hostfit.Host{RAMTotalGB: ram}
			carded := hostfit.Host{RAMTotalGB: ram, GPUCount: 1, VRAM0MB: vram}
			for _, m := range manifests {
				for _, v := range m.Variants {
					if !supports(v, catalog.RuntimeOllama) {
						continue
					}
					if !hostfit.OllamaFit(v, bare).Fits {
						continue
					}
					if !hostfit.OllamaFit(v, carded).Fits {
						t.Fatalf("%s/%s: %d GB of RAM serves it, but the same host with a %d MB card does not",
							m.ModelID, v.VariantID, ram, vram)
					}
				}
			}
		}
	}
}

func TestVLLMFit(t *testing.T) {
	v := catalog.Variant{VariantID: "awq-int4", MinVRAMMB: 16000}

	for _, tc := range []struct {
		name       string
		v          catalog.Variant
		budgetMB   int
		wantFits   bool
		wantReason string
	}{
		{"a 24 GB card serves a 16 GB variant", v, 24564, true, hostfit.ReasonOK},
		{"a 12 GB budget does not", v, 12288, false, hostfit.ReasonInsufficientVRAM},
		{"no budget is no gpu, not a shortfall", v, 0, false, hostfit.ReasonNoGPU},
		{
			// vLLM does not run without a GPU; "fits" would be worse than
			// naming the missing card.
			"no budget stays no gpu even with no declared minimum",
			catalog.Variant{}, 0, false, hostfit.ReasonNoGPU,
		},
		{"no declared minimum fits any real budget", catalog.Variant{}, 8192, true, hostfit.ReasonOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.VLLMFit(tc.v, tc.budgetMB)
			if got.Fits != tc.wantFits || got.Reason != tc.wantReason {
				t.Errorf("VLLMFit() = %+v, want {Fits:%v Reason:%q}", got, tc.wantFits, tc.wantReason)
			}
		})
	}

	// The no-GPU verdict states the requirement but no "have": there is
	// no figure to compare against, and 0 GB would read as a measurement.
	if got := hostfit.VLLMFit(v, 0); got.NeedMB != 16000 || got.HaveMB != 0 {
		t.Errorf("no-gpu verdict = %+v, want need 16000 / have unset", got)
	}
}

// TestBundledCatalog_WaiRed942 runs the rules against the REAL bundled
// catalog on the host waired#942 was reported from: 128 GB of RAM and a
// 24 GB card. The control plane compared system RAM alone there, so the
// largest ollama model passed and — being the first runnable entry in
// filename order — became the wizard's DEFAULT.
//
// The claim moved with #229. Being offered is no longer the failure;
// being auto-SELECTED is. Withholding a model a machine can run, because
// it owns a graphics card, was never the right answer either. So what is
// asserted is: the machine is pointed at the model that lives in its
// card, and anything that would be genuinely slow there is excluded from
// that choice by the speed bound rather than by capacity.
func TestBundledCatalog_WaiRed942(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	host := hostFromWire(t, wireBigRAMSmallGPU)

	var fitting, selectable int
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			got := hostfit.OllamaFit(v, host)
			if !got.Fits {
				continue
			}
			fitting++
			if !got.Estimate.UpperBound || got.Estimate.MeetsSpeedFloor {
				selectable++
			}
		}
	}
	if selectable == 0 {
		t.Fatal("nothing is auto-selectable on a 128 GB / 24 GB-card host; " +
			"the rules are rejecting everything")
	}
	if fitting == selectable {
		t.Error("every fitting variant also cleared the speed bound on this host; " +
			"the speed term is not discriminating and this test proves nothing")
	}

	// Both models from the report RUN — that is the #229 correction, and
	// the 62 GB one is a 5.1B-active mixture of experts that decodes
	// usefully with two thirds of its weights in system RAM.
	assertFit(t, manifests, host, "gpt-oss-120b", true)
	assertFit(t, manifests, host, "qwen3.6-35b-a3b", true)
	// The one that should not be auto-selected here reads twice the
	// active parameters through the same spill.
	assertSelectable(t, manifests, host, "qwen3.5-122b-a10b", false)

	// waired#942 itself — the DEFAULT — stays fixed, and now by the
	// honest mechanism: the model that lives in the card is also the
	// highest-quality one, so it wins outright rather than by its rivals
	// being hidden.
	if best := bestByTier(t, manifests, host); best != "qwen3.6-35b-a3b" {
		t.Errorf("highest-tier auto-selectable model on the waired#942 host = %s, "+
			"want qwen3.6-35b-a3b (a 62 GB model must not be what this machine is pointed at)", best)
	}
}

// assertSelectable checks whether ANY ollama variant of modelID survives
// both gates — capacity, and the speed bound where one exists.
func assertSelectable(t *testing.T, manifests []catalog.Manifest, host hostfit.Host, modelID string, want bool) {
	t.Helper()
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			got := hostfit.OllamaFit(v, host)
			ok := got.Fits && (!got.Estimate.UpperBound || got.Estimate.MeetsSpeedFloor)
			if ok == want {
				return
			}
		}
		t.Fatalf("%s: no ollama variant with selectable=%v on %+v", modelID, want, host)
	}
	t.Fatalf("%s is not in the bundled catalog", modelID)
}

// bestByTier is what a tier-ordered picker lands on — the agent's
// RankModels and the control plane's recommendation both do this.
func bestByTier(t *testing.T, manifests []catalog.Manifest, host hostfit.Host) string {
	t.Helper()
	best, bestTier := "", -1
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			got := hostfit.OllamaFit(v, host)
			ok := got.Fits && (!got.Estimate.UpperBound || got.Estimate.MeetsSpeedFloor)
			if ok && v.QualityTier > bestTier {
				best, bestTier = m.ModelID, v.QualityTier
			}
		}
	}
	return best
}

func assertFit(t *testing.T, manifests []catalog.Manifest, host hostfit.Host, modelID string, want bool) {
	t.Helper()
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			if got := hostfit.OllamaFit(v, host); got.Fits == want {
				return
			}
		}
		t.Errorf("%s: no ollama variant with Fits=%v on %+v", modelID, want, host)
		return
	}
	t.Skipf("%s is no longer in the bundled catalog", modelID)
}

func supports(v catalog.Variant, engine string) bool {
	return slices.Contains(v.RuntimeSupport, engine)
}

// --- host class + decode estimate (waired-ai/waired-agent#229) --------

// TestHostClass pins the three-way split. The classes are not
// interchangeable: what happens when the weights do not fit differs in
// kind, not degree — a discrete card spills to system RAM, a unified
// pool has nowhere to spill to, and a CPU-only host was never holding
// anything anywhere else.
func TestHostClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		host hostfit.Host
		want hostfit.Class
	}{
		{"cpu only", hostFromWire(t, wireCPUOnly), hostfit.ClassCPUOnly},
		{"discrete nvidia", hostFromWire(t, wireRTX4090), hostfit.ClassDiscrete},
		{"apple unified memory", hostFromWire(t, wireMac16), hostfit.ClassUnified},
		{
			// Strix Halo enumerates a device AND shares the pool. Unified
			// wins: the spill target is the same memory the weights are
			// already in, so treating it as discrete would model a
			// transfer that does not happen.
			"unified memory that also enumerates a gpu",
			hostfit.Host{RAMTotalGB: 128, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 96 * 1024},
			hostfit.ClassUnified,
		},
		{
			// A pre-v0.2.4 agent sends no unified_memory flag. Reading it
			// as discrete is the safe wrong answer — it is what the rules
			// did before the flag existed.
			"a pre-v0.2.4 agent reads as discrete",
			hostFromWire(t, wireLegacyGPU), hostfit.ClassDiscrete,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.host.Class(); got != tc.want {
				t.Errorf("Class() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestActiveBytesPerToken pins the term capacity math cannot see. These
// are product contracts: the two 27B-class entries below sit within
// 6 GB of each other on disk and differ by SEVEN TIMES in what a decode
// step must read, because one is a mixture of experts. Ranking them by
// size gets the speed order exactly backwards.
func TestActiveBytesPerToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    catalog.Variant
		want float64
	}{
		{
			"a dense model reads all of its weights",
			catalog.Variant{EstimatedWeightGB: 16.3, ParamCount: 27_000_000_000},
			16.3,
		},
		{
			"a mixture of experts reads only the active share",
			catalog.Variant{EstimatedWeightGB: 22.6, ParamCount: 35_000_000_000, ActiveParams: 3_300_000_000},
			22.6 * 3.3 / 35,
		},
		{
			"an unannotated variant makes no claim",
			catalog.Variant{ParamCount: 27_000_000_000},
			0,
		},
		{
			// Defensive: a manifest saying the active share is the whole
			// model is a dense model spelled differently.
			"active >= total is dense",
			catalog.Variant{EstimatedWeightGB: 4.0, ParamCount: 7_000_000_000, ActiveParams: 7_000_000_000},
			4.0,
		},
		{
			// No param_count to scale by: fall back to the full weight
			// rather than inventing an active share.
			"no param count is dense",
			catalog.Variant{EstimatedWeightGB: 4.0, ActiveParams: 1_000_000_000},
			4.0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.ActiveBytesPerToken(tc.v)
			if diff := got - tc.want; diff > 0.01 || diff < -0.01 {
				t.Errorf("ActiveBytesPerToken() = %.3f, want %.3f", got, tc.want)
			}
		})
	}
}

// TestEstimateOllamaDecode walks the per-class arithmetic.
//
// The tok/s figures themselves are a record of today's model at today's
// bandwidth constants — they move when those constants are replaced by
// measured per-device bandwidth. What is CONTRACT is the verdicts: a
// dense 27B is too slow on a small unified pool, a mixture of experts of
// similar size is not, and a discrete card holding the whole model is
// never the wall.
// TestBandwidthConstantsKeepTheirDirection pins WHICH WAY each bandwidth
// constant is allowed to be wrong. That is the property a comment alone
// already failed to protect once: the original text on
// BandwidthSystemRAMGBs argued for treating it as a population floor and
// asserted that "guessing low only adds 'this may be slow'", which is the
// opposite of what the one deciding branch needs.
//
// BandwidthSystemRAMGBs feeds ClassDiscrete-spilled, the only case that
// may EXCLUDE a model, and the estimate there is directly proportional to
// it. Lowering it toward a measured effective figure — the intuitive edit
// — turns models a machine runs into models the wizard refuses to offer.
// 48 GB/s is the highest sustained streaming read on record for a
// mainstream host (DDR5-4800 dual channel, ~62 % of its 76.8 GB/s spec),
// so the constant has to stay above that.
//
// BandwidthUnifiedGBs is the reverse. It may only annotate, so it is a
// floor and has to stay at or below the smallest shipping unified part.
// Raising it would silence the warning on exactly the machines that need
// it.
func TestBandwidthConstantsKeepTheirDirection(t *testing.T) {
	const (
		sustainedMainstreamGBs = 48.0  // DDR5-4800 dual channel, measured
		smallestUnifiedPartGBs = 120.0 // Apple M-series base
	)
	if hostfit.BandwidthSystemRAMGBs < sustainedMainstreamGBs {
		t.Errorf("BandwidthSystemRAMGBs = %v, below the %v GB/s a mainstream host actually sustains. "+
			"It is an UPPER bound on the branch that excludes; lowering it refuses runnable models. "+
			"Per-host measurement (#252) is the fix, not a smaller constant",
			hostfit.BandwidthSystemRAMGBs, sustainedMainstreamGBs)
	}
	if hostfit.BandwidthUnifiedGBs > smallestUnifiedPartGBs {
		t.Errorf("BandwidthUnifiedGBs = %v, above the smallest shipping unified part (%v GB/s). "+
			"It is a FLOOR that may only annotate; raising it silences the warning on the parts that need it",
			hostfit.BandwidthUnifiedGBs, smallestUnifiedPartGBs)
	}
}

func TestEstimateOllamaDecode(t *testing.T) {
	dense27b := catalog.Variant{
		VariantID: "q4-gguf", EstimatedWeightGB: 16.3,
		ParamCount: 27_000_000_000, KVBytesPerTokenFP16: 65536,
	}
	moe35b := catalog.Variant{
		VariantID: "mtp-q4-gguf", EstimatedWeightGB: 22.6,
		ParamCount: 35_000_000_000, ActiveParams: 3_300_000_000, KVBytesPerTokenFP16: 20480,
	}
	big120b := catalog.Variant{
		VariantID: "mxfp4-gguf", EstimatedWeightGB: 62.0, MinRAMGB: 96,
		ParamCount: 116_800_000_000, ActiveParams: 5_100_000_000, KVBytesPerTokenFP16: 98304,
	}
	moe122b := catalog.Variant{
		VariantID: "q4-gguf", EstimatedWeightGB: 81.0, MinRAMGB: 128,
		ParamCount: 122_000_000_000, ActiveParams: 10_000_000_000, KVBytesPerTokenFP16: 24576,
	}

	mac24 := hostfit.Host{RAMTotalGB: 24, UnifiedMemory: true, UsableVRAMMB: 18432, VRAM0MB: 24576}
	cpu128 := hostfit.Host{RAMTotalGB: 128}
	card24 := hostfit.Host{RAMTotalGB: 128, GPUCount: 1, VRAM0MB: 24564}

	for _, tc := range []struct {
		name      string
		v         catalog.Variant
		host      hostfit.Host
		wantFloor bool
		wantRes   bool
		wantBound bool
	}{
		// The 24 GB Mac is why this term exists. Both models sit in the
		// pool; the dense one decodes at a fraction of the speed. Neither
		// figure is an upper bound — a faster chip of the same pool size
		// beats it — so both are annotations, not exclusions.
		{"a dense 27B is too slow on a small unified pool", dense27b, mac24, false, true, false},
		{"a 3B-active MoE is not", moe35b, mac24, true, true, false},

		{"a dense 27B is far too slow on the cpu", dense27b, cpu128, false, false, false},
		{"the same cpu runs a 3B-active MoE", moe35b, cpu128, true, false, false},

		// A card holding the whole model decides the rate with its own
		// bandwidth, which this package does not know — so it makes no
		// claim rather than a wrong one.
		{"a resident discrete card is never the wall", moe35b, card24, true, true, false},
		// Spilled: the card's own reads are priced at zero, so this IS an
		// upper bound and may be acted on. The active share per token is
		// small enough that it still clears the floor.
		{"a heavily spilled 5B-active MoE still clears the floor", big120b, card24, true, false, true},
		// Spilled with twice the active share: too slow even with the
		// card's contribution free, which is what makes the exclusion
		// safe on a card of any speed.
		{"a spilled 10B-active MoE does not", moe122b, card24, false, false, true},

		{
			"a variant with no sizing annotations makes no claim",
			catalog.Variant{VariantID: "unknown"}, card24, true, false, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostfit.EstimateOllamaDecode(tc.v, tc.host)
			if got.MeetsSpeedFloor != tc.wantFloor || got.Resident != tc.wantRes || got.UpperBound != tc.wantBound {
				t.Errorf("EstimateOllamaDecode() = %+v, want MeetsSpeedFloor=%v Resident=%v UpperBound=%v",
					got, tc.wantFloor, tc.wantRes, tc.wantBound)
			}
			if got.Resident && got.ResidentShare != 1 {
				t.Errorf("resident verdict with share %.2f", got.ResidentShare)
			}
			// The rule the router acts on: only an upper bound may
			// exclude. Anything else is a sentence in the wizard.
			if got.UpperBound && got.Resident {
				t.Error("a resident estimate cannot be an upper bound; the card's " +
					"speed is the whole answer there")
			}
		})
	}
}

// TestEstimateIsMonotoneInHardware: adding a graphics card cannot make a
// machine slower, so the estimate must never demote a model from "fast
// enough" to "slow" when a card appears. Asserted as a property over the
// real catalog rather than as cases, because the invariant is the point:
// it is what lets the capacity gate drop its residency requirement on
// discrete hosts without the wizard's recommendation getting worse.
func TestEstimateIsMonotoneInHardware(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, ram := range []int{8, 16, 32, 64, 128, 512} {
		for _, vram := range []int{4096, 8192, 12288, 16384, 24564, 49152} {
			bare := hostfit.Host{RAMTotalGB: ram}
			carded := hostfit.Host{RAMTotalGB: ram, GPUCount: 1, VRAM0MB: vram}
			for _, m := range manifests {
				for _, v := range m.Variants {
					if !supports(v, catalog.RuntimeOllama) {
						continue
					}
					b := hostfit.EstimateOllamaDecode(v, bare)
					c := hostfit.EstimateOllamaDecode(v, carded)
					if b.MeetsSpeedFloor && !c.MeetsSpeedFloor {
						t.Fatalf("%s/%s: %d GB of RAM clears the speed floor (%.1f tok/s), "+
							"but adding a %d MB card drops it to %.1f",
							m.ModelID, v.VariantID, ram, b.TokpsEstimate, vram, c.TokpsEstimate)
					}
					if !c.Resident && c.TokpsEstimate < b.TokpsEstimate {
						t.Fatalf("%s/%s: a %d MB card made the estimate WORSE (%.1f -> %.1f tok/s)",
							m.ModelID, v.VariantID, vram, b.TokpsEstimate, c.TokpsEstimate)
					}
				}
			}
		}
	}
}

// TestBundledCatalog_SmallMacPrefersSpeed is the concrete outcome the
// estimate buys, measured against the real catalog: on a 24 GB Mac the
// highest-tier model that FITS decodes at a fraction of the speed of a
// lower-tier one that also fits. Capacity alone picks the slow one, and
// that is what ships today.
func TestBundledCatalog_SmallMacPrefersSpeed(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	mac := hostfit.Host{RAMTotalGB: 24, UnifiedMemory: true, UsableVRAMMB: 18432, VRAM0MB: 24576}

	type cand struct {
		id    string
		tier  int
		tokps float64
	}
	var byCapacity, bySpeed *cand
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !supports(v, catalog.RuntimeOllama) {
				continue
			}
			got := hostfit.OllamaFit(v, mac)
			if !got.Fits {
				continue
			}
			c := &cand{m.ModelID, v.QualityTier, got.Estimate.TokpsEstimate}
			if byCapacity == nil || c.tier > byCapacity.tier {
				byCapacity = c
			}
			if got.Estimate.MeetsSpeedFloor && (bySpeed == nil || c.tier > bySpeed.tier) {
				bySpeed = c
			}
		}
	}
	if byCapacity == nil || bySpeed == nil {
		t.Fatal("a 24 GB Mac fits nothing at all; the rule is rejecting everything")
	}
	if byCapacity.id == bySpeed.id {
		t.Fatalf("capacity and speed agree on %s here, so this fixture no longer "+
			"demonstrates the problem — re-check the catalog", byCapacity.id)
	}
	if byCapacity.tokps >= bySpeed.tokps {
		t.Fatalf("the capacity pick (%s, %.1f tok/s) is not slower than the speed pick (%s, %.1f tok/s)",
			byCapacity.id, byCapacity.tokps, bySpeed.id, bySpeed.tokps)
	}
	t.Logf("24 GB Mac: capacity picks %s (tier %d, %.1f tok/s); speed picks %s (tier %d, %.1f tok/s)",
		byCapacity.id, byCapacity.tier, byCapacity.tokps, bySpeed.id, bySpeed.tier, bySpeed.tokps)
}
