package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
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

// TestReadPrefix covers the control plane's use: the first few kilobytes of
// a GGUF, fetched with a Range request, cut somewhere inside the tokenizer
// arrays (waired-ai/waired#1476).
func TestReadPrefix(t *testing.T) {
	var b headerBuilder
	b.text("general.architecture", "llama")
	b.u32("llama.block_count", 28)
	b.u32("llama.context_length", 40960)
	b.u32("llama.attention.head_count_kv", 8)
	b.strs("tokenizer.ggml.tokens", []string{strings.Repeat("x", 4000), strings.Repeat("y", 4000)})
	b.u32("llama.after_tokens", 1)
	b.tensor("token_embd.weight", 12, 2048, 151936)
	full := b.bytes()

	h, err := ReadPrefix(bytes.NewReader(full))
	if err != nil || !h.Complete {
		t.Fatalf("whole header: complete=%v err=%v", h.Complete, err)
	}
	if full, err := Read(bytes.NewReader(full)); err != nil || !full.Complete {
		t.Fatalf("Read of a whole header: complete=%v err=%v", full.Complete, err)
	}

	cut := full[:2048] // inside the first token string
	h, err = ReadPrefix(bytes.NewReader(cut))
	if err != nil {
		t.Fatalf("ReadPrefix of a cut header: %v", err)
	}
	if h.Complete {
		t.Error("a cut header reported complete")
	}
	if h.Architecture() != "llama" {
		t.Errorf("architecture = %q", h.Architecture())
	}
	if n, ok := h.ArchUint("context_length"); !ok || n != 40960 {
		t.Errorf("context_length = %d, %v", n, ok)
	}
	if n, ok := h.ArchUint("attention.head_count_kv"); !ok || n != 8 {
		t.Errorf("head_count_kv = %d, %v", n, ok)
	}
	if _, ok := h.ArchUint("after_tokens"); ok {
		t.Error("a key past the cut was reported")
	}
	if len(h.Tensors) != 0 {
		t.Errorf("tensors = %d, want none from a cut header", len(h.Tensors))
	}

	if _, err := Read(bytes.NewReader(cut)); err == nil {
		t.Error("Read accepted a cut header")
	}
	if _, err := ReadPrefix(strings.NewReader("NOTAGGUF")); !errors.Is(err, ErrNotGGUF) {
		t.Errorf("wrong magic: %v, want ErrNotGGUF", err)
	}
	if _, err := ReadPrefix(bytes.NewReader(full[:3])); err == nil {
		t.Error("a stream shorter than the magic was accepted")
	}
}

// TestReadRefusesCountsNoModelHas: the counts at the top of a header are
// whatever the file says, and the control plane reads the header of a file
// any signed-in person names. A header that claims 2^40 tensors used to
// reserve a slice that size before reading one — a crafted 16 KB prefix
// could take the control plane's memory (review of waired-ai/waired#1473,
// 2026-09-22). Both counts are now an error, for the whole header and for a
// prefix.
func TestReadRefusesCountsNoModelHas(t *testing.T) {
	var b headerBuilder
	b.text("general.architecture", "llama")
	b.u32("llama.block_count", 28)
	good := b.bytes()

	for name, patch := range map[string]func([]byte){
		"tensors": func(h []byte) { binary.LittleEndian.PutUint64(h[8:16], 1<<40) },
		"keys":    func(h []byte) { binary.LittleEndian.PutUint64(h[16:24], 1<<40) },
	} {
		t.Run(name, func(t *testing.T) {
			hostile := append([]byte(nil), good...)
			patch(hostile)
			if _, err := Read(bytes.NewReader(hostile)); err == nil {
				t.Error("Read accepted the count")
			}
			if _, err := ReadPrefix(bytes.NewReader(hostile)); err == nil {
				t.Error("ReadPrefix accepted the count")
			}
		})
	}

	// The bound is not tight against real files: the largest count a
	// published build carries is far below it.
	var ok headerBuilder
	ok.text("general.architecture", "llama")
	for i := range 3000 {
		ok.tensor(fmt.Sprintf("blk.%d.attn_q.weight", i), 12, 256, 256)
	}
	h, err := Read(bytes.NewReader(ok.bytes()))
	if err != nil || len(h.Tensors) != 3000 {
		t.Fatalf("3,000 tensors: %d, %v", len(h.Tensors), err)
	}
}

// FuzzReadPrefix holds the reader to one property on any input: it returns,
// with an error or a header, and never panics. The seeds are a real-shaped
// header and its cuts; `go test -fuzz` explores from there.
func FuzzReadPrefix(f *testing.F) {
	var b headerBuilder
	b.text("general.architecture", "llama")
	b.u32("llama.block_count", 28)
	b.i32s("llama.attention.head_count_kv", []int32{8, 8, 8, 8})
	b.strs("tokenizer.ggml.tokens", []string{"a", "b"})
	b.tensor("token_embd.weight", 12, 2048, 151936)
	full := b.bytes()
	f.Add(full)
	f.Add(full[:40])
	f.Add([]byte("GGUF"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadPrefix(bytes.NewReader(data))
		_, _ = Read(bytes.NewReader(data))
	})
}
