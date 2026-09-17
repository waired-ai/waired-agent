package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// max_parallel must serialise away when unset, so a consumer built against
// an older proto tag sees a byte-identical variant (additive-only proto
// contract, CLAUDE.md §Modules). The name is pinned because the bundled
// catalog and the control plane read it by name.
func TestMaxParallelIsAbsentWhenUnsetAndNamedWhenSet(t *testing.T) {
	b, err := json.Marshal(Variant{VariantID: "v", Format: FormatOllamaTag})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"max_parallel"`) {
		t.Errorf("max_parallel must be omitempty; got %s", b)
	}
	b, err = json.Marshal(Variant{VariantID: "v", Format: FormatOllamaTag, MaxParallel: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"max_parallel":1`) {
		t.Errorf(`missing "max_parallel":1 in %s`, b)
	}
}

// VariantSHA's payload is frozen (its doc comment): a limit must not make
// every persisted measurement stop matching.
func TestVariantSHA_IgnoresMaxParallel(t *testing.T) {
	base := Variant{VariantID: "q4-gguf", Format: FormatOllamaTag, Source: VariantSource{Type: SourceOllama, Tag: "qwen3.5:9b-q4_K_M"}}
	capped := base
	capped.MaxParallel = 1
	if VariantSHA(base) != VariantSHA(capped) {
		t.Error("VariantSHA changed when only max_parallel was set")
	}
}

func TestValidateMaxParallel(t *testing.T) {
	manifest := func(v Variant) Manifest {
		return Manifest{ModelID: "m", ContextLength: 262144, Variants: []Variant{v}}
	}
	tag := Variant{VariantID: "q4-gguf", Format: FormatOllamaTag, RuntimeSupport: []string{RuntimeOllama},
		QualityTier: 50, ParamCount: 9e9, QuantizationTier: 4, Source: VariantSource{Type: SourceOllama, Tag: "qwen3.5:9b-q4_K_M"}}
	gguf := Variant{VariantID: "q4", Format: FormatGGUF, RuntimeSupport: []string{RuntimeOllama},
		QualityTier: 50, ParamCount: 9e9, QuantizationTier: 4}
	st := Variant{VariantID: "bf16", Format: FormatSafetensors, RuntimeSupport: []string{RuntimeVLLM},
		QualityTier: 50, ParamCount: 9e9, QuantizationTier: 8, Source: VariantSource{Type: SourceHuggingFace, RepoID: "Qwen/Qwen3.5-9B"}}
	for _, tc := range []struct {
		name string
		v    Variant
		ok   bool
	}{
		{"unset on an ollama tag", tag, true},
		{"one on an ollama tag", with(tag, func(v *Variant) { v.MaxParallel = 1 }), true},
		{"one on a gguf build", with(gguf, func(v *Variant) { v.MaxParallel = 1 }), true},
		{"negative", with(tag, func(v *Variant) { v.MaxParallel = -1 }), false},
		{"one on a vLLM build", with(st, func(v *Variant) { v.MaxParallel = 1 }), false},
		{"unset on a vLLM build", st, true},
	} {
		m := manifest(tc.v)
		err := m.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestServedMaxParallelIn(t *testing.T) {
	ms := []Manifest{
		{ModelID: "capped", ModelAliases: []string{"capped-alias"}, Variants: []Variant{
			{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}, MaxParallel: 1},
			{VariantID: "q2", RuntimeSupport: []string{RuntimeOllama}, MaxParallel: 1},
			{VariantID: "fp8", RuntimeSupport: []string{RuntimeVLLM}},
		}},
		{ModelID: "mixed", Variants: []Variant{
			{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}, MaxParallel: 1},
			{VariantID: "q2", RuntimeSupport: []string{RuntimeOllama}},
		}},
		{ModelID: "free", Variants: []Variant{
			{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}},
		}},
	}
	for _, tc := range []struct {
		name, engine, model, variant string
		want                         int
	}{
		{"the served build's limit", RuntimeOllama, "capped", "q4", 1},
		{"resolved through an alias", RuntimeOllama, "capped-alias", "q2", 1},
		{"vLLM serving the same model", RuntimeVLLM, "capped", "fp8", 0},
		{"vLLM with no variant reported", RuntimeVLLM, "capped", "", 0},
		{"no variant reported, every ollama build agrees", RuntimeOllama, "capped", "", 1},
		{"a variant this catalog does not know, every build agrees", RuntimeOllama, "capped", "q3", 1},
		{"no variant reported, the builds disagree", RuntimeOllama, "mixed", "", 0},
		{"the uncapped build of a mixed model", RuntimeOllama, "mixed", "q2", 0},
		{"the capped build of a mixed model", RuntimeOllama, "mixed", "q4", 1},
		{"a model with no limit", RuntimeOllama, "free", "q4", 0},
		{"an unknown model", RuntimeOllama, "nope", "q4", 0},
		{"no engine reported", "", "capped", "q4", 0},
	} {
		if got := ServedMaxParallelIn(ms, tc.engine, tc.model, tc.variant); got != tc.want {
			t.Errorf("%s: ServedMaxParallelIn(%q, %q, %q) = %d, want %d", tc.name, tc.engine, tc.model, tc.variant, got, tc.want)
		}
	}
}

// The bundled values (waired-ai/waired-agent#1423, owner decision
// 2026-09-17): ollama v0.34.0 starts the qwen35 and qwen35moe families with
// one slot, and every ollama build of Qwen 3.5, 3.6 and 3.8 27B is one of
// them. flash-next (qwen4exp) and granite4-350m (granite) are not, and the
// same models on vLLM batch. The families were read from each tag's config
// blob; the agent's catalog-sources integration test re-reads them.
func TestBundledMaxParallel(t *testing.T) {
	want := map[string]map[string]int{
		"qwen3.5-0.8b":       {"q8-gguf": 1},
		"qwen3.5-2b":         {"q4-gguf": 1},
		"qwen3.5-4b":         {"q4-gguf": 1},
		"qwen3.5-9b":         {"q4-gguf": 1},
		"qwen3.5-27b":        {"q4-gguf": 1},
		"qwen3.5-35b-a3b":    {"q4-gguf": 1},
		"qwen3.5-122b-a10b":  {"q4-gguf": 1},
		"qwen3.6-27b":        {"mtp-q4-gguf": 1, "q4-gguf": 1},
		"qwen3.6-35b-a3b":    {"mtp-q4-gguf": 1, "q4-gguf": 1, "mtp-q3-gguf": 1, "mtp-q2-gguf": 1},
		"qwen3.8-27b":        {"mtp-q4-gguf": 1, "q3-gguf": 1, "q2-gguf": 1},
		"qwen3.8-flash-next": {"q2-gguf": 0},
		"granite4-350m":      {"bf16-gguf": 0},
	}
	ms, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range ms {
		for _, v := range m.Variants {
			w, listed := want[m.ModelID][v.VariantID]
			if !supportsRuntime(v.RuntimeSupport, RuntimeOllama) {
				if v.MaxParallel != 0 {
					t.Errorf("%s/%s: vLLM build carries max_parallel %d", m.ModelID, v.VariantID, v.MaxParallel)
				}
				continue
			}
			if !listed {
				t.Errorf("%s/%s: an ollama build this test does not list; read its tag's model_family and add it", m.ModelID, v.VariantID)
				continue
			}
			seen++
			if v.MaxParallel != w {
				t.Errorf("%s/%s: max_parallel = %d, want %d", m.ModelID, v.VariantID, v.MaxParallel, w)
			}
			if got := ServedMaxParallel(RuntimeOllama, m.ModelID, v.VariantID); got != w {
				t.Errorf("ServedMaxParallel(ollama, %s, %s) = %d, want %d", m.ModelID, v.VariantID, got, w)
			}
		}
	}
	if seen != 18 {
		t.Errorf("checked %d ollama builds, want 18", seen)
	}
}
