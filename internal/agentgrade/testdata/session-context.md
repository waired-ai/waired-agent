Here is the context for the repository I am working in: the project's conventions, and the files I have already read in this session.

# Project conventions

- The wire-protocol module is additive-only once published: exported symbols may
  not be removed, retyped, or renamed, and new fields must serialise away when
  unset. A guard compares every change against the last published tag.
- Tests run with the race detector. A failure that only appears under parallel
  execution is a real defect in the code under test, not a flaky test, and is
  never to be papered over by raising a timeout.
- Errors crossing a package boundary are wrapped with %w so callers can match on
  sentinel values with errors.Is.
- Anything user-facing is written for someone who does not know the internals.
- One decision per file under docs/decisions/, dated. Never a single shared log.

# Files I have read so far

## internal/router/coding_floor.go (lines 1-120)

```go
// #624: the ~200k coding-agent context floor.
//
// Real coding-agent sessions measured on this repo peak at 75k–200k
// input tokens (heavy ones 300k+), with 35–50k of fixed overhead
// (system prompt + tool schemas + project instructions) before any
// conversation. A model that cannot hold ~200k either truncates or
// compacts constantly, so auto-selection prefers models that can
// actually serve that window. Two independent gates:
//
//   - Native floor (engine-independent): the manifest's own
//     context_length must reach codingAgentNativeContextMin. Applied
//     to auto-selection only; an explicit PreferredModelID bypasses it
//     with a visible warning.
//   - Host gate (ollama path): the host must serve the floor window
//     with q8_0 KV either fully GPU-resident, or — on discrete GPUs
//     only — within a bounded expected spill
//     (OllamaMaxExpectedSpillFraction). A bounded-spill flagship still
//     dominates the no-spill mid-tier fallback on both quality and
//     speed (24 GB anchor: tier-90 spilled at 85–104 tok/s vs the
//     tier-69 dense that fits un-spilled at ~32 tok/s), so selection
//     keeps a generous bound; the serve tuning separately caps the
//     spill it CREATES at OllamaIntentionalSpillCapExpected so decode
//     stays at the coding-agent selection floor (#670/#765 — at the
//     60 true-decode floor the cap clamps to the selection bound).
//   - Host gate (vllm path, #675/#678): the floor window's KV (fp8 on
//     Ada+ per #676, else fp16) plus activation-padded weights must fit
//     the default gpu-memory-utilization budget at the auto
//     tensor-parallel size (VLLMServesContextFloor). vLLM has no spill
//     semantics — an unfittable window is clamped at serve time — so
//     this gate is a plain window comparison with no spill allowance.
package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

const (
	// CodingAgentSelectionFloorTokps is the decode throughput (tok/s,
	// shallow-context boot benchmark, TRUE decode per #764: engine
	// counters or the overhead-corrected slope, median of 3) below
	// which a model is considered too slow for coding-agent use on
	// that host (#765, decision 20260711). 60 is anchored on industry
	// data: hosted Claude Sonnet 5 serves 67–90 tok/s output (the
	// entire Claude Code user base works in that band daily) and
	// NVIDIA's agentic-coding benchmark evaluates at 20 and 60 tok/s
	// SLOs. The previous value (100, #670) was calibrated against the
	// wall-clock benchmark #764 replaced, which under-measured fast
	// hosts ~35% — in true-decode terms the felt threshold already sat
	// in this 60–80 band, so this is a re-expression on the corrected
	// scale more than a loosening.
	// This is the default for the #133 lighter/upgrade recommendation
	// floor (config interactive_floor_tokps overrides it); it is NOT
	// the Phase-7 admission divisor, which stays at 30 tok/s — that one
	// models sustained per-session consumption, not acceptable latency
	// (see cmd/waired-agent/inference_bench.go).
	CodingAgentSelectionFloorTokps = 60.0

	// CodingAgentDepthFloorFraction scales the selection floor for the
	// depth-benchmark leg of the #133 comparison: the shallow boot
	// decode must clear the floor itself, while decode measured at
	// 64k–200k depth must clear floor × this fraction (= 48 tok/s at
	// the 60 default). 0.8 matches the measured long-context
	// degradation band (~200k-depth decode runs at roughly 0.7–0.8×
	// the shallow rate on the anchor host,
	// docs/reports/20260704-mtp-vs-spill-24gb.md C1: 165→116 tok/s at
	// 115k), so a host at the shallow floor still lands at or above
	// the scaled floor at depth. The shallow floor already prices in
	// the expected depth degradation; demanding the full floor at
	// depth would double-count it and nag on every host.
	CodingAgentDepthFloorFraction = 0.8

	// CodingAgentContextFloorTokens is the serve-time floor window:
	// ~200k, pre-aligned to 1024 (196×1024) and identical to the #625
	// measurement window so the calibration data maps 1:1.
	CodingAgentContextFloorTokens = 200704

	// codingAgentNativeContextMin gates manifest membership in the
	// coding-agent auto-selection pool. 200000 (not 200704) so exactly
	// the 262144-native manifests pass and the 131072 class does not.
	codingAgentNativeContextMin = 200000

	// ollamaSpillCalibration maps the byte-math spill prediction to
	// ollama's own /api/ps accounting. Single-point calibration on the
	// anchor host: predicted 3.9% ↔ measured 13.5% (#625 report).
	ollamaSpillCalibration = 3.0

	// OllamaMaxExpectedSpillFraction bounds the *expected measured*
	// spill the SELECTION gate accepts for a variant's floor-window
	// serviceability: within this bound a spilled high-tier model still
	// dominates the no-spill lower-tier alternative on both quality
	// and speed (24 GB anchor: qwen3.6-35b-a3b mtp at 11.5% expected
	// decodes 85–104 tok/s vs the no-spill tier-69 dense at ~32), so
	// excluding it from RankModels would produce strictly worse picks.
	// The anchor's 11.5% expected passes; the corrected non-MTP tag
	// (23.9 GB, expected ≈ 25%) does not.
	OllamaMaxExpectedSpillFraction = 0.20

	// OllamaIntentionalSpillCapExpected bounds the expected spill the
	// serve tuning deliberately CREATES when widening the window toward
	// the coding floor. Derived from the #664 A/B on the anchor host,
	// where the spilled fraction executes on a single CPU thread:
	// no-spill decode 158.6 tok/s, 13.4% measured spill → ~85 tok/s.
	// Modeling 1/rate = (1-s)/158.6 + s/21.25 (the second term is the
	// fitted effective rate of the CPU-executed share), decode stays at
	// or above the 60 tok/s selection floor (#765) while measured spill
	// s ≤ ~0.25, i.e. expected ≤ ~0.22 at the anchor's expected↔
	// measured ratio (11.5% ↔ 13.4%). That exceeds the selection gate's
	// outer bound, so the cap clamps to OllamaMaxExpectedSpillFraction:
	// every variant the gate admits now serves its full floor window,
	// and the tuner's trim only protects preferred-override models that
	// bypass the gate. (At the previous 100 floor the same model gave
	// ~0.075 and the 24 GB anchor traded window for decode.) Re-run
	// this derivation whenever the floor or the #664 numbers change
	// (an engine fix parallelizing the spilled phase raises the derived
	// bound further above the clamp).
	OllamaIntentionalSpillCapExpected = 0.20
)

```

## internal/router/model_picker.go (lines 90-260)

```go
//     weights + KV budget must fit GPU-resident, see ollamaFitsVRAM).
//  5. Sort by (quality_tier desc, MinVRAMMB asc, MinRAMGB asc, manifest
//     position asc).
//
// PickModel is RankModels(in)[0] with a richer reason trace. Returns the
// same errors PickModel does (ErrModelNotFound, ErrCapabilityNotMet,
// ErrHardwareInsufficient, or a plain error when Engine is empty). The
// returned slice is never empty on a nil error. Each Pick carries a
// short per-candidate reason; callers that want the full "why it won"
// trace should use PickModel (or build their own, as LighterCandidate
// does).
func RankModels(in PickInput) ([]Pick, error) {
	if in.Engine == "" {
		return nil, errors.New("router: RankModels requires Engine to be set")
	}

	// Step 1: PreferredModelID gate.
	manifests := in.Catalog
	if in.PreferredModelID != "" {
		filtered := make([]catalog.Manifest, 0, 1)
		for _, m := range in.Catalog {
			if m.ModelID == in.PreferredModelID {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrModelNotFound, in.PreferredModelID)
		}
		manifests = filtered
	}

	// Step 2: capability filter (manifest-level).
	var capable []catalog.Manifest
	for _, m := range manifests {
		if !manifestHasAll(m, in.RequireCapability) {
			continue
		}
		capable = append(capable, m)
	}
	if len(capable) == 0 {
		return nil, fmt.Errorf("%w: required %v", ErrCapabilityNotMet, in.RequireCapability)
	}

	// Steps 3+4: variant expansion + host-fit filter.
	type candidate struct {
		manifestIdx int
		manifest    catalog.Manifest
		variant     catalog.Variant
		floorOK     bool
		spill       float64
		est         hostfit.Estimate
	}
	var fits []candidate
	for i, m := range capable {
		for _, v := range m.Variants {
			if !engineSupports(v, in.Engine) {
				continue
			}
			if !engineVersionSatisfies(v, in.EngineVersion) {
				continue
			}
			if !variantSupportedByVendor(v, in.Engine, in.Hardware) {
				continue
			}
			if !hostFits(in.Engine, v, in.Hardware) {
				continue
			}
			// No speed claim by default — vLLM has no roofline model
			// here, and hostfit spells "no claim" as a passing floor.
			c := candidate{manifestIdx: i, manifest: m, variant: v,
				est: hostfit.Estimate{MeetsSpeedFloor: true}}
			// #624 coding-agent context floor: native window plus the
			// per-engine host gate — bounded-spill on ollama, the
			// utilization-budget window check on vllm (#675/#678; vLLM
			// clamps instead of spilling, so no spill fraction there).
			c.floorOK = MeetsNativeContextFloor(m)
			if in.Engine == catalog.RuntimeOllama {
				hostOK, spill := OllamaServesContextFloor(m, v, in.Hardware)
				c.spill = spill
				c.floorOK = c.floorOK && hostOK
				c.est = hostfit.EstimateOllamaDecode(v, in.Hardware.HostFit())
			}
			if in.Engine == catalog.RuntimeVLLM {
				c.floorOK = c.floorOK && VLLMServesContextFloor(m, v, in.Hardware)
			}
			fits = append(fits, c)
		}
	}
	if len(fits) == 0 {
		return nil, fmt.Errorf("%w: no variant fits hardware (engine=%s)", ErrHardwareInsufficient, in.Engine)
	}

	// Three-pass quality gating, best bar first, each falling through
	// only when it would leave nothing. An explicit PreferredModelID
	// bypasses all of it — the user asked for that model — with the
	// status still reported on the Pick.
	//
	//  1. #624 coding-agent context floor: native window plus the
	//     per-engine host gate.
	//  2. #229 decode floor: fast enough to be worth serving at all.
	//     This pass is the counterweight to the capacity gate no longer
	//     requiring GPU residency on discrete hosts — without it a host
	//     would auto-select a model that spills most of its layers
	//     whenever that model carried a higher quality tier.
	//
	//     It applies ONLY where the estimate is an upper bound. Two
	//     cases qualify. The spilled-discrete one: there the GPUs' own
	//     reads are priced at zero, a margin no unknown hardware can
	//     eat — and since #264 that share is computed against the POOL,
	//     because pricing two cards as one manufactures the very spill
	//     this pass then acts on. And, since #251, the unified-memory
	//     one when the host published its part's peak: a peak is an
	//     upper bound on THAT machine.
	//
	//     A unified host with no published peak falls back to a
	//     population constant that is neither bound — #270 checked, and
	//     the M1/M2/M3 bases sit below it, so it is not the FLOOR this
	//     comment used to claim. The CPU-only figure rests on a constant
	//     meant as an upper bound but with no margin behind it, so a
	//     host whose memory beats it would be excluded on the constant
	//     alone. Both get an annotation rather than a smaller catalog
	//     until per-device bandwidth lands (#252, #266).
	//  3. Everything that fits, so neither floor can newly turn a
	//     working host into an under-spec one.
	narrow := func(keep func(candidate) bool) {
		var pass []candidate
		for _, c := range fits {
			if keep(c) {
				pass = append(pass, c)
			}
		}
		if len(pass) > 0 {
			fits = pass
		}
	}
	if in.PreferredModelID == "" && !in.NoContextFloor {
		narrow(func(c candidate) bool { return c.floorOK })
	}
	if in.PreferredModelID == "" {
		narrow(func(c candidate) bool { return !c.est.UpperBound || c.est.MeetsSpeedFloor })
	}

	// Step 5: sort by tier desc, then MinVRAM/MinRAM asc, then manifest order.
	sort.SliceStable(fits, func(i, j int) bool {
		a, b := fits[i].variant, fits[j].variant
		if a.QualityTier != b.QualityTier {
			return a.QualityTier > b.QualityTier
		}
		if in.Engine == catalog.RuntimeVLLM {
			if a.MinVRAMMB != b.MinVRAMMB {
				return a.MinVRAMMB < b.MinVRAMMB
			}
		} else {
			if a.MinRAMGB != b.MinRAMGB {
				return a.MinRAMGB < b.MinRAMGB
			}
		}
		return fits[i].manifestIdx < fits[j].manifestIdx
	})

	out := make([]Pick, 0, len(fits))
	for _, c := range fits {
		p := Pick{
			Manifest: c.manifest,
			Variant:  c.variant,
			Reasons: []string{fmt.Sprintf("fitting candidate %s/%s (quality_tier=%d)",
				c.manifest.ModelID, c.variant.VariantID, c.variant.QualityTier)},
			ContextFloorSatisfied: c.floorOK,
			ExpectedSpillFraction: c.spill,
			DecodeEstimate:        c.est,
		}
```

## internal/router/install_picker.go (lines 1-73)

```go
package router

import (
	"errors"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// InstallQualityFloorTier is the coding-quality floor for install-time
// bundled-model auto-selection (#517). At install / `waired init` the
// installer auto-selects the largest catalog model that fits the host
// (via the runtime fit machinery) AND clears this quality_tier floor.
// When even the best-fitting model is below it — i.e. only sub-coding
// tiny models fit — the host is treated as under-spec and local
// inference is skipped (the node still enrolls and runs as a
// gateway/relay; it can route inference to peers).
//
// 30 == qwen2.5-coder-3b-instruct, the smallest usable coding model we
// ship. qwen3.5-2b (tier 27) and qwen3.5-0.8b (tier 12) fall below the
// floor and do not qualify for auto-selection.
//
// The value lives in proto/hostfit so the control plane's onboarding
// wizard can recommend what this host would have picked for itself
// rather than re-deriving the floor from prose (waired-ai/waired#941).
const InstallQualityFloorTier = hostfit.InstallQualityFloorTier

// SelectInstallModel chooses the bundled model to pre-pull at install
// time: the highest-quality_tier variant that both fits the host (via
// RankModels) AND clears minTier. It deliberately REUSES the runtime fit
// machinery — no new fit math — so the installer's pick matches what the
// agent would actually serve once enrolled.
//
// It returns the above-floor candidates in RankModels' canonical order
// (best first), so a caller facing a disk-space shortfall can step down
// to a smaller-but-still-above-floor model without re-ranking. ok is true
// when at least one fitting candidate clears the floor.
//
// ok=false with a nil error means "under-spec": either nothing fits the
// host at all (RankModels returned ErrHardwareInsufficient) or the
// best-fitting model is below the coding-quality floor. A non-nil error
// is a real misconfiguration (empty Engine, an unknown PreferredModelID,
// an unmet RequireCapability) that the caller should surface rather than
// silently treat as under-spec.
func SelectInstallModel(in PickInput, minTier int) (above []Pick, ok bool, err error) {
	ranked, err := RankModels(in)
	if err != nil {
		// "Nothing fits this host" is the under-spec signal, not a fault:
		// the caller skips local inference with a warning. Every other
		// error is a genuine misconfiguration worth surfacing.
		if errors.Is(err, ErrHardwareInsufficient) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// ranked is sorted quality_tier-desc; keep that order so above[0] is
	// the best fit and later entries are progressively lighter.
	for _, p := range ranked {
		if p.Variant.QualityTier >= minTier {
			above = append(above, p)
		}
	}
	// #624 must never turn a previously-working host into an under-spec
	// one: when the context floor left nothing above the quality-tier
	// floor (e.g. a 4 GB CPU host whose only tier-30+ fit is a 32k-window
	// model), retry without the context floor. The picks carry
	// ContextFloorSatisfied=false, so the install notes state the
	// compromise instead of silently disabling inference.
	if len(above) == 0 && !in.NoContextFloor {
		in.NoContextFloor = true
		return SelectInstallModel(in, minTier)
	}
	return above, len(above) > 0, nil
}
```

## internal/setup/modelselect.go (lines 100-205)

```go
// SelectBundledModel picks the bundled model to pre-pull at install time
// from the host's hardware profile (#517). It:
//
//  1. picks the engine (router.PickEngine);
//  2. selects the largest catalog model that fits the host AND clears the
//     coding-quality floor (router.SelectInstallModel);
//  3. pre-flights free disk at the download target and steps down to a
//     smaller-but-still-above-floor model when the best fit won't fit
//     disk (or skips the pull when even the smallest won't);
//  4. on an under-spec host (nothing above the floor fits) reports
//     EnableInference=false with an actionable warning — unless the
//     operator pinned a model or forced inference on.
//
// It reuses the runtime fit/scoring machinery wholesale; the only
// install-specific logic is the quality floor and the disk pre-flight.
func SelectBundledModel(in BundledModelInputs) (BundledModelSelection, error) {
	sel := BundledModelSelection{
		ModelID:         in.Inference.BundledModelID,
		EnableInference: true,
	}

	// Operator pinned a specific model: honour it verbatim, skip
	// auto-selection and the under-spec disable. The deploy-time defensive
	// disk check still guards a mid-download "disk full".
	if in.Pinned {
		sel.Notes = append(sel.Notes, fmt.Sprintf(
			"using pinned bundled model %q (hardware auto-selection skipped)", sel.ModelID))
		if m, found := catalog.LookupByAlias(sel.ModelID, in.Manifests); found && !router.MeetsNativeContextFloor(m) {
			sel.Notes = append(sel.Notes, fmt.Sprintf(
				"pinned model's native window is %d tokens — the ~200k coding-agent context floor is not enforced for pins",
				m.ContextLength))
		}
		return sel, nil
	}

	enginePick, err := router.PickEngine(router.EnginePickInput{
		Hardware:   in.Hardware,
		Preference: in.Inference.PreferredEngine,
	})
	if err != nil {
		return sel, fmt.Errorf("pick engine: %w", err)
	}
	engine := enginePick.Engine

	engineVer := ""
	if engine == catalog.RuntimeOllama {
		engineVer = infruntime.OllamaPinnedVersion
		if in.Inference.OllamaSource == agentconfig.OllamaSourceReuse {
			engineVer = in.ReuseOllamaVer
		}
	}

	above, ok, err := router.SelectInstallModel(router.PickInput{
		Catalog:       in.Manifests,
		Hardware:      in.Hardware,
		Engine:        engine,
		EngineVersion: engineVer,
	}, in.FloorTier)
	if err != nil {
		return sel, fmt.Errorf("select install model: %w", err)
	}

	if !ok {
		// Under-spec: no model above the coding-quality floor fits.
		sel.UnderSpec = true
		// Does anything fit at all, below the floor? On a 2–4 GB host the tiny
		// 0.5B may still run; expose it so the caller can offer local inference
		// on it as a deliberate low-quality opt-in rather than silently
		// disabling. (Guaranteed sub-floor: a tier ≥ floor variant would have
		// satisfied the floor pick above.)
		if below, belowOK, derr := router.SelectInstallModel(router.PickInput{
			Catalog:       in.Manifests,
			Hardware:      in.Hardware,
			Engine:        engine,
			EngineVersion: engineVer,
		}, 1); derr == nil && belowOK && len(below) > 0 {
			sel.BelowFloorModelID = below[0].Manifest.ModelID
		}
		if in.Forced {
			sel.Notes = append(sel.Notes, fmt.Sprintf(
				"hardware is under-spec for a usable coding model, but inference was forced on — %q may fail to load (%s)",
				sel.ModelID, describeHardwareFit(in.Hardware, engine)))
			return sel, nil
		}
		sel.EnableInference = false
		if sel.BelowFloorModelID == "" {
			// Nothing fits at all — emit the generic under-spec guidance. When a
			// below-floor model DOES fit, messaging is left to the caller (the
			// interactive opt-in dialog, or a non-interactive "left disabled" note).
			sel.Notes = append(sel.Notes,
				"local inference disabled: this host is under-spec for a usable coding model "+
					underSpecNeed(in.Manifests, engine, in.FloorTier, in.Hardware)+".",
				"This node still enrolls and runs as a gateway/relay — it can route inference to "+
					"mesh peers. Enable local inference later with a capable engine via "+
					"`waired runtimes install`, then `waired init`.")
		}
		return sel, nil
	}

	// A fitting model above the floor exists. Pick the best that also fits
	// free disk; fall back to the smallest-above-floor with the pull
	// skipped when even that won't fit disk.
	chosen, skipPull, notes := applyDiskPreflight(in, above, engine)
	sel.ModelID = chosen.Manifest.ModelID
	sel.SkipPull = skipPull
	sel.Notes = append(sel.Notes, notes...)
```

## proto/catalog/manifest.go (lines 30-175)

```go

// Manifest mirrors the JSON-on-disk schema for one model. Keep
// json tags in sync with both the embedded bundled/*.json files and
// the future CP /model-manifests endpoint payload.
type Manifest struct {
	ModelID       string        `json:"model_id"`
	DisplayName   string        `json:"display_name,omitempty"`
	ModelAliases  []string      `json:"model_aliases,omitempty"`
	License       string        `json:"license,omitempty"`
	ContextLength int           `json:"context_length"`
	Capabilities  []string      `json:"capabilities,omitempty"`
	Runtime       RuntimePolicy `json:"runtime"`
	Variants      []Variant     `json:"variants"`
	Security      Security      `json:"security"`
}

// RuntimePolicy expresses the manifest author's runtime preference.
// Phase A only honours `Preferred`; `Fallback` is reserved for later.
type RuntimePolicy struct {
	Preferred string   `json:"preferred"`
	Fallback  []string `json:"fallback,omitempty"`
}

// Variant is one (format × runtime × hardware footprint) combination
// of the model. A manifest must have at least one variant.
//
// QualityTier is the maintainer-assigned ranking the auto-picker uses
// to break ties: higher tier wins when multiple variants fit the host.
// Range [1, 100]; values must be unique across the bundled catalog.
//
// MinVRAMMB / MinRAMMB are the host-fit thresholds the auto-picker
// compares against hardware.Profile.GPUs[0].VRAMTotalMB / RAMTotalGB
// (RAM stays in GB because /proc/meminfo precision is plenty there).
// MinVRAMMB only applies to GPU runtimes (vllm); MinRAMGB only applies
// to CPU runtimes (ollama).
//
// ParamCount is the total parameter count (e.g. 8e9 for Qwen3-8B). For
// MoE models this is the TOTAL parameter count, not the active count,
// because "model quality / capability" — which the Phase 7 router
// scoring uses — scales with the full parameter pool, not just the
// active subset that fits in VRAM.
//
// QuantizationTier is the weight-precision ladder used in the Phase 7
// router score (`score = ParamCount × QuantizationTier`). Higher = more
// precision retained. Range [1, 8]:
//
//   - 4: AWQ-int4 / Q4_K_M / Q4_0
//   - 5: Q5_K_M
//   - 6: Q6_K
//   - 8: Q8_0 / FP16 / BF16 (treated as saturation; coding-agent
//     quality differences below ~8 bit start to matter much less than
//     param count, so 8 is the cap).
type Variant struct {
	VariantID         string        `json:"variant_id"`
	Format            string        `json:"format"` // safetensors | gguf | ollama-tag
	Quantization      string        `json:"quantization,omitempty"`
	DType             string        `json:"dtype,omitempty"`
	RuntimeSupport    []string      `json:"runtime_support"` // subset of {ollama, vllm}
	EstimatedWeightGB float64       `json:"estimated_weight_gb,omitempty"`
	MinRAMGB          int           `json:"min_ram_gb,omitempty"`
	MinVRAMMB         int           `json:"min_vram_mb,omitempty"`
	QualityTier       int           `json:"quality_tier"`
	ParamCount        int64         `json:"param_count"`
	QuantizationTier  int           `json:"quantization_tier"`
	Source            VariantSource `json:"source"`

	// ActiveParams is the MoE active parameter count (= decode FLOPs/tok
	// / 2). For dense models leave 0 — callers treat 0 as "= ParamCount".
	// Validate() enforces 0 ≤ ActiveParams ≤ ParamCount.
	ActiveParams int64 `json:"active_params,omitempty"`

	// KVBytesPerTokenFP16 is the per-token KV-cache footprint in bytes
	// assuming FP16 KV, after hybrid-mamba / sliding-window correction
	// (i.e. the value the Auto Selector should use directly when
	// budgeting context length). 0 means "unknown / unmeasured".
	KVBytesPerTokenFP16 int `json:"kv_bytes_per_token_fp16,omitempty"`

	// AttentionArch tags the attention topology so the Auto Selector can
	// reason about KV-cache scaling vs context length. Empty == unknown
	// (treated as standard for budgeting).
	AttentionArch string `json:"attention_arch,omitempty"`

	// VendorSupport is the GPU-vendor × runtime compatibility matrix.
	// nil == permissive (every supported runtime / vendor combination is
	// assumed "stable"). Empty per-cell strings have the same meaning.
	VendorSupport *VendorSupportMatrix `json:"vendor_support,omitempty"`

	// MXFP4Native is set for models distributed natively in MXFP4 (e.g.
	// openai/gpt-oss-*). When true the on-disk size matches MXFP4 even
	// without an extra quantization step, and the runtime must support
	// MXFP4 ingest (vLLM ≥ 0.6 stable, Ollama 0.4+ via llama.cpp).
	MXFP4Native bool `json:"mxfp4_native,omitempty"`

	// MinEngineVersion is the minimum SERVING-engine version (dotted,
	// e.g. "0.30.0") required to load this variant — e.g. qwen3.6 mtp
	// tags need Ollama >= 0.30 or the registry refuses the pull
	// server-side with no useful indication why. Compared against the
	// live engine version (HTTP /api/version; binary --version
	// fallback). Empty = no floor. An UNKNOWN live version excludes
	// the variant: a silent server-side failure is exactly the
	// incident this field prevents, so the gate fails closed.
	MinEngineVersion string `json:"min_engine_version,omitempty"`
}

// VendorSupportMatrix records, for one variant, which GPU vendor / runtime
// combinations the manifest author considers production-ready. Missing
// cells (zero value VendorRuntimeSupport / empty status strings) default
// to "stable" so manifests can be terse for the common case.
type VendorSupportMatrix struct {
	Nvidia VendorRuntimeSupport `json:"nvidia"`
	AMD    VendorRuntimeSupport `json:"amd"`
	Mac    VendorRuntimeSupport `json:"mac"`
}

// VendorRuntimeSupport carries one status string per runtime adapter.
// Values must be one of the VendorSupport* constants below; an empty
// string is treated as VendorSupportStable.
type VendorRuntimeSupport struct {
	VLLM     string `json:"vllm,omitempty"`
	Ollama   string `json:"ollama,omitempty"`
	LlamaCPP string `json:"llama_cpp,omitempty"`
	MLX      string `json:"mlx,omitempty"`
}

// VariantSource is the location from which the binary weights are
// fetched. Type=="ollama" uses Tag; Type=="huggingface" uses RepoID
// (and optionally Revision = commit SHA for reproducible pulls).
type VariantSource struct {
	Type     string `json:"type"`
	Tag      string `json:"tag,omitempty"`
	RepoID   string `json:"repo_id,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// SourceHF is the Type value for Hugging Face Hub repositories.
const SourceHuggingFace = "huggingface"

// SourceOllama is the Type value for Ollama tag-named blobs.
const SourceOllama = "ollama"

// Security captures the manifest-author-declared safety posture.
type Security struct {
	TrustRemoteCodeRequired bool `json:"trust_remote_code_required"`
	AllowPersistentKVCache  bool `json:"allow_persistent_kv_cache"`
}

```

# What I have concluded so far

- The narrowing helper in RankModels discards its filter whenever no candidate
  passes it. That is deliberate for the context floor — a host where nothing
  clears it should still be offered something — but the consequence is that the
  filter does nothing on exactly the hosts where it would matter most.
- RequireCapability has no production caller anywhere in the tree. Every bundled
  manifest advertises the same three capability strings, so wiring it up as-is
  would still be a no-op.
- InstallQualityFloorTier is a coding-QUALITY floor. It says nothing about
  whether a model can drive a tool-calling harness, which is a different axis
  and is currently unmeasured.

# What I still need to work out

- Whether the quality floor and the context floor should compose or whether one
  subsumes the other on small hosts.
- Which of the two failing selection tests encodes the intended behaviour.
