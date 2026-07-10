//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// writeFakeOllama is an extractFn double: it materialises an executable
// bin/ollama under destDir as if a real tarball had been unpacked.
func writeFakeOllama(t *testing.T) func([]byte, string) error {
	t.Helper()
	return func(_ []byte, destDir string) error {
		binDir := filepath.Join(destDir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(binDir, "ollama"), []byte("#!/bin/sh\n"), 0o755)
	}
}

func TestOllamaInstaller_Install_Bundled(t *testing.T) {
	orig := ollamaTarballMinBytes
	ollamaTarballMinBytes = 4
	t.Cleanup(func() { ollamaTarballMinBytes = orig })

	inst := NewOllamaInstaller(t.TempDir())
	var gotURL string
	inst.downloadFn = func(_ context.Context, url string, _ func(int64, int64, int64)) ([]byte, error) {
		gotURL = url
		return []byte("BIGENOUGH"), nil
	}
	inst.extractFn = writeFakeOllama(t)

	if inst.Active() {
		t.Fatal("Active() true before install")
	}
	var stages []string
	if err := inst.Install(context.Background(), func(p OllamaInstallProgress) { stages = append(stages, p.Stage) }); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !inst.Active() {
		t.Errorf("Active() false after install; expected %s executable", inst.BinaryPath())
	}
	if !strings.Contains(gotURL, "ollama-linux-") {
		t.Errorf("download URL %q should reference the linux release asset", gotURL)
	}
	// CUDA/CPU host (no AMD): the rocm overlay stage must NOT run.
	for _, s := range stages {
		if s == "download-rocm" {
			t.Errorf("rocm overlay attempted on non-AMD host")
		}
	}
}

func TestOllamaInstaller_Install_AMDOverlay(t *testing.T) {
	orig := ollamaTarballMinBytes
	ollamaTarballMinBytes = 4
	t.Cleanup(func() { ollamaTarballMinBytes = orig })

	inst := NewOllamaInstaller(t.TempDir())
	inst.GPUVendor = "amd"
	var urls []string
	inst.downloadFn = func(_ context.Context, url string, _ func(int64, int64, int64)) ([]byte, error) {
		urls = append(urls, url)
		return []byte("BIGENOUGH"), nil
	}
	inst.extractFn = writeFakeOllama(t)
	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	sawRocm := false
	for _, u := range urls {
		if filepath.Base(u) == "ollama-linux-"+mustArch(t)+"-rocm.tar.zst" {
			sawRocm = true
		}
	}
	if !sawRocm {
		t.Errorf("AMD host should fetch the rocm overlay; urls=%v", urls)
	}
}

func TestOllamaInstaller_Install_TooSmall(t *testing.T) {
	inst := NewOllamaInstaller(t.TempDir())
	inst.downloadFn = func(_ context.Context, _ string, _ func(int64, int64, int64)) ([]byte, error) {
		return []byte("tiny"), nil
	}
	extracted := false
	inst.extractFn = func([]byte, string) error { extracted = true; return nil }
	if err := inst.Install(context.Background(), nil); err == nil {
		t.Fatal("expected error for suspiciously small tarball")
	}
	if extracted {
		t.Error("must not extract a too-small download")
	}
}

func TestOllamaInstaller_Install_DownloadError(t *testing.T) {
	inst := NewOllamaInstaller(t.TempDir())
	inst.downloadFn = func(context.Context, string, func(int64, int64, int64)) ([]byte, error) {
		return nil, errors.New("offline")
	}
	inst.extractFn = func([]byte, string) error { return nil }
	if err := inst.Install(context.Background(), nil); err == nil {
		t.Fatal("expected download error to propagate")
	}
}

// Install must forward downloadFn's byte updates to its progress callback
// stamped with the owning download stage, alongside the existing
// URL/stage announces.
func TestOllamaInstaller_Install_ForwardsByteProgress(t *testing.T) {
	orig := ollamaTarballMinBytes
	ollamaTarballMinBytes = 4
	t.Cleanup(func() { ollamaTarballMinBytes = orig })

	inst := NewOllamaInstaller(t.TempDir())
	inst.downloadFn = func(_ context.Context, _ string, onProgress func(int64, int64, int64)) ([]byte, error) {
		onProgress(5, 9, -1)
		onProgress(9, 9, 42)
		return []byte("BIGENOUGH"), nil
	}
	inst.extractFn = writeFakeOllama(t)

	var byteEvents []OllamaInstallProgress
	err := inst.Install(context.Background(), func(p OllamaInstallProgress) {
		if p.Completed > 0 || p.Total != 0 {
			byteEvents = append(byteEvents, p)
		}
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(byteEvents) != 2 {
		t.Fatalf("byte events = %+v, want 2", byteEvents)
	}
	want := OllamaInstallProgress{Stage: "download", Completed: 9, Total: 9, BytesPerSec: 42}
	if byteEvents[1] != want {
		t.Errorf("final byte event = %+v, want %+v", byteEvents[1], want)
	}
}

// httpGet must stream byte progress with the response Content-Length as
// total, ending on completed == total. (The reader itself is covered in
// internal/download/progress_test.go; this pins the installer's wiring.)
func TestOllamaInstaller_HTTPGetStreamsProgress(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 256<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	inst := NewOllamaInstaller(t.TempDir())
	var gotBytes, totals []int64
	got, err := inst.httpGet(context.Background(), srv.URL, func(c, tot, _ int64) {
		gotBytes, totals = append(gotBytes, c), append(totals, tot)
	})
	if err != nil || len(got) != len(body) {
		t.Fatalf("httpGet: err=%v got %d bytes, want %d", err, len(got), len(body))
	}
	if len(gotBytes) == 0 {
		t.Fatal("no progress emitted")
	}
	if last := gotBytes[len(gotBytes)-1]; last != int64(len(body)) {
		t.Errorf("final completed = %d, want %d", last, len(body))
	}
	for _, tot := range totals {
		if tot != int64(len(body)) {
			t.Errorf("total = %d, want %d (Content-Length)", tot, len(body))
		}
	}
}

// Without a Content-Length (chunked response) the total must degrade to -1
// while byte progress still streams.
func TestOllamaInstaller_HTTPGetUnknownLength(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	inst := NewOllamaInstaller(t.TempDir())
	var events int
	var lastCompleted, lastTotal int64
	got, err := inst.httpGet(context.Background(), srv.URL, func(c, tot, _ int64) {
		events++
		lastCompleted, lastTotal = c, tot
	})
	if err != nil || len(got) != len(body) {
		t.Fatalf("httpGet: err=%v got %d bytes, want %d", err, len(got), len(body))
	}
	if events == 0 {
		t.Fatal("no progress emitted")
	}
	if lastTotal != -1 {
		t.Errorf("total = %d, want -1 for an unknown length", lastTotal)
	}
	if lastCompleted != int64(len(body)) {
		t.Errorf("final completed = %d, want %d", lastCompleted, len(body))
	}
}

func TestOllamaVersionAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"0.24.0", "0.6.0", true},
		{"ollama version is 0.24.0", "0.6.0", true},
		{"0.6.0", "0.6.0", true},
		{"0.5.9", "0.6.0", false},
		{"v0.7.1-rc1", "0.6.0", true},
		{"0.6.3.post1", "0.6.0", true},
		{"garbage", "0.6.0", false},
	}
	for _, c := range cases {
		if got := OllamaVersionAtLeast(c.v, c.min); got != c.want {
			t.Errorf("ollamaVersionAtLeast(%q,%q)=%v want %v", c.v, c.min, got, c.want)
		}
	}
}

func mustArch(t *testing.T) string {
	t.Helper()
	a, err := ollamaLinuxArch()
	if err != nil {
		t.Skipf("unsupported arch: %v", err)
	}
	return a
}

// TestExtractTarZst exercises the real extractor end-to-end with a
// synthetic archive: in-process zstd decode streamed into the system
// tar must materialise regular files AND symlinks (the ollama release
// layout uses soname symlinks under lib/ollama).
func TestExtractTarZst(t *testing.T) {
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
	if err := extractTarZst(zstBuf.Bytes(), dest); err != nil {
		t.Fatalf("extractTarZst: %v", err)
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

// TestExtractTarZst_NotZstd: a non-zstd payload (e.g. the old .tgz or
// an HTML error page) must fail loudly instead of feeding garbage to tar.
func TestExtractTarZst_NotZstd(t *testing.T) {
	if err := extractTarZst([]byte("<html>not a release</html>"), t.TempDir()); err == nil {
		t.Fatal("expected an error for non-zstd input")
	}
}
