package download

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSized(t *testing.T, dir, rel string, n int) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The partials huggingface_hub leaves when its process is killed are never
// resumed (waired-agent#1519), so the sweep takes every one of them — in
// subdirectories too, where a file of a repository subdirectory is parked —
// and nothing else: the .metadata files let the next run skip files that
// did land, and the landed files are the model.
func TestSweepHFIncomplete(t *testing.T) {
	dir := t.TempDir()
	top := writeSized(t, dir, ".cache/huggingface/download/Y6g195.etag.f3c513c9.incomplete", 100)
	top2 := writeSized(t, dir, ".cache/huggingface/download/Y6g195.etag.b5242aa3.incomplete", 50)
	sub := writeSized(t, dir, ".cache/huggingface/download/original/D5-QTa.etag.59ac15f7.incomplete", 25)
	meta := writeSized(t, dir, ".cache/huggingface/download/model-00003-of-00003.safetensors.metadata", 1)
	landed := writeSized(t, dir, "model-00003-of-00003.safetensors", 7)

	removed, bytes, err := SweepHFIncomplete(dir)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 3 || bytes != 175 {
		t.Errorf("removed %d files / %d bytes, want 3 / 175", removed, bytes)
	}
	for _, p := range []string{top, top2, sub} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", filepath.Base(p))
		}
	}
	for _, p := range []string{meta, landed} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed; only partials may go", filepath.Base(p))
		}
	}

	// Nothing downloaded yet, or not a vLLM host: no cache directory.
	if n, b, err := SweepHFIncomplete(t.TempDir()); err != nil || n != 0 || b != 0 {
		t.Errorf("sweep of a directory with no cache = %d, %d, %v; want 0, 0, nil", n, b, err)
	}
}
