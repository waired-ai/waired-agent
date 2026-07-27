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
			// waired#942, the whole point: 128 GB of RAM clears MinRAMGB=96,
			// but 62 GB of weights cannot live in a 24 GB card. The control
			// plane said "runnable" here and made it the DEFAULT.
			"big does not fit a 24 GB card however much RAM the host has",
			big, hostFromWire(t, wireBigRAMSmallGPU), false, hostfit.ReasonInsufficientVRAM,
		},
		{
			// Same variant, same RAM, no GPU: spilling to system RAM is how
			// a CPU host is meant to run, so the RAM gate is the only bound.
			// The asymmetry is deliberate here and questioned in #229.
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

	vram := hostfit.OllamaFit(big, hostFromWire(t, wireBigRAMSmallGPU))
	if want := hostfit.OllamaResidentMB(big, false); vram.NeedMB != want || vram.HaveMB != 24564 {
		t.Errorf("vram shortfall = need %d have %d, want %d / 24564", vram.NeedMB, vram.HaveMB, want)
	}

	ram := hostfit.OllamaFit(big, hostfit.Host{RAMTotalGB: 64})
	if ram.NeedMB != 96*1024 || ram.HaveMB != 64*1024 {
		t.Errorf("ram shortfall = need %d have %d, want %d / %d",
			ram.NeedMB, ram.HaveMB, 96*1024, 64*1024)
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

// TestBundledCatalog_WaiRed942 runs the rule against the REAL bundled
// catalog on the host the issue was reported from: 128 GB of RAM and a
// 24 GB card. Before this package the control plane compared system RAM
// alone, so the largest ollama model in the catalog passed and — being
// the first runnable entry in filename order — became the wizard's
// default.
func TestBundledCatalog_WaiRed942(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	host := hostFromWire(t, wireBigRAMSmallGPU)
	eff := host.EffectiveVRAMMB()

	var fitting int
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
			// Every accepted variant must genuinely be resident-able.
			if need := hostfit.OllamaResidentMB(v, false); need > eff {
				t.Errorf("%s/%s accepted on a %d MB card while needing %d MB",
					m.ModelID, v.VariantID, eff, need)
			}
		}
	}
	if fitting == 0 {
		t.Fatal("no bundled ollama variant fits a 128 GB / 24 GB-card host; " +
			"the rule is rejecting everything")
	}

	// And the specific pair from the report: the 62 GB model is out, the
	// 22.6 GB one is in.
	assertFit(t, manifests, host, "gpt-oss-120b", false)
	assertFit(t, manifests, host, "qwen3.6-35b-a3b", true)
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
