package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBundledManifests_Loads(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	// Auto-Selector ships 17 manifests (the original 5 coding families
	// plus the qwen3.5 lineup). Keep the floor at ≥6 so the test
	// doesn't regress trivially if one or two are temporarily removed.
	if len(ms) < 6 {
		t.Fatalf("Auto Selector catalog should ship ≥6 coding-model manifests, got %d", len(ms))
	}
	// The qwen3.5 lineup (added 2026-06-10) spans the hardware ladder
	// from 0.8B to 122B-A10B; pin the family's presence so a catalog
	// regeneration can't silently drop it.
	for _, id := range []string{"qwen3.5-2b", "qwen3.5-9b", "qwen3.5-27b", "qwen3.5-35b-a3b", "qwen3.5-122b-a10b"} {
		if _, ok := LookupByAlias(id, ms); !ok {
			t.Errorf("qwen3.5 lineup manifest %q missing from bundled catalog", id)
		}
	}
	// waired/default and waired/coding are dynamic aliases since #632:
	// the router resolves them to the host's current coding default
	// (preferred > active > bundled). No manifest may own them — a
	// static entry would shadow the dynamic resolution for hosts that
	// never pulled that manifest and re-break the default flow.
	for _, alias := range []string{"waired/default", "waired/coding"} {
		if m, ok := LookupByAlias(alias, ms); ok {
			t.Errorf("alias %q is statically owned by manifest %q; it must stay dynamic (#632)", alias, m.ModelID)
		}
	}
}

func TestBundledManifests_QualityTiersAscending(t *testing.T) {
	// quality_tier values must be unique across the whole catalog so the
	// model picker has an unambiguous ordering. Tier 0 means "unset" and is
	// rejected by Validate. CheckTierUniqueness is the shared implementation
	// (catalog-tool validate calls the same function).
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if err := CheckTierUniqueness(ms); err != nil {
		t.Error(err)
	}
}

func TestCheckTierUniqueness(t *testing.T) {
	mk := func(tier int) Manifest {
		return Manifest{ModelID: "m", Variants: []Variant{{VariantID: "v", QualityTier: tier}}}
	}
	if err := CheckTierUniqueness([]Manifest{mk(10), mk(20)}); err != nil {
		t.Errorf("distinct tiers rejected: %v", err)
	}
	if err := CheckTierUniqueness([]Manifest{mk(10), mk(10)}); err == nil {
		t.Error("duplicate tier accepted")
	}
	if err := CheckTierUniqueness([]Manifest{mk(0)}); err == nil {
		t.Error("out-of-range tier accepted")
	}
	if err := CheckTierUniqueness([]Manifest{mk(101)}); err == nil {
		t.Error("out-of-range tier accepted")
	}
}

func TestBundledManifests_AWQOrgConstraint(t *testing.T) {
	// AWQ variants must source from the official Qwen/* org so that
	// Step 2 doesn't pull community quantizations of unknown provenance.
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range ms {
		for _, v := range m.Variants {
			if !isAWQ(v.Quantization) {
				continue
			}
			if v.Source.Type != "huggingface" {
				t.Errorf("%s/%s AWQ variant must use source.type=huggingface, got %q", m.ModelID, v.VariantID, v.Source.Type)
				continue
			}
			if !strings.HasPrefix(v.Source.RepoID, "Qwen/") {
				t.Errorf("%s/%s AWQ source.repo_id %q must start with Qwen/", m.ModelID, v.VariantID, v.Source.RepoID)
			}
		}
	}
}

func TestBundledManifests_VLLMVariantsCarryHFRepo(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range ms {
		for _, v := range m.Variants {
			if !contains(v.RuntimeSupport, RuntimeVLLM) {
				continue
			}
			if v.Source.Type != "huggingface" {
				t.Errorf("%s/%s vllm variant: source.type must be huggingface, got %q", m.ModelID, v.VariantID, v.Source.Type)
			}
			if v.Source.RepoID == "" {
				t.Errorf("%s/%s vllm variant: source.repo_id required", m.ModelID, v.VariantID)
			}
			if v.EstimatedWeightGB <= 0 {
				t.Errorf("%s/%s vllm variant: estimated_weight_gb must be > 0", m.ModelID, v.VariantID)
			}
		}
	}
}

func TestValidate_MinEngineVersion(t *testing.T) {
	base := func(mev string) Manifest {
		return Manifest{
			ModelID: "x", ContextLength: 4096,
			Variants: []Variant{{
				VariantID: "v", Format: "ollama-tag",
				RuntimeSupport:   []string{"ollama"},
				Source:           VariantSource{Type: "ollama", Tag: "x:y"},
				QualityTier:      10,
				ParamCount:       1,
				QuantizationTier: 4,
				MinEngineVersion: mev,
			}},
		}
	}
	for _, ok := range []string{"", "0.30.0", "v0.30", "1.2.3-rc1"} {
		m := base(ok)
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(min_engine_version=%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"latest", "garbage", ">=0.30"} {
		m := base(bad)
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "min_engine_version") {
			t.Errorf("Validate(min_engine_version=%q) = %v, want min_engine_version error", bad, err)
		}
	}
}

// TestBundledManifests_MTPRequiresEngine030 pins the field that
// motivated min_engine_version: qwen3.6 mtp tags are refused
// server-side by Ollama < 0.30.
func TestBundledManifests_MTPRequiresEngine030(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range ms {
		if m.ModelID != "qwen3.6-27b" {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == "mtp-q4-gguf" {
				if v.MinEngineVersion != "0.30.0" {
					t.Errorf("qwen3.6-27b/mtp-q4-gguf min_engine_version = %q, want 0.30.0", v.MinEngineVersion)
				}
				return
			}
		}
		t.Fatal("qwen3.6-27b has no mtp-q4-gguf variant")
	}
	t.Fatal("qwen3.6-27b manifest not found")
}

func TestValidate_QualityTierRequired(t *testing.T) {
	m := Manifest{
		ModelID: "x", ContextLength: 4096,
		Variants: []Variant{{
			VariantID: "v", Format: "ollama-tag",
			RuntimeSupport: []string{"ollama"},
			Source:         VariantSource{Type: "ollama", Tag: "x:y"},
			// QualityTier omitted → defaults to 0
		}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "quality_tier") {
		t.Errorf("want quality_tier error, got %v", err)
	}
}

func TestValidate_AWQSourceConstraint(t *testing.T) {
	cases := []struct {
		name    string
		repoID  string
		wantErr bool
	}{
		{"official Qwen org", "Qwen/Qwen3-32B-Instruct-AWQ", false},
		{"community fork rejected", "TheBloke/Qwen3-32B-Instruct-AWQ", true},
		{"empty repo rejected", "", true},
		{"wrong prefix", "Qwen2/Qwen3-32B-Instruct-AWQ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Manifest{
				ModelID: "x", ContextLength: 4096,
				Variants: []Variant{{
					VariantID:         "awq",
					Format:            "safetensors",
					Quantization:      "AWQ-int4",
					RuntimeSupport:    []string{"vllm"},
					EstimatedWeightGB: 22.0,
					QualityTier:       50,
					ParamCount:        32_000_000_000,
					QuantizationTier:  4,
					Source:            VariantSource{Type: "huggingface", RepoID: c.repoID},
				}},
			}
			err := m.Validate()
			if c.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", c.repoID)
			}
			if !c.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", c.repoID, err)
			}
		})
	}
}

func TestValidate_SafetensorsRequiresHFRepo(t *testing.T) {
	m := Manifest{
		ModelID: "x", ContextLength: 4096,
		Variants: []Variant{{
			VariantID: "v", Format: "safetensors",
			RuntimeSupport: []string{"vllm"}, QualityTier: 50,
			EstimatedWeightGB: 16,
			ParamCount:        8_000_000_000,
			QuantizationTier:  8,
			Source:            VariantSource{Type: "huggingface" /* repo_id missing */},
		}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "repo_id") {
		t.Errorf("want repo_id error, got %v", err)
	}
}

func TestBundledManifests_AllValid(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range ms {
		if err := m.Validate(); err != nil {
			t.Errorf("manifest %q invalid: %v", m.ModelID, err)
		}
	}
}

func TestLookupByAlias(t *testing.T) {
	ms := []Manifest{
		{ModelID: "qwen3-8b-instruct", ModelAliases: []string{"waired/default", "waired/coding"}},
		{ModelID: "llama3-70b", ModelAliases: []string{"waired/large"}},
	}
	cases := []struct {
		in      string
		wantID  string
		wantHit bool
	}{
		{"waired/default", "qwen3-8b-instruct", true},
		{"waired/coding", "qwen3-8b-instruct", true},
		{"qwen3-8b-instruct", "qwen3-8b-instruct", true}, // exact model id
		{"waired/large", "llama3-70b", true},
		{"waired/missing", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := LookupByAlias(c.in, ms)
		if ok != c.wantHit {
			t.Errorf("LookupByAlias(%q) hit=%v, want %v", c.in, ok, c.wantHit)
			continue
		}
		if ok && got.ModelID != c.wantID {
			t.Errorf("LookupByAlias(%q) = %q, want %q", c.in, got.ModelID, c.wantID)
		}
	}
}

func TestValidate_NoVariants(t *testing.T) {
	m := Manifest{ModelID: "x"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "variant") {
		t.Errorf("want variant error, got %v", err)
	}
}

func TestValidate_VariantNoRuntimes(t *testing.T) {
	m := Manifest{ModelID: "x", Variants: []Variant{{VariantID: "v", Format: "gguf", QualityTier: 10}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "runtime_support") {
		t.Errorf("want runtime_support error, got %v", err)
	}
}

func TestValidate_FormatRuntimeMismatch(t *testing.T) {
	cases := []struct {
		format   string
		runtimes []string
		source   VariantSource
		wantErr  bool
	}{
		{"safetensors", []string{"vllm"}, VariantSource{Type: "huggingface", RepoID: "Qwen/x"}, false},
		{"safetensors", []string{"ollama"}, VariantSource{Type: "huggingface", RepoID: "Qwen/x"}, true},
		{"gguf", []string{"ollama"}, VariantSource{Type: "ollama", Tag: "qwen3:8b"}, false},
		{"gguf", []string{"vllm"}, VariantSource{Type: "ollama", Tag: "qwen3:8b"}, true},
		{"ollama-tag", []string{"ollama"}, VariantSource{Type: "ollama", Tag: "qwen3:8b"}, false},
		{"ollama-tag", []string{"vllm"}, VariantSource{Type: "ollama", Tag: "qwen3:8b"}, true},
	}
	for _, c := range cases {
		m := Manifest{
			ModelID: "x", ContextLength: 4096,
			Variants: []Variant{{
				VariantID: "v", Format: c.format, RuntimeSupport: c.runtimes,
				QualityTier: 10, EstimatedWeightGB: 1,
				ParamCount: 8_000_000_000, QuantizationTier: 4,
				Source: c.source,
			}},
		}
		err := m.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("format=%s runtimes=%v: err=%v wantErr=%v", c.format, c.runtimes, err, c.wantErr)
		}
	}
}

func TestManifest_JSONRoundTrip(t *testing.T) {
	m := Manifest{
		ModelID:       "x",
		ContextLength: 4096,
		Capabilities:  []string{"chat"},
		Runtime:       RuntimePolicy{Preferred: "ollama"},
		Variants: []Variant{{
			VariantID: "v", Format: "ollama-tag", QualityTier: 10,
			ParamCount: 8_000_000_000, QuantizationTier: 4,
			RuntimeSupport: []string{"ollama"},
			Source:         VariantSource{Type: "ollama", Tag: "x:y"},
		}},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Manifest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.ModelID != m.ModelID || back.Variants[0].Source.Tag != "x:y" {
		t.Errorf("round-trip lost data: %+v", back)
	}
	if back.Variants[0].ParamCount != 8_000_000_000 || back.Variants[0].QuantizationTier != 4 {
		t.Errorf("round-trip lost Phase 7 fields: ParamCount=%d QuantizationTier=%d",
			back.Variants[0].ParamCount, back.Variants[0].QuantizationTier)
	}
}

// TestValidate_ParamCountRequired enforces that variants must declare
// a positive parameter count. Phase 7 router scoring uses
// ParamCount × QuantizationTier as the primary mesh-peer ranking key;
// a zero param count would silently push the variant to the bottom of
// every ranking, masking real mis-configuration. Reject loudly at
// load time instead.
func TestValidate_ParamCountRequired(t *testing.T) {
	m := Manifest{
		ModelID: "x", ContextLength: 4096,
		Variants: []Variant{{
			VariantID: "v", Format: "ollama-tag",
			RuntimeSupport:   []string{"ollama"},
			Source:           VariantSource{Type: "ollama", Tag: "x:y"},
			QualityTier:      10,
			QuantizationTier: 4,
			// ParamCount omitted → defaults to 0
		}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "param_count") {
		t.Errorf("want param_count error, got %v", err)
	}
}

// TestValidate_QuantizationTierRange enforces [1, 8] on the new field.
// Out-of-range values would distort the Phase 7 score multiplicatively
// — easier to reject at load time than to clamp silently in the
// router and have to explain the deviation later.
func TestValidate_QuantizationTierRange(t *testing.T) {
	cases := []struct {
		name    string
		tier    int
		wantErr bool
	}{
		{"zero rejected", 0, true},
		{"min accepted", 1, false},
		{"q4 typical", 4, false},
		{"q8/fp16 cap", 8, false},
		{"above cap rejected", 9, true},
		{"negative rejected", -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Manifest{
				ModelID: "x", ContextLength: 4096,
				Variants: []Variant{{
					VariantID: "v", Format: "ollama-tag",
					RuntimeSupport:   []string{"ollama"},
					Source:           VariantSource{Type: "ollama", Tag: "x:y"},
					QualityTier:      10,
					ParamCount:       8_000_000_000,
					QuantizationTier: c.tier,
				}},
			}
			err := m.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("tier=%d: err=%v wantErr=%v", c.tier, err, c.wantErr)
			}
			if c.wantErr && err != nil && !strings.Contains(err.Error(), "quantization_tier") {
				t.Errorf("tier=%d: want quantization_tier error, got %v", c.tier, err)
			}
		})
	}
}

// TestBundledManifests_HavePhase7Fields verifies every bundled variant
// declares the Phase 7 score inputs. A merge that adds a manifest but
// forgets these fields would otherwise only fail in production at
// router-decision time, by which point the operator already sees the
// "wrong peer chosen" symptom and not the root cause.
func TestBundledManifests_HavePhase7Fields(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range ms {
		for _, v := range m.Variants {
			if v.ParamCount <= 0 {
				t.Errorf("%s/%s: param_count must be > 0, got %d", m.ModelID, v.VariantID, v.ParamCount)
			}
			if v.QuantizationTier < 1 || v.QuantizationTier > 8 {
				t.Errorf("%s/%s: quantization_tier must be in [1, 8], got %d", m.ModelID, v.VariantID, v.QuantizationTier)
			}
		}
	}
}

// TestValidate_ActiveParamsBounds enforces 0 ≤ ActiveParams ≤ ParamCount.
// MoE manifests must declare active_params for the Auto Selector's
// FLOPs-budget formula (decode FLOPs/tok = 2 × active_params); a value
// above ParamCount would point at a configuration error.
func TestValidate_ActiveParamsBounds(t *testing.T) {
	base := Variant{
		VariantID: "v", Format: "ollama-tag",
		RuntimeSupport:   []string{"ollama"},
		Source:           VariantSource{Type: "ollama", Tag: "x:y"},
		QualityTier:      10,
		ParamCount:       30_000_000_000,
		QuantizationTier: 4,
	}
	cases := []struct {
		name   string
		active int64
		want   bool
	}{
		{"zero ok (dense default)", 0, false},
		{"valid MoE active", 3_000_000_000, false},
		{"equal to total ok", 30_000_000_000, false},
		{"negative rejected", -1, true},
		{"above total rejected", 40_000_000_000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := base
			v.ActiveParams = c.active
			m := Manifest{ModelID: "x", ContextLength: 4096, Variants: []Variant{v}}
			err := m.Validate()
			if (err != nil) != c.want {
				t.Errorf("active=%d: err=%v wantErr=%v", c.active, err, c.want)
			}
		})
	}
}

// TestValidate_AttentionArchEnum rejects unknown attention topology tags.
// The Auto Selector branches on these to pick the right KV-cache
// scaling formula; a silent typo would otherwise produce the wrong
// context-budget calculation at runtime.
func TestValidate_AttentionArchEnum(t *testing.T) {
	cases := []struct {
		arch    string
		wantErr bool
	}{
		{"", false},
		{AttentionStandard, false},
		{AttentionGQA, false},
		{AttentionMLA, false},
		{AttentionHybridMamba, false},
		{AttentionSlidingWindow, false},
		{"banana", true},
		{"GQA", true}, // case-sensitive
	}
	for _, c := range cases {
		m := Manifest{
			ModelID: "x", ContextLength: 4096,
			Variants: []Variant{{
				VariantID: "v", Format: "ollama-tag",
				RuntimeSupport:   []string{"ollama"},
				Source:           VariantSource{Type: "ollama", Tag: "x:y"},
				QualityTier:      10,
				ParamCount:       8_000_000_000,
				QuantizationTier: 4,
				AttentionArch:    c.arch,
			}},
		}
		err := m.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("arch=%q: err=%v wantErr=%v", c.arch, err, c.wantErr)
		}
	}
}

// TestValidate_VendorSupportEnum rejects unknown status strings inside
// the VendorSupport matrix. Picker logic relies on the literal value
// "unsupported" to exclude variants; a typo'd status would silently
// pass that filter.
func TestValidate_VendorSupportEnum(t *testing.T) {
	mk := func(status string) Manifest {
		return Manifest{
			ModelID: "x", ContextLength: 4096,
			Variants: []Variant{{
				VariantID: "v", Format: "ollama-tag",
				RuntimeSupport:   []string{"ollama"},
				Source:           VariantSource{Type: "ollama", Tag: "x:y"},
				QualityTier:      10,
				ParamCount:       8_000_000_000,
				QuantizationTier: 4,
				VendorSupport: &VendorSupportMatrix{
					Mac: VendorRuntimeSupport{MLX: status},
				},
			}},
		}
	}
	for _, ok := range []string{"", VendorSupportStable, VendorSupportExperimental, VendorSupportCommunity, VendorSupportUnsupported} {
		m := mk(ok)
		if err := m.Validate(); err != nil {
			t.Errorf("valid status %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"yes", "STABLE", "broken"} {
		m := mk(bad)
		if err := m.Validate(); err == nil {
			t.Errorf("invalid status %q accepted", bad)
		}
	}
}

// TestBundledManifests_HFAliasesResolve verifies the Phase 5 alias
// extension: an external OpenAI-compat endpoint reporting an HF-style
// model id via /v1/models must resolve back to the matching waired
// manifest through LookupByAlias.
func TestBundledManifests_HFAliasesResolve(t *testing.T) {
	ms, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	cases := []struct {
		alias       string
		wantModelID string
	}{
		// waired/* aliases (operator-facing default tiers).
		// waired/default and waired/coding are router-resolved dynamic
		// aliases since #632 and deliberately absent here.
		{"waired/small", "qwen2.5-coder-3b-instruct"},
		{"waired/medium", "qwen2.5-coder-14b-instruct"},
		{"waired/flagship", "gpt-oss-120b"},
		{"waired/dense-large", "qwen3.6-27b"},
		{"waired/moe-small", "qwen3-coder-30b-a3b-instruct"},
		{"waired/moe-mid", "qwen3-coder-next-80b-a3b-instruct"},
		{"waired/moe-large", "qwen3-coder-480b-a35b-instruct"},
		{"waired/moe-mit", "glm-4.5-air-106b-a12b"},
		{"waired/moe-coding", "qwen3.6-35b-a3b"},
		{"waired/oss-small", "gpt-oss-20b"},
		// HF-style aliases (Phase 5 OpenAI-compat reverse lookup).
		{"Qwen/Qwen2.5-Coder-7B-Instruct", "qwen2.5-coder-7b-instruct"},
		{"Qwen/Qwen3-Coder-30B-A3B-Instruct", "qwen3-coder-30b-a3b-instruct"},
		{"openai/gpt-oss-20b", "gpt-oss-20b"},
		{"openai/gpt-oss-120b", "gpt-oss-120b"},
		{"zai-org/GLM-4.5-Air", "glm-4.5-air-106b-a12b"},
		// Short-form aliases operators commonly paste.
		{"qwen2.5-coder-3b", "qwen2.5-coder-3b-instruct"},
		{"qwen2.5-coder-7b", "qwen2.5-coder-7b-instruct"},
		{"qwen2.5-coder-14b", "qwen2.5-coder-14b-instruct"},
		{"qwen3-coder-30b-a3b", "qwen3-coder-30b-a3b-instruct"},
		{"qwen3-coder-next-80b-a3b", "qwen3-coder-next-80b-a3b-instruct"},
		{"qwen3-coder-480b-a35b", "qwen3-coder-480b-a35b-instruct"},
		{"qwen3.6-27b", "qwen3.6-27b"},
		{"qwen3.6-35b-a3b", "qwen3.6-35b-a3b"},
		{"Qwen/Qwen3.6-35B-A3B", "qwen3.6-35b-a3b"},
		{"gpt-oss-20b", "gpt-oss-20b"},
		{"gpt-oss-120b", "gpt-oss-120b"},
		{"glm-4.5-air", "glm-4.5-air-106b-a12b"},
	}
	for _, c := range cases {
		got, ok := LookupByAlias(c.alias, ms)
		if !ok {
			t.Errorf("LookupByAlias(%q) miss; want %q", c.alias, c.wantModelID)
			continue
		}
		if got.ModelID != c.wantModelID {
			t.Errorf("LookupByAlias(%q) = %q, want %q", c.alias, got.ModelID, c.wantModelID)
		}
	}
}
