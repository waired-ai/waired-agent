package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The installer runs on all three OSes since #492, so this file carries no
// build tag: every case below asserts against the release table for the
// runner it happens to be on. The extractors themselves are per-OS and are
// tested in ollama_extract_<goos>_test.go.

// fakeExtract is an extractFn double. It materialises the engine binary
// where the real archive for this OS would have put it, and records the
// directory it was handed so a caller can check the extract root.
func fakeExtract(t *testing.T, inst *OllamaInstaller, gotDest *string) func(string, string, bool) error {
	t.Helper()
	return func(_, destDir string, _ bool) error {
		*gotDest = destDir
		bin := inst.BinaryPath()
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}
		return os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	}
}

// stubRelease wires the two network-facing seams to one fixed archive
// body, with a checksum list that agrees with it. It returns the URLs the
// installer asked for.
func stubRelease(t *testing.T, inst *OllamaInstaller, body []byte) *[]string {
	t.Helper()
	sum := sha256.Sum256(body)
	stubChecksums(t, inst, hex.EncodeToString(sum[:]))
	var urls []string
	inst.downloadFn = func(_ context.Context, url, destPath string, onProgress func(int64, int64, int64)) (int64, error) {
		urls = append(urls, url)
		if onProgress != nil {
			onProgress(int64(len(body)), int64(len(body)), 42)
		}
		if err := os.WriteFile(destPath, body, 0o600); err != nil {
			return 0, err
		}
		return int64(len(body)), nil
	}
	return &urls
}

// stubChecksums answers the checksum seam with hexsum for every asset this
// host's release entry names.
func stubChecksums(t *testing.T, inst *OllamaInstaller, hexsum string) {
	t.Helper()
	inst.checksumsFn = func(context.Context) (map[string]string, error) {
		rel := hostRelease(t)
		sums := map[string]string{rel.Base: hexsum}
		if rel.ROCm != "" {
			sums[rel.ROCm] = hexsum
		}
		return sums, nil
	}
}

func hostRelease(t *testing.T) ollamaRelease {
	t.Helper()
	rel, err := ollamaReleaseFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("ollamaReleaseFor(%s/%s): %v", runtime.GOOS, runtime.GOARCH, err)
	}
	return rel
}

// lowerFloor drops the size floor so the tiny fake archives below get past
// it; the checksum is what actually guards the content.
func lowerFloor(t *testing.T) {
	t.Helper()
	orig := ollamaArchiveMinBytes
	ollamaArchiveMinBytes = 4
	t.Cleanup(func() { ollamaArchiveMinBytes = orig })
}

func TestOllamaInstaller_Install_Bundled(t *testing.T) {
	lowerFloor(t)

	base := t.TempDir()
	inst := NewOllamaInstaller(base)
	urls := stubRelease(t, inst, []byte("BIGENOUGH"))
	var gotDest string
	inst.extractFn = fakeExtract(t, inst, &gotDest)

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

	rel := hostRelease(t)
	if len(*urls) != 1 || filepath.Base((*urls)[0]) != rel.Base {
		t.Errorf("download URLs = %v, want one ending in %s", *urls, rel.Base)
	}
	// The extract root is the whole reason ollamaRelease.ExtractSub exists:
	// two of the three archives carry their payload at the archive root and
	// must land under bin/ for the binary to end up where the daemon looks.
	wantDest := base
	if rel.ExtractSub != "" {
		wantDest = filepath.Join(base, rel.ExtractSub)
	}
	if gotDest != wantDest {
		t.Errorf("extract dir = %q, want %q", gotDest, wantDest)
	}
	// CUDA/CPU host (no AMD): the rocm overlay stage must NOT run.
	for _, s := range stages {
		if s == "download-rocm" {
			t.Errorf("rocm overlay attempted on non-AMD host")
		}
	}
	// The staging directory is transient; leaving it behind is what cost
	// ~1.4 GB per killed install before #191.
	if _, err := os.Stat(filepath.Join(base, ollamaStageDirName)); !os.IsNotExist(err) {
		t.Errorf("staging dir survived a successful install (err=%v)", err)
	}
}

func TestOllamaInstaller_Install_AMDOverlay(t *testing.T) {
	lowerFloor(t)

	inst := NewOllamaInstaller(t.TempDir())
	inst.WantROCmOverlay = true
	urls := stubRelease(t, inst, []byte("BIGENOUGH"))
	var dest string
	inst.extractFn = fakeExtract(t, inst, &dest)
	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	rel := hostRelease(t)
	sawRocm := false
	for _, u := range *urls {
		if rel.ROCm != "" && filepath.Base(u) == rel.ROCm {
			sawRocm = true
		}
	}
	switch {
	case rel.ROCm == "" && len(*urls) != 1:
		// macOS: Metal and MLX are inside the binary, so an AMD answer from
		// hardware detection must not send us looking for an overlay that
		// upstream does not publish.
		t.Errorf("no ROCm asset exists for %s, but the installer fetched %v", runtime.GOOS, *urls)
	case rel.ROCm != "" && !sawRocm:
		t.Errorf("AMD host should fetch %s; urls=%v", rel.ROCm, *urls)
	}
}

func TestOllamaInstaller_Install_TooSmall(t *testing.T) {
	inst := NewOllamaInstaller(t.TempDir())
	stubRelease(t, inst, []byte("tiny"))
	extracted := false
	inst.extractFn = func(string, string, bool) error { extracted = true; return nil }
	err := inst.Install(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error for a suspiciously small archive")
	}
	if !strings.Contains(err.Error(), "suspiciously small") {
		t.Errorf("error = %v, want it to name the size floor", err)
	}
	if extracted {
		t.Error("must not extract a too-small download")
	}
}

func TestOllamaInstaller_Install_DownloadError(t *testing.T) {
	inst := NewOllamaInstaller(t.TempDir())
	stubChecksums(t, inst, strings.Repeat("a", 64))
	inst.downloadFn = func(context.Context, string, string, func(int64, int64, int64)) (int64, error) {
		return 0, errors.New("offline")
	}
	inst.extractFn = func(string, string, bool) error { return nil }
	if err := inst.Install(context.Background(), nil); err == nil {
		t.Fatal("expected the download error to propagate")
	}
}

// A body that does not hash to what the release published must never be
// extracted — that is the whole point of the check #492 added.
func TestOllamaInstaller_Install_ChecksumMismatch(t *testing.T) {
	lowerFloor(t)

	inst := NewOllamaInstaller(t.TempDir())
	stubRelease(t, inst, []byte("BIGENOUGH"))
	// Same asset names, a checksum for different bytes.
	other := sha256.Sum256([]byte("something else entirely"))
	stubChecksums(t, inst, hex.EncodeToString(other[:]))
	extracted := false
	inst.extractFn = func(string, string, bool) error { extracted = true; return nil }

	err := inst.Install(context.Background(), nil)
	if err == nil {
		t.Fatal("expected a checksum mismatch to fail the install")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error = %v, want it to name the checksum", err)
	}
	if extracted {
		t.Error("must not extract an archive that failed its checksum")
	}
}

// A release whose checksum list does not mention our asset is not a
// license to install it unverified.
func TestOllamaInstaller_Install_ChecksumEntryMissing(t *testing.T) {
	lowerFloor(t)

	inst := NewOllamaInstaller(t.TempDir())
	stubRelease(t, inst, []byte("BIGENOUGH"))
	inst.checksumsFn = func(context.Context) (map[string]string, error) {
		return map[string]string{"some-other-asset.zip": strings.Repeat("b", 64)}, nil
	}
	extracted := false
	inst.extractFn = func(string, string, bool) error { extracted = true; return nil }

	if err := inst.Install(context.Background(), nil); err == nil {
		t.Fatal("expected a missing checksum entry to fail the install")
	}
	if extracted {
		t.Error("must not extract an asset with no published checksum")
	}
}

// An unreachable checksum list is fatal and stops the install BEFORE the
// ~1.4 GB transfer, rather than silently downgrading to HTTPS + a size
// floor.
func TestOllamaInstaller_Install_ChecksumFetchFatal(t *testing.T) {
	inst := NewOllamaInstaller(t.TempDir())
	inst.checksumsFn = func(context.Context) (map[string]string, error) {
		return nil, errors.New("404")
	}
	downloaded := false
	inst.downloadFn = func(context.Context, string, string, func(int64, int64, int64)) (int64, error) {
		downloaded = true
		return 0, nil
	}
	inst.extractFn = func(string, string, bool) error { return nil }

	if err := inst.Install(context.Background(), nil); err == nil {
		t.Fatal("expected an unreachable checksum list to fail the install")
	}
	if downloaded {
		t.Error("must not start the archive download without checksums")
	}
}

// A staging directory left by a killed install is swept, not inherited.
func TestOllamaInstaller_Install_SweepsStaleStage(t *testing.T) {
	lowerFloor(t)

	base := t.TempDir()
	stale := filepath.Join(base, ollamaStageDirName)
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(stale, "ollama-half-a-release.zip")
	if err := os.WriteFile(junk, []byte("abandoned"), 0o600); err != nil {
		t.Fatal(err)
	}

	inst := NewOllamaInstaller(base)
	stubRelease(t, inst, []byte("BIGENOUGH"))
	var dest string
	inst.extractFn = fakeExtract(t, inst, &dest)
	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Errorf("stale staged archive survived (err=%v)", err)
	}
}

// Install must forward downloadFn's byte updates to its progress callback
// stamped with the owning download stage, alongside the existing
// URL/stage announces.
func TestOllamaInstaller_Install_ForwardsByteProgress(t *testing.T) {
	lowerFloor(t)

	inst := NewOllamaInstaller(t.TempDir())
	sum := sha256.Sum256([]byte("BIGENOUGH"))
	stubChecksums(t, inst, hex.EncodeToString(sum[:]))
	inst.downloadFn = func(_ context.Context, _, destPath string, onProgress func(int64, int64, int64)) (int64, error) {
		onProgress(5, 9, -1)
		onProgress(9, 9, 42)
		if err := os.WriteFile(destPath, []byte("BIGENOUGH"), 0o600); err != nil {
			return 0, err
		}
		return 9, nil
	}
	var dest string
	inst.extractFn = fakeExtract(t, inst, &dest)

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

// fetchToFile must stream byte progress with the response Content-Length
// as total, ending on completed == total, and leave the bytes on disk.
// (The reader itself is covered in internal/download/progress_test.go;
// this pins the installer's wiring.)
func TestOllamaInstaller_FetchToFileStreamsProgress(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 256<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	inst := NewOllamaInstaller(t.TempDir())
	dest := filepath.Join(t.TempDir(), "archive")
	var gotBytes, totals []int64
	n, err := inst.fetchToFile(context.Background(), srv.URL, dest, func(c, tot, _ int64) {
		gotBytes, totals = append(gotBytes, c), append(totals, tot)
	})
	if err != nil || n != int64(len(body)) {
		t.Fatalf("fetchToFile: err=%v got %d bytes, want %d", err, n, len(body))
	}
	onDisk, err := os.ReadFile(dest)
	if err != nil || len(onDisk) != len(body) {
		t.Fatalf("staged file: err=%v got %d bytes, want %d", err, len(onDisk), len(body))
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
func TestOllamaInstaller_FetchToFileUnknownLength(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	inst := NewOllamaInstaller(t.TempDir())
	var events int
	var lastCompleted, lastTotal int64
	n, err := inst.fetchToFile(context.Background(), srv.URL, filepath.Join(t.TempDir(), "archive"),
		func(c, tot, _ int64) {
			events++
			lastCompleted, lastTotal = c, tot
		})
	if err != nil || n != int64(len(body)) {
		t.Fatalf("fetchToFile: err=%v got %d bytes, want %d", err, n, len(body))
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

// A failed transfer must not leave a truncated archive behind for the next
// run to find and trust.
func TestOllamaInstaller_FetchToFileRemovesPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	inst := NewOllamaInstaller(t.TempDir())
	dest := filepath.Join(t.TempDir(), "archive")
	if _, err := inst.fetchToFile(context.Background(), srv.URL, dest, nil); err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("partial download survived (err=%v)", err)
	}
}

// fetchChecksums parses what upstream actually publishes.
func TestOllamaInstaller_FetchChecksums(t *testing.T) {
	// Verbatim shape of ollama's sha256sum.txt: two spaces, "./" prefix.
	const list = "230d9815c37ceb7091a4dfc446d3fc47dc40f8a29a536ef42c89a240b3ff8f33  ./ollama-linux-arm64-jetpack6.tar.zst\n" +
		"0C4F92389FCC1F651C17282E2EAFFD68C8D3D06E1F7B307604102AD0E09A10C9  ./ollama-darwin.tgz\n" +
		"\n" +
		"not a checksum line\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(list))
	}))
	defer srv.Close()

	origBase := OllamaDownloadURLBase
	OllamaDownloadURLBase = srv.URL
	t.Cleanup(func() { OllamaDownloadURLBase = origBase })

	inst := NewOllamaInstaller(t.TempDir())
	sums, err := inst.fetchChecksums(context.Background())
	if err != nil {
		t.Fatalf("fetchChecksums: %v", err)
	}
	if len(sums) != 2 {
		t.Errorf("parsed %d checksums, want 2: %v", len(sums), sums)
	}
	// Lower-cased on the way in so the comparison never depends on how
	// upstream happened to render the hex.
	if got := sums["ollama-darwin.tgz"]; got != "0c4f92389fcc1f651c17282e2eaffd68c8d3d06e1f7b307604102ad0e09a10c9" {
		t.Errorf("ollama-darwin.tgz = %q", got)
	}
}

// An empty or unparseable list is an error, not an empty map that would
// read as "no asset has a checksum".
func TestOllamaInstaller_FetchChecksumsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>404</html>\n"))
	}))
	defer srv.Close()

	origBase := OllamaDownloadURLBase
	OllamaDownloadURLBase = srv.URL
	t.Cleanup(func() { OllamaDownloadURLBase = origBase })

	inst := NewOllamaInstaller(t.TempDir())
	if _, err := inst.fetchChecksums(context.Background()); err == nil {
		t.Fatal("expected an error for a list with no checksums in it")
	}
}
