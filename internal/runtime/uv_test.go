//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The resolver is the one place the vLLM installer gets uv from, and
// waired-ai/waired#1435 made it exactly one place: <Root>/<pin>/uv,
// downloaded and verified when absent. These tests serve a real .tar.gz
// built here, in the astral release layout, so the download, checksum
// and extraction code runs for real against a local server.

const uvTestBinary = "#!/bin/sh\necho uv-from-tarball\n"

// uvReleaseTarball builds a gzipped tar holding <triple>/uv for every
// triple given, each with uvTestBinary as its body.
func uvReleaseTarball(t *testing.T, triples ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, triple := range triples {
		if err := tw.WriteHeader(&tar.Header{Name: triple + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"uv", "uvx"} {
			hdr := &tar.Header{Name: triple + "/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(uvTestBinary))}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(uvTestBinary)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uvRelease serves body for every request, records the paths it was
// asked for, and points the resolver's download base and both arch pins
// at it for the duration of the test.
type uvRelease struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	hits  atomic.Int32
}

func serveUVRelease(t *testing.T, body []byte, status int) *uvRelease {
	t.Helper()
	rel := &uvRelease{}
	rel.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rel.hits.Add(1)
		rel.mu.Lock()
		rel.paths = append(rel.paths, req.URL.Path)
		rel.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(rel.srv.Close)

	sum := sha256.Sum256(body)
	pinTo(t, hex.EncodeToString(sum[:]))
	base := UVDownloadURLBase
	UVDownloadURLBase = rel.srv.URL
	t.Cleanup(func() { UVDownloadURLBase = base })
	return rel
}

// pinTo points both arch pins at sha for the duration of the test.
func pinTo(t *testing.T, sha string) {
	t.Helper()
	prev := uvPinnedSHA256OverrideForTest
	uvPinnedSHA256OverrideForTest = map[string]string{"amd64": sha, "arm64": sha}
	t.Cleanup(func() { uvPinnedSHA256OverrideForTest = prev })
}

func (rel *uvRelease) resolver(root, goarch string) *UVResolver {
	return &UVResolver{Root: root, HTTPClient: rel.srv.Client(), GOARCH: goarch}
}

// uvStubPath creates <root>/<pin>/ and returns the path a pinned uv
// lives at, so installer tests can drop a stub there instead of letting
// the resolver download.
func uvStubPath(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, UVPinnedVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "uv")
}

func TestUVResolve_DownloadsThePinnedAssetForEachArch(t *testing.T) {
	for _, tc := range []struct {
		goarch string
		triple string
	}{
		{"amd64", "uv-x86_64-unknown-linux-gnu"},
		{"arm64", "uv-aarch64-unknown-linux-gnu"},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			// The tarball carries only this arch's triple, so a resolver
			// reading the other triple's member fails here (see also
			// TestUVResolve_WrongArchTarballInstallsNothing).
			rel := serveUVRelease(t, uvReleaseTarball(t, tc.triple), http.StatusOK)
			root := t.TempDir()

			got, err := rel.resolver(root, tc.goarch).Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if want := filepath.Join(root, UVPinnedVersion, "uv"); got != want {
				t.Errorf("Resolve = %q, want %q", got, want)
			}
			if err := assertExecutable(got); err != nil {
				t.Errorf("resolved uv is not executable: %v", err)
			}
			b, err := os.ReadFile(got)
			if err != nil || string(b) != uvTestBinary {
				t.Errorf("resolved uv content = %q (%v), want the tarball's uv member", b, err)
			}
			wantPath := "/" + UVPinnedVersion + "/" + tc.triple + ".tar.gz"
			if len(rel.paths) != 1 || rel.paths[0] != wantPath {
				t.Errorf("requested %v, want exactly [%s]", rel.paths, wantPath)
			}
			if _, err := os.Stat(filepath.Join(root, UVPinnedVersion, "uvx")); !os.IsNotExist(err) {
				t.Errorf("only the uv member is extracted; uvx stat err = %v", err)
			}
		})
	}
}

// A tarball for the other architecture has no member for this one, so
// the resolver must fail rather than install something.
func TestUVResolve_WrongArchTarballInstallsNothing(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-aarch64-unknown-linux-gnu"), http.StatusOK)
	root := t.TempDir()
	if _, err := rel.resolver(root, "amd64").Resolve(context.Background()); err == nil {
		t.Fatal("Resolve succeeded with no member for amd64 in the tarball")
	}
	if _, err := os.Stat(filepath.Join(root, UVPinnedVersion)); !os.IsNotExist(err) {
		t.Errorf("a version directory was left behind: %v", err)
	}
}

// PRODUCT RULE — waired-ai/waired#1435 (owner-approved plan): a uv on
// PATH is never used; the managed pin is.
func TestUVResolve_IgnoresAUVOnPATH(t *testing.T) {
	pathDir := t.TempDir()
	mustWriteExec(t, filepath.Join(pathDir, "uv"), "#!/bin/sh\necho from-path\n")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	root := t.TempDir()
	got, err := rel.resolver(root, "amd64").Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.HasPrefix(got, pathDir) {
		t.Errorf("Resolve returned the uv on PATH: %s", got)
	}
	if n := rel.hits.Load(); n != 1 {
		t.Errorf("downloads = %d, want exactly 1 (PATH must not short-circuit the pin)", n)
	}
}

// The resolver does not look in the invoking user's home either.
func TestUVResolve_IgnoresTheOldHomeCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	old := filepath.Join(home, ".local", "share", "waired", "bin")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExec(t, filepath.Join(old, "uv"), "#!/bin/sh\necho old\n")

	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	root := t.TempDir()
	got, err := rel.resolver(root, "amd64").Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.HasPrefix(got, home) {
		t.Errorf("Resolve returned the home copy: %s", got)
	}
	if n := rel.hits.Load(); n != 1 {
		t.Errorf("downloads = %d, want 1", n)
	}
}

func TestUVResolve_PinnedVersionAlreadyThereDoesNotDownload(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	root := t.TempDir()
	stub := uvStubPath(t, root)
	mustWriteExec(t, stub, "#!/bin/sh\necho already\n")

	got, err := rel.resolver(root, "amd64").Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != stub {
		t.Errorf("Resolve = %q, want %q", got, stub)
	}
	if n := rel.hits.Load(); n != 0 {
		t.Errorf("downloads = %d, want 0", n)
	}
}

// A pin move leaves the previous version behind; resolving the new one
// removes it, and removes staging directories an interrupted download
// left, but never the cache.
func TestUVResolve_PrunesOtherVersionsAndStaleStagingButKeepsTheCache(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldVer := mk("0.11.26")
	mustWriteExec(t, filepath.Join(oldVer, "uv"), "#!/bin/sh\n")
	staleTmp := mk(uvTmpPrefix + "stale")
	old := time.Now().Add(-2 * uvStaleTmpAge)
	if err := os.Chtimes(staleTmp, old, old); err != nil {
		t.Fatal(err)
	}
	freshTmp := mk(uvTmpPrefix + "fresh")
	cache := mk(uvCacheDirName + "/wheels-v5")
	unrelated := mk("notes")

	if _, err := rel.resolver(root, "amd64").Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, gone := range []string{oldVer, staleTmp} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed (stat err %v)", gone, err)
		}
	}
	for _, kept := range []string{freshTmp, cache, unrelated} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}
}

func TestUVResolve_ChecksumMismatchLeavesNothing(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	pinTo(t, strings.Repeat("a", 64))
	root := t.TempDir()

	_, err := rel.resolver(root, "amd64").Resolve(context.Background())
	if !errors.Is(err, ErrUVChecksumMismatch) {
		t.Fatalf("err = %v, want ErrUVChecksumMismatch", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("root not empty after a checksum mismatch: %v", entries)
	}
}

func TestUVResolve_HTTPError(t *testing.T) {
	rel := serveUVRelease(t, nil, http.StatusNotFound)
	pinTo(t, strings.Repeat("b", 64))
	_, err := rel.resolver(t.TempDir(), "amd64").Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want HTTP 404 mention", err)
	}
}

func TestUVResolve_PlaceholderPinRefusesToDownload(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	pinTo(t, strings.Repeat("0", 64))
	_, err := rel.resolver(t.TempDir(), "amd64").Resolve(context.Background())
	if !errors.Is(err, ErrUVUnverifiedPin) {
		t.Errorf("err = %v, want ErrUVUnverifiedPin", err)
	}
	if n := rel.hits.Load(); n != 0 {
		t.Errorf("downloads = %d, want 0 with a placeholder pin", n)
	}
}

func TestUVResolve_UnsupportedArch(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	_, err := rel.resolver(t.TempDir(), "riscv64").Resolve(context.Background())
	if !errors.Is(err, ErrUVUnsupportedPlatform) {
		t.Errorf("err = %v, want ErrUVUnsupportedPlatform", err)
	}
	if n := rel.hits.Load(); n != 0 {
		t.Errorf("downloads = %d, want 0", n)
	}
}

// Two installs racing (the CLI and the daemon's converge) end with one
// usable binary and no error from either.
func TestUVResolve_ConcurrentResolversConverge(t *testing.T) {
	rel := serveUVRelease(t, uvReleaseTarball(t, "uv-x86_64-unknown-linux-gnu"), http.StatusOK)
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	paths := make([]string, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = rel.resolver(root, "amd64").Resolve(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("resolver %d: %v", i, err)
		}
		if paths[i] != filepath.Join(root, UVPinnedVersion, "uv") {
			t.Errorf("resolver %d path = %q", i, paths[i])
		}
	}
	if err := assertExecutable(filepath.Join(root, UVPinnedVersion, "uv")); err != nil {
		t.Errorf("no usable uv after the race: %v", err)
	}
}

func TestUVResolver_CacheDirIsBesideTheVersions(t *testing.T) {
	r := NewUVResolverAt("/state/runtimes/uv")
	if got := r.CacheDir(); got != "/state/runtimes/uv/cache" {
		t.Errorf("CacheDir = %q", got)
	}
	if got := r.Path(); got != "/state/runtimes/uv/"+UVPinnedVersion+"/uv" {
		t.Errorf("Path = %q", got)
	}
}

func TestIsPlaceholderSHA(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("0", 64): true,
		strings.Repeat("a", 64): false,
		"deadbeef":              false, // wrong length
		"":                      false,
		strings.Repeat("0", 63): false,
	}
	for in, want := range cases {
		if got := isPlaceholderSHA(in); got != want {
			t.Errorf("isPlaceholderSHA(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLooksLikeUVVersion(t *testing.T) {
	for in, want := range map[string]bool{
		"0.11.26": true, "1.0": true, "cache": false, ".tmp-x": false, "": false, "v0.1": false, "0.1-rc1": false,
	} {
		if got := looksLikeUVVersion(in); got != want {
			t.Errorf("looksLikeUVVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func mustWriteExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
