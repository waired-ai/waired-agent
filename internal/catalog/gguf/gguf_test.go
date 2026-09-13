package gguf

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadHeader decodes a synthetic header shaped like a qwen35 build:
// scalars, a per-layer numeric array (kept), a tokenizer string array
// (skipped), and a tensor table whose byte sizes come from ggml's block
// layout. token_embd at Q4_K for a 248,320 × 5,120 table is 682.03 MiB,
// the CPU_Mapped buffer llama.cpp logs for the dense 27B MTP build.
func TestReadHeader(t *testing.T) {
	var b headerBuilder
	b.text("general.architecture", "qwen35")
	b.u32("qwen35.block_count", 65)
	b.u32("qwen35.nextn_predict_layers", 1)
	b.f32("qwen35.rope.freq_base", 1e7)
	b.i32s("qwen35.attention.head_count_kv", []int32{0, 0, 0, 4, 0, 0, 0, 4})
	b.strs("tokenizer.ggml.tokens", []string{"<|endoftext|>", "hello", strings.Repeat("x", 300)})
	b.tensor("token_embd.weight", 12, 5120, 248320)
	b.tensor("blk.0.ffn_up.weight", 8, 5120, 17408)
	b.tensor("blk.64.nextn.eh_proj.weight", 1, 10240, 5120)

	// Trailing bytes stand in for tensor data the reader must never touch.
	h, err := Read(io.MultiReader(bytes.NewReader(b.bytes()), errReader{}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if h.Architecture() != "qwen35" {
		t.Errorf("architecture = %q", h.Architecture())
	}
	if n, ok := h.ArchUint("block_count"); !ok || n != 65 {
		t.Errorf("block_count = %d, %v", n, ok)
	}
	if got := h.Arrays["qwen35.attention.head_count_kv"]; len(got) != 8 || got[3] != 4 {
		t.Errorf("head_count_kv = %v, want the per-layer array", got)
	}
	if _, kept := h.Arrays["tokenizer.ggml.tokens"]; kept {
		t.Error("string arrays must be skipped, not kept")
	}
	embd, err := h.TensorBytes(func(n string) bool { return n == "token_embd.weight" })
	if err != nil || embd != 715161600 {
		t.Errorf("token_embd bytes = %d, %v; want 715161600 (682.03 MiB at Q4_K)", embd, err)
	}
	up, _ := h.TensorBytes(func(n string) bool { return n == "blk.0.ffn_up.weight" })
	if want := uint64(5120*17408) / 32 * 34; up != want {
		t.Errorf("Q8_0 tensor bytes = %d, want %d", up, want)
	}
	if !h.HasTensorPrefix("blk.64.nextn") {
		t.Error("HasTensorPrefix missed the nextn tensor")
	}
}

// errReader fails any read: reaching it means the reader went past the
// tensor table.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read past the header") }

func TestReadRejectsWhatIsNotAHeader(t *testing.T) {
	if _, err := Read(strings.NewReader("GGML....")); !errors.Is(err, ErrNotGGUF) {
		t.Errorf("wrong magic: err = %v, want ErrNotGGUF", err)
	}
	var b headerBuilder
	b.tensor("x", 99, 32)
	h, err := Read(bytes.NewReader(b.bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, err := h.Tensors[0].Bytes(); err == nil {
		t.Error("an unknown ggml type must be an error, not a guessed size")
	}
	if _, err := Read(bytes.NewReader(b.bytes()[:20])); err == nil {
		t.Error("a truncated header must be an error")
	}
}
