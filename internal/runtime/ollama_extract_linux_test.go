//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// stageArchive writes body to a temp file and returns its path, since the
// extractor reads the archive from disk (#492 staged the download so the
// Windows ZIP reader could seek, and so a ~1.4 GB transfer stopped living
// in resident memory).
func stageArchive(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.zst")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExtractOllamaArchive exercises the real extractor end-to-end with a
// synthetic archive: in-process zstd decode streamed into the system tar
// must materialise regular files AND symlinks (the ollama release layout
// uses soname symlinks under lib/ollama).
func TestExtractOllamaArchive(t *testing.T) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("#!/bin/sh\necho fake-ollama\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "bin/ollama", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "lib/libfoo.so", Linkname: "../bin/ollama", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var zstBuf bytes.Buffer
	zw, err := zstd.NewWriter(&zstBuf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractOllamaArchive(stageArchive(t, zstBuf.Bytes()), dest, true); err != nil {
		t.Fatalf("extractOllamaArchive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "bin", "ollama"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("bin/ollama content mismatch: err=%v got=%q", err, got)
	}
	if fi, err := os.Stat(filepath.Join(dest, "bin", "ollama")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("bin/ollama mode = %v err=%v, want 0755", fi.Mode(), err)
	}
	if link, err := os.Readlink(filepath.Join(dest, "lib", "libfoo.so")); err != nil || link != "../bin/ollama" {
		t.Errorf("symlink = %q err=%v, want ../bin/ollama", link, err)
	}
}

// TestExtractOllamaArchive_NotZstd: a non-zstd payload (e.g. the darwin
// .tgz fetched by mistake, or an HTML error page) must fail loudly instead
// of feeding garbage to tar.
func TestExtractOllamaArchive_NotZstd(t *testing.T) {
	if err := extractOllamaArchive(stageArchive(t, []byte("<html>not a release</html>")), t.TempDir(), true); err == nil {
		t.Fatal("expected an error for non-zstd input")
	}
}

// A missing archive is an error from the extractor, not a panic.
func TestExtractOllamaArchive_Missing(t *testing.T) {
	if err := extractOllamaArchive(filepath.Join(t.TempDir(), "absent.tar.zst"), t.TempDir(), true); err == nil {
		t.Fatal("expected an error for a missing archive")
	}
}
