package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The MTP fields must serialise away when unset, so a consumer built against
// an older proto tag sees a byte-identical variant (additive-only proto
// contract, CLAUDE.md §Modules). Their tags are pinned because the bundled
// catalog and the control plane read them by name.
func TestMTPFieldsAreAbsentWhenUnsetAndNamedWhenSet(t *testing.T) {
	b, err := json.Marshal(Variant{VariantID: "v", Format: FormatSafetensors})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mtp_layers", "mtp_kv_bytes_per_token_fp16", "mtp_draft_tokens"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("%s must be omitempty; got %s", key, b)
		}
	}
	b, err = json.Marshal(Variant{VariantID: "v", Format: FormatSafetensors, MTPLayers: 1, MTPKVBytesPerTokenFP16: 4096, MTPDraftTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mtp_layers":1`, `"mtp_kv_bytes_per_token_fp16":4096`, `"mtp_draft_tokens":2`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
}

// VariantSHA's payload is frozen (its doc comment): an MTP choice must not
// make every persisted measurement stop matching.
func TestVariantSHA_IgnoresMTPFields(t *testing.T) {
	base := Variant{VariantID: "bf16", Format: FormatSafetensors, Source: VariantSource{Type: SourceHuggingFace, RepoID: "Qwen/Qwen3.5-4B"}}
	with := base
	with.MTPLayers, with.MTPKVBytesPerTokenFP16, with.MTPDraftTokens = 1, 4096, 1
	if VariantSHA(base) != VariantSHA(with) {
		t.Error("VariantSHA changed when only MTP fields were set")
	}
}

func TestMTPDraftTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    Variant
		want int
	}{
		{"vLLM build with a chosen draft", Variant{MTPLayers: 1, MTPKVBytesPerTokenFP16: 4096, MTPDraftTokens: 2}, 2},
		{"vLLM build with MTP layers, no choice", Variant{MTPLayers: 1, MTPKVBytesPerTokenFP16: 4096}, 0},
		{"vLLM build without MTP layers", Variant{MTPDraftTokens: 2}, 0},
		{"library tag sets its own draft", Variant{GGUF: &GGUFLayout{NextNLayers: 1, DraftMaxTokens: 4}}, 4},
		{"the tag's own draft wins over a stamp value", Variant{MTPDraftTokens: 2, GGUF: &GGUFLayout{NextNLayers: 1, DraftMaxTokens: 4}}, 4},
		{"stamped draft on a tag without one", Variant{MTPDraftTokens: 2, GGUF: &GGUFLayout{NextNLayers: 1}}, 2},
		{"no nextn blocks, nothing to draft with", Variant{MTPDraftTokens: 2, GGUF: &GGUFLayout{}}, 0},
		{"GGUF build without a draft", Variant{GGUF: &GGUFLayout{NextNLayers: 1}}, 0},
	} {
		if got := MTPDraftTokens(tc.v); got != tc.want {
			t.Errorf("%s: MTPDraftTokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestValidateMTP(t *testing.T) {
	st := Variant{VariantID: "bf16", Format: FormatSafetensors}
	tag := Variant{VariantID: "q2-gguf", Format: FormatOllamaTag}
	for _, tc := range []struct {
		name string
		v    Variant
		ok   bool
	}{
		{"nothing set", st, true},
		{"vLLM facts and a draft", with(st, func(v *Variant) { v.MTPLayers, v.MTPKVBytesPerTokenFP16, v.MTPDraftTokens = 1, 4096, 1 }), true},
		{"vLLM facts without a draft", with(st, func(v *Variant) { v.MTPLayers, v.MTPKVBytesPerTokenFP16 = 1, 4096 }), true},
		{"layers without their KV price", with(st, func(v *Variant) { v.MTPLayers = 1 }), false},
		{"KV price without layers", with(st, func(v *Variant) { v.MTPKVBytesPerTokenFP16 = 4096 }), false},
		{"a draft on a vLLM build without layers", with(st, func(v *Variant) { v.MTPDraftTokens = 1 }), false},
		{"negative draft", with(st, func(v *Variant) { v.MTPDraftTokens = -1 }), false},
		{"checkpoint facts on an ollama build", with(tag, func(v *Variant) { v.MTPLayers, v.MTPKVBytesPerTokenFP16 = 1, 4096 }), false},
		{"stamp on a tag with nextn blocks", with(tag, func(v *Variant) { v.MTPDraftTokens = 2; v.GGUF = &GGUFLayout{NextNLayers: 1} }), true},
		{"stamp on a tag without nextn blocks", with(tag, func(v *Variant) { v.MTPDraftTokens = 2; v.GGUF = &GGUFLayout{} }), false},
		{"stamp on a tag with no layout", with(tag, func(v *Variant) { v.MTPDraftTokens = 2 }), false},
		{"stamp on a tag that already sets its draft", with(tag, func(v *Variant) { v.MTPDraftTokens = 2; v.GGUF = &GGUFLayout{NextNLayers: 1, DraftMaxTokens: 2} }), false},
	} {
		err := validateMTP("m", tc.v)
		if (err == nil) != tc.ok {
			t.Errorf("%s: validateMTP err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func with(v Variant, f func(*Variant)) Variant { f(&v); return v }
