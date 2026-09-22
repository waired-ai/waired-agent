package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests pin the custom-model rules recorded as owner rulings in the
// private monorepo (docs/decisions/20260922/0300-custom-model-rulings.md)
// and the wire contract settled on waired-ai/waired#1475. They are product
// contracts: the control plane and every agent run the same checks, and a
// change here changes what either side accepts.

func validCustomOllama(t *testing.T) Manifest {
	t.Helper()
	id, err := MintCustomModelID("Qwen3-0.6B-GGUF Q4_K_M", CustomIdentity(RuntimeOllama, "unsloth/Qwen3-0.6B-GGUF", "Qwen3-0.6B-Q4_K_M.gguf", "sha256:"+strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	return Manifest{
		ModelID:       id,
		DisplayName:   "Qwen3 0.6B (unsloth Q4_K_M)",
		ContextLength: 40960,
		Runtime:       RuntimePolicy{Preferred: RuntimeOllama},
		Variants: []Variant{{
			VariantID:      "q4_k_m",
			Format:         FormatOllamaTag,
			Quantization:   "Q4_K_M",
			RuntimeSupport: []string{RuntimeOllama},
			Source: VariantSource{
				Type:     SourceOllama,
				Tag:      "hf.co/unsloth/Qwen3-0.6B-GGUF:Q4_K_M",
				Revision: strings.Repeat("b", 40),
				Digest:   "sha256:" + strings.Repeat("a", 64),
			},
			Renderer: "qwen3.5",
			Parser:   "qwen3.5",
		}},
		ManualOnly: "imported by a person (waired-ai/waired#1473)",
		Provenance: ProvenanceCustom,
	}
}

func validCustomVLLM(t *testing.T) Manifest {
	t.Helper()
	sha := strings.Repeat("c", 40)
	id, err := MintCustomModelID("Qwen3-8B-AWQ", CustomIdentity(RuntimeVLLM, "Qwen/Qwen3-8B-AWQ", "", sha))
	if err != nil {
		t.Fatal(err)
	}
	return Manifest{
		ModelID:       id,
		DisplayName:   "Qwen3 8B AWQ",
		ContextLength: 40960,
		Runtime:       RuntimePolicy{Preferred: RuntimeVLLM},
		Variants: []Variant{{
			VariantID:           "awq",
			Format:              FormatSafetensors,
			Quantization:        "awq",
			RuntimeSupport:      []string{RuntimeVLLM},
			Source:              VariantSource{Type: SourceHuggingFace, RepoID: "Qwen/Qwen3-8B-AWQ", Revision: sha},
			VLLMToolCallParser:  "hermes",
			VLLMReasoningParser: "qwen3",
		}},
		ManualOnly: "imported by a person (waired-ai/waired#1473)",
		Provenance: ProvenanceCustom,
	}
}

func allBundled(t *testing.T) []Manifest {
	t.Helper()
	ms, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestBundledNamesAvoidTheCustomPrefix(t *testing.T) {
	for _, m := range allBundled(t) {
		names := append([]string{m.ModelID, m.DisplayName}, m.ModelAliases...)
		for _, n := range names {
			if IsCustomModelID(strings.ToLower(n)) {
				t.Errorf("bundled %s: name %q starts with %q, which only custom models may use", m.ModelID, n, CustomModelIDPrefix)
			}
		}
		if m.Provenance != "" {
			t.Errorf("bundled %s: provenance %q, want empty", m.ModelID, m.Provenance)
		}
	}
	for _, r := range Retirements() {
		for _, n := range r.Names {
			if IsCustomModelID(strings.ToLower(n)) {
				t.Errorf("retired name %q starts with %q", n, CustomModelIDPrefix)
			}
		}
	}
}

func TestBundledManifestsAreNotCustom(t *testing.T) {
	bundled := allBundled(t)
	for _, m := range bundled {
		if err := ValidateCustomManifest(m, bundled); err == nil {
			t.Errorf("bundled %s passed ValidateCustomManifest", m.ModelID)
		}
	}
}

func TestBundledJSONCarriesNoProvenanceKey(t *testing.T) {
	for _, m := range allBundled(t) {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"provenance"`) || strings.Contains(string(b), `"vllm_tool_call_parser"`) || strings.Contains(string(b), `"vllm_reasoning_parser"`) {
			t.Errorf("bundled %s re-marshals with a custom-model key: %s", m.ModelID, b)
		}
	}
}

func TestCustomFieldsCanonicalJSON(t *testing.T) {
	m := Manifest{ModelID: "x", Provenance: ProvenanceCustom, Variants: []Variant{{VLLMToolCallParser: "hermes", VLLMReasoningParser: "qwen3"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"provenance":"custom"`, `"vllm_tool_call_parser":"hermes"`, `"vllm_reasoning_parser":"qwen3"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshal missing %s: %s", want, b)
		}
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Provenance != ProvenanceCustom || back.Variants[0].VLLMToolCallParser != "hermes" || back.Variants[0].VLLMReasoningParser != "qwen3" {
		t.Errorf("round trip lost a field: %+v", back)
	}
}

// TestCustomIdentityGolden pins the identity so that the control plane and
// every agent version mint the same id for the same content.
func TestCustomIdentityGolden(t *testing.T) {
	got := CustomIdentity("ollama", "Unsloth/Qwen3-0.6B-GGUF", "Qwen3-0.6B-Q4_K_M.gguf", "SHA256:ABC")
	if again := CustomIdentity("ollama", "unsloth/qwen3-0.6b-gguf", "Qwen3-0.6B-Q4_K_M.gguf", "sha256:abc"); again != got {
		t.Errorf("identity depends on repo or digest case: %s vs %s", got, again)
	}
	const want = "f08c8561225bb0e4c375dae458cffad9d2226a3e8dd77895975e4c55235f5828"
	if got != want {
		t.Errorf("CustomIdentity = %s, want %s (the pinned value; changing it re-ids every import)", got, want)
	}
	for _, other := range []string{
		CustomIdentity("vllm", "unsloth/Qwen3-0.6B-GGUF", "Qwen3-0.6B-Q4_K_M.gguf", "sha256:abc"),
		CustomIdentity("ollama", "unsloth/Qwen3-0.6B-GGUF", "Qwen3-0.6B-Q8_0.gguf", "sha256:abc"),
		CustomIdentity("ollama", "unsloth/Qwen3-0.6B-GGUF", "Qwen3-0.6B-Q4_K_M.gguf", "sha256:abd"),
	} {
		if other == got {
			t.Errorf("a different engine, file or digest produced the same identity")
		}
	}
}

func TestMintCustomModelID(t *testing.T) {
	ident := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct{ slug, want string }{
		{"Qwen3-0.6B-GGUF Q4_K_M", "custom-qwen3-0.6b-gguf-q4-k-m-01234567"},
		{"  ..weird__NAME//x  ", "custom-weird-name-x-01234567"},
		{"", "custom-model-01234567"},
		{"-", "custom-model-01234567"},
		{strings.Repeat("a", 200), "custom-" + strings.Repeat("a", 48) + "-01234567"},
	} {
		got, err := MintCustomModelID(tc.slug, ident)
		if err != nil {
			t.Errorf("%q: %v", tc.slug, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MintCustomModelID(%q) = %q, want %q", tc.slug, got, tc.want)
		}
		if len(got) > MaxCustomModelIDBytes || strings.Contains(got, "/") || !ValidCustomModelID(got) {
			t.Errorf("%q: minted %q is not a valid id", tc.slug, got)
		}
	}
	if _, err := MintCustomModelID("x", "0123"); err == nil {
		t.Error("a short identity was accepted")
	}
	if _, err := MintCustomModelID("x", "XYZ12345"); err == nil {
		t.Error("a non-hex identity was accepted")
	}
}

func TestValidCustomModelID(t *testing.T) {
	for _, id := range []string{"custom-a-01234567", "custom-qwen3.5-4b-deadbeef"} {
		if !ValidCustomModelID(id) {
			t.Errorf("%q rejected", id)
		}
	}
	for _, id := range []string{
		"qwen3.5-4b", "custom-01234567", "custom--a-01234567", "custom-a/b-01234567",
		"custom-A-01234567", "custom-a-0123456", "custom-a-0123456g", "custom-a..b-01234567",
		"custom-" + strings.Repeat("a", 49) + "-01234567",
	} {
		if ValidCustomModelID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestValidateCustomManifest(t *testing.T) {
	bundled := allBundled(t)
	for name, mk := range map[string]func(*testing.T) Manifest{"ollama": validCustomOllama, "vllm": validCustomVLLM} {
		if err := ValidateCustomManifest(mk(t), bundled); err != nil {
			t.Errorf("valid %s manifest refused: %v", name, err)
		}
	}
	someBundled := bundled[0]
	cases := map[string]func(m *Manifest){
		"no provenance":      func(m *Manifest) { m.Provenance = "" },
		"bundled-looking id": func(m *Manifest) { m.ModelID = "qwen3.5-4b" },
		"no manual_only":     func(m *Manifest) { m.ManualOnly = "" },
		"internal_only":      func(m *Manifest) { m.InternalOnly = "x" },
		"alias":              func(m *Manifest) { m.ModelAliases = []string{"q"} },
		"rope scaling": func(m *Manifest) {
			m.RopeScaling = &RopeScaling{Type: RopeScalingYaRN, Factor: 4, OriginalContextLength: 1}
		},
		"remote code":           func(m *Manifest) { m.Security.TrustRemoteCodeRequired = true },
		"two variants":          func(m *Manifest) { m.Variants = append(m.Variants, m.Variants[0]) },
		"no context length":     func(m *Manifest) { m.ContextLength = 0 },
		"no display name":       func(m *Manifest) { m.DisplayName = "" },
		"control in name":       func(m *Manifest) { m.DisplayName = "a\nb" },
		"bundled display name":  func(m *Manifest) { m.DisplayName = strings.ToUpper(someBundled.DisplayName) },
		"retired name":          func(m *Manifest) { m.DisplayName = "qwen2.5-coder-7b" },
		"tag not hf.co":         func(m *Manifest) { m.Variants[0].Source.Tag = "qwen3:0.6b" },
		"tag traversal":         func(m *Manifest) { m.Variants[0].Source.Tag = "hf.co/../x:Q4" },
		"tag leading dash":      func(m *Manifest) { m.Variants[0].Source.Tag = "hf.co/-x/y:Q4" },
		"tag newline":           func(m *Manifest) { m.Variants[0].Source.Tag = "hf.co/x/y:Q4\nFROM z" },
		"no digest":             func(m *Manifest) { m.Variants[0].Source.Digest = "" },
		"short revision":        func(m *Manifest) { m.Variants[0].Source.Revision = "main" },
		"renderer newline":      func(m *Manifest) { m.Variants[0].Renderer = "qwen\nPARAMETER x" },
		"parser leading dash":   func(m *Manifest) { m.Variants[0].Parser = "-x" },
		"vllm parser on ollama": func(m *Manifest) { m.Variants[0].VLLMToolCallParser = "hermes" },
		"mtp":                   func(m *Manifest) { m.Variants[0].MTPDraftTokens = 2 },
		"negative tier":         func(m *Manifest) { m.Variants[0].QualityTier = -1 },
		"default_variant":       func(m *Manifest) { m.DefaultVariant = map[string]string{RuntimeOllama: "q4_k_m"} },

		// Names that look like another one (review of waired-ai/waired#1473,
		// 2026-09-22): each renders as a bundled or teammate's name, or
		// reorders the text around it.
		"zero-width space in name": func(m *Manifest) { m.DisplayName = someBundled.DisplayName + "\u200b" },
		"bidi override in name":    func(m *Manifest) { m.DisplayName = "abc\u202edef" },
		"bidi isolate in name":     func(m *Manifest) { m.DisplayName = "abc\u2066def" },
		"line separator in name":   func(m *Manifest) { m.DisplayName = "abc\u2028def" },
		"private use in name":      func(m *Manifest) { m.DisplayName = "abc\ue000" },
		"bundled name, spaces doubled": func(m *Manifest) {
			m.DisplayName = strings.ReplaceAll(someBundledWithSpace(t, bundled).DisplayName, " ", "  ")
		},
		"bundled name, full-width space": func(m *Manifest) {
			m.DisplayName = strings.ReplaceAll(someBundledWithSpace(t, bundled).DisplayName, " ", "\u3000")
		},
		"license too long":  func(m *Manifest) { m.License = strings.Repeat("a", 65) },
		"license newline":   func(m *Manifest) { m.License = "mit\nx" },
		"license non-ascii": func(m *Manifest) { m.License = "licença" },
		"dtype flag":        func(m *Manifest) { m.Variants[0].DType = "--enforce-eager" },
	}
	for _, name := range []string{"カスタム\u3000モデル", "🚀 fast coder", "Qwen3 8B (mine)"} {
		m := validCustomOllama(t)
		m.DisplayName = name
		if err := ValidateCustomManifest(m, bundled); err != nil {
			t.Errorf("name %q refused: %v", name, err)
		}
	}
	for _, dt := range []string{"", "auto", "bfloat16"} {
		m := validCustomVLLM(t)
		m.Variants[0].DType = dt
		if err := ValidateCustomManifest(m, bundled); err != nil {
			t.Errorf("dtype %q refused: %v", dt, err)
		}
	}
	for name, mutate := range cases {
		m := validCustomOllama(t)
		mutate(&m)
		if err := ValidateCustomManifest(m, bundled); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	vcases := map[string]func(m *Manifest){
		"repo traversal": func(m *Manifest) { m.Variants[0].Source.RepoID = "../etc" },
		"repo no org":    func(m *Manifest) { m.Variants[0].Source.RepoID = "Qwen3-8B" },
		"repo and tag":   func(m *Manifest) { m.Variants[0].Source.Tag = "hf.co/x/y:q" },
		"bad parser":     func(m *Manifest) { m.Variants[0].VLLMToolCallParser = "hermes --x" },
		"no revision":    func(m *Manifest) { m.Variants[0].Source.Revision = "" },
		"gguf on vllm":   func(m *Manifest) { m.Variants[0].Format = FormatGGUF },
	}
	for name, mutate := range vcases {
		m := validCustomVLLM(t)
		mutate(&m)
		if err := ValidateCustomManifest(m, bundled); err == nil {
			t.Errorf("vllm %s: accepted", name)
		}
	}
}

// someBundledWithSpace is a bundled manifest whose display name has a space
// to double or widen.
func someBundledWithSpace(t *testing.T, bundled []Manifest) Manifest {
	t.Helper()
	for _, b := range bundled {
		if strings.Contains(b.DisplayName, " ") {
			return b
		}
	}
	t.Fatal("no bundled display name has a space")
	return Manifest{}
}

func TestCustomNameKey(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"Qwen3 8B", "qwen3  8b"},
		{"Qwen3 8B", "QWEN3\u30008B"},
		{"Qwen3 8B", " qwen3\t8b "},
		{"Qwen3 8B", "qwen3\u00a08b"},
	} {
		if CustomNameKey(tc.a) != CustomNameKey(tc.b) {
			t.Errorf("%q and %q compare different: %q vs %q", tc.a, tc.b, CustomNameKey(tc.a), CustomNameKey(tc.b))
		}
	}
	if CustomNameKey("Qwen3 8B") == CustomNameKey("Qwen3 8") {
		t.Error("different names compare equal")
	}
}

func TestValidateCustomProjection(t *testing.T) {
	bundled := allBundled(t)
	m := validCustomOllama(t)
	if err := ValidateCustomProjection(m, bundled); err == nil {
		t.Error("a full manifest passed as a teammate's copy")
	}
	v := &m.Variants[0]
	v.Source.Digest, v.Source.Revision, v.Renderer, v.Parser = "", "", "", ""
	if err := ValidateCustomProjection(m, bundled); err != nil {
		t.Errorf("routing-only copy refused: %v", err)
	}
	vm := validCustomVLLM(t)
	vv := &vm.Variants[0]
	vv.Source.Revision, vv.VLLMToolCallParser, vv.VLLMReasoningParser = "", "", ""
	if err := ValidateCustomProjection(vm, bundled); err != nil {
		t.Errorf("routing-only vLLM copy refused: %v", err)
	}
}

func TestParseHFTag(t *testing.T) {
	repo, quant, ok := ParseHFTag("hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL")
	if !ok || repo != "unsloth/Qwen3.8-27B-GGUF" || quant != "UD-Q3_K_XL" {
		t.Errorf("got %q %q %v", repo, quant, ok)
	}
	for _, bad := range []string{"", "hf.co/x/y", "huggingface.co/x/y:q", "hf.co/x:q", "hf.co/x/y/z:q", "hf.co/x/y:", "hf.co/x/y:q q"} {
		if _, _, ok := ParseHFTag(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestBundledSourceMatch(t *testing.T) {
	bundled := allBundled(t)
	var tag, want string
	for _, m := range bundled {
		for _, v := range m.Variants {
			if strings.HasPrefix(v.Source.Tag, "hf.co/") && tag == "" {
				tag, want = v.Source.Tag, m.ModelID
			}
		}
	}
	if tag == "" {
		t.Fatal("the bundled catalog carries no hf.co tag to match against")
	}
	if got, ok := BundledSourceMatch(RuntimeOllama, strings.ToUpper(tag[:6])+tag[6:], bundled); !ok || got != want {
		t.Errorf("BundledSourceMatch(%q) = %q, %v; want %q", tag, got, ok, want)
	}
	if _, ok := BundledSourceMatch(RuntimeOllama, "hf.co/someone/else:Q4_K_M", bundled); ok {
		t.Error("an unrelated tag matched")
	}
	if _, ok := BundledSourceMatch(RuntimeVLLM, tag, bundled); ok {
		t.Error("an ollama tag matched as a vLLM repository")
	}
}
