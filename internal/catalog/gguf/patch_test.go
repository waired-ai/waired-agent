package gguf

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fixture writes a GGUF whose header looks like the qwen35moe builds the
// catalog ships, with some tensor-data bytes after it so a patch has
// something it could corrupt if it wrote to the wrong place.
func fixture(t *testing.T, ctx uint32) (path string, tail []byte) {
	t.Helper()
	var b headerBuilder
	b.text("general.architecture", "qwen35moe")
	b.u32("qwen35moe.block_count", 48)
	b.u32("qwen35moe.context_length", ctx)
	b.f32("qwen35moe.rope.freq_base", 1e7)
	b.text("general.name", "a name long enough to move the offsets around")
	b.u32("qwen35moe.attention.head_count", 64)
	b.tensor("blk.0.attn_q.weight", 0, 4096, 4096)

	tail = bytes.Repeat([]byte{0xAB}, 4096)
	path = filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, append(b.bytes(), tail...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, tail
}

func TestSetArchUint32(t *testing.T) {
	path, tail := fixture(t, 262144)

	old, err := SetArchUint32(path, "context_length", 1048576)
	if err != nil {
		t.Fatalf("SetArchUint32: %v", err)
	}
	if old != 262144 {
		t.Errorf("old = %d, want 262144", old)
	}

	got, ok, err := ArchUint32(path, "context_length")
	if err != nil || !ok {
		t.Fatalf("ArchUint32: %v ok=%v", err, ok)
	}
	if got != 1048576 {
		t.Errorf("context_length = %d, want 1048576", got)
	}

	// Nothing else moved. This is the whole claim of an in-place edit, and
	// it is the claim that matters for a 20 GB file: the neighbours keep
	// their values and the bytes after the header are untouched.
	h, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]uint64{
		"qwen35moe.block_count": 48, "qwen35moe.attention.head_count": 64,
	} {
		if v, _ := h.Uint(key); v != want {
			t.Errorf("%s = %d, want %d", key, v, want)
		}
	}
	if h.Architecture() != "qwen35moe" {
		t.Errorf("architecture = %q", h.Architecture())
	}
	if len(h.Tensors) != 1 || h.Tensors[0].Name != "blk.0.attn_q.weight" {
		t.Errorf("tensor table changed: %+v", h.Tensors)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[len(raw)-len(tail):], tail) {
		t.Error("bytes after the header changed")
	}
}

func TestSetArchUint32_Refusals(t *testing.T) {
	t.Run("absent key", func(t *testing.T) {
		path, _ := fixture(t, 262144)
		if _, err := SetArchUint32(path, "no_such_key", 1); !errors.Is(err, ErrKeyAbsent) {
			t.Errorf("err = %v, want ErrKeyAbsent — growing the header would move every tensor", err)
		}
	})

	t.Run("wrong width", func(t *testing.T) {
		// rope.freq_base is an f32: same four bytes, different meaning, and
		// writing an integer over it would be silent corruption.
		path, _ := fixture(t, 262144)
		if _, err := SetArchUint32(path, "rope.freq_base", 1); !errors.Is(err, ErrNotUint32) {
			t.Errorf("err = %v, want ErrNotUint32", err)
		}
		v, ok := func() (float64, bool) {
			h, err := readFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, ok := h.Scalars["qwen35moe.rope.freq_base"].(float64)
			return f, ok
		}()
		if !ok || v != 1e7 {
			t.Errorf("rope.freq_base = %v (ok=%v), want it untouched at 1e7", v, ok)
		}
	})

	t.Run("no architecture", func(t *testing.T) {
		var b headerBuilder
		b.u32("something.context_length", 262144)
		path := filepath.Join(t.TempDir(), "m.gguf")
		if err := os.WriteFile(path, b.bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := SetArchUint32(path, "context_length", 1048576); err == nil {
			t.Error("a file with no general.architecture was accepted")
		}
	})

	t.Run("not a gguf", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "m.gguf")
		if err := os.WriteFile(path, []byte("not a gguf at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := SetArchUint32(path, "context_length", 1048576); !errors.Is(err, ErrNotGGUF) {
			t.Errorf("err = %v, want ErrNotGGUF", err)
		}
	})
}

// Setting the value it already holds must not open the file for writing:
// the common case on a re-pull is "already done", and it should cost a read.
func TestSetArchUint32_AlreadyThere(t *testing.T) {
	path, _ := fixture(t, 1048576)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := SetArchUint32(path, "context_length", 1048576)
	if err != nil {
		t.Fatalf("SetArchUint32: %v", err)
	}
	if old != 1048576 {
		t.Errorf("old = %d", old)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("the file was rewritten when the value was already correct")
	}
}

// The input-layer weights llama.cpp keeps in system RAM, read off the tensor
// table: token_embd and the per-layer token embeddings, nothing else — the
// sum cmd/catalog-tool derives host_resident_weight_gb from
// (waired-ai/waired#1481).
func TestHostResidentBytes(t *testing.T) {
	var b headerBuilder
	b.text("general.architecture", "gemma3n")
	b.tensor("token_embd.weight", 1, 64, 1000)           // f16: 2 bytes
	b.tensor("per_layer_token_embd.weight", 1, 32, 1000) // f16
	b.tensor("blk.0.attn_q.weight", 1, 64, 64)           // not input-layer
	b.tensor("output.weight", 1, 64, 1000)               // the output head goes to the device
	path := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(path, b.bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HostResidentBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(64*1000*2 + 32*1000*2); got != want {
		t.Errorf("HostResidentBytes = %d, want %d", got, want)
	}
	if _, err := HostResidentBytes(filepath.Join(t.TempDir(), "missing.gguf")); err == nil {
		t.Error("a missing file read as a figure")
	}
}
