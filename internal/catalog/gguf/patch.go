package gguf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNotUint32 is returned when a key exists but is not stored as a UINT32,
// so it cannot be rewritten without moving everything after it.
var ErrNotUint32 = errors.New("gguf: value is not a UINT32")

// ErrKeyAbsent is returned when the file carries no such key. A GGUF header
// is a flat list with no room to grow: adding a key shifts every tensor in
// the file, so this package will not do it.
var ErrKeyAbsent = errors.New("gguf: key is absent")

// SetArchUint32 rewrites one architecture-scoped UINT32 metadata value in
// place — "<general.architecture>.<suffix>", e.g. "qwen35moe.context_length".
// It returns the value that was there before.
//
// In place means exactly that: four bytes at a known offset, nothing else
// moved, no copy of the weights. That is the only kind of edit worth making
// to a file of this size, and it is available because the value is already a
// UINT32 in every build the catalog ships. A key that is absent or a
// different width is refused rather than worked around: growing the header
// would shift every tensor after it, which for a 20 GB build means writing
// 20 GB.
//
// The caller owns the question of whether editing this file is allowed at
// all. What this function guarantees is that a partial edit cannot go
// unnoticed: the value is read back from disk after the write and compared,
// so a torn or ignored write is an error rather than a file that looks fine
// and serves wrong.
func SetArchUint32(path, suffix string, want uint32) (old uint32, err error) {
	h, err := readFile(path)
	if err != nil {
		return 0, err
	}
	arch := h.Architecture()
	if arch == "" {
		return 0, fmt.Errorf("gguf: %s: no general.architecture", path)
	}
	key := arch + "." + suffix
	loc, ok := h.ScalarValueAt[key]
	if !ok {
		return 0, fmt.Errorf("gguf: %s: %s: %w", path, key, ErrKeyAbsent)
	}
	if loc.Type != typeUint32 {
		return 0, fmt.Errorf("gguf: %s: %s is type %d: %w", path, key, loc.Type, ErrNotUint32)
	}
	prev, _ := h.Uint(key)
	old = uint32(prev)
	if old == want {
		return old, nil
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return old, fmt.Errorf("gguf: %s: %w", path, err)
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], want)
	if _, err := f.WriteAt(b[:], loc.Offset); err != nil {
		f.Close()
		return old, fmt.Errorf("gguf: %s: write %s: %w", path, key, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return old, fmt.Errorf("gguf: %s: sync: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return old, fmt.Errorf("gguf: %s: close: %w", path, err)
	}

	// Read back. A write that did not land, or landed half, must not look
	// like success — the engine would then serve a window the product
	// believes it asked for and did not get.
	again, err := readFile(path)
	if err != nil {
		return old, fmt.Errorf("gguf: %s: re-read after write: %w", path, err)
	}
	got, ok := again.Uint(key)
	if !ok {
		return old, fmt.Errorf("gguf: %s: %s vanished after write", path, key)
	}
	if uint32(got) != want {
		return old, fmt.Errorf("gguf: %s: %s reads back as %d, wrote %d", path, key, got, want)
	}
	return old, nil
}

// ArchUint32 reads one architecture-scoped UINT32 without writing.
func ArchUint32(path, suffix string) (uint32, bool, error) {
	h, err := readFile(path)
	if err != nil {
		return 0, false, err
	}
	v, ok := h.ArchUint(suffix)
	return uint32(v), ok, nil
}

// HostResidentBytes is the size of the input-layer tensors llama.cpp keeps in
// system RAM on every load — token_embd and, where the model has them, the
// per-layer token embeddings — read off the file's tensor table. The same
// sum cmd/catalog-tool derives a bundled build's host_resident_weight_gb
// from; the agent reads it for a build whose manifest does not carry it (a
// custom model, waired-ai/waired#1481).
func HostResidentBytes(path string) (uint64, error) {
	h, err := readFile(path)
	if err != nil {
		return 0, err
	}
	return h.TensorBytes(func(name string) bool {
		return name == "token_embd.weight" || strings.HasPrefix(name, "per_layer_token_embd")
	})
}

func readFile(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, fmt.Errorf("gguf: %s: %w", path, err)
	}
	defer f.Close()
	h, err := Read(f)
	if err != nil {
		return Header{}, fmt.Errorf("gguf: %s: %w", path, err)
	}
	return h, nil
}
