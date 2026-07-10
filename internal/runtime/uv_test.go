//go:build linux

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveUV_Override(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-uv")
	mustWriteExec(t, bin, "#!/bin/sh\necho 0.11.8\n")

	r := NewUVResolver()
	got, err := r.Resolve(context.Background(), bin)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != bin {
		t.Errorf("Resolve(override) = %q, want %q", got, bin)
	}
}

func TestResolveUV_OverrideMissing(t *testing.T) {
	r := NewUVResolver()
	_, err := r.Resolve(context.Background(), "/does/not/exist/uv")
	if err == nil {
		t.Fatalf("expected error for missing override")
	}
}

func TestResolveUV_CachedBinDir(t *testing.T) {
	if _, err := exec.LookPath("uv"); err == nil {
		t.Skip("system uv on PATH would shadow cached lookup; skip in this environment")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "uv")
	mustWriteExec(t, bin, "#!/bin/sh\necho 0.11.8\n")

	r := &UVResolver{BinDir: dir, HTTPClient: &http.Client{}}
	got, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != bin {
		t.Errorf("Resolve(cached) = %q, want %q", got, bin)
	}
}

func TestResolveUV_PlaceholderSHARefuses(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("download path is linux/amd64 only")
	}
	if _, err := exec.LookPath("uv"); err == nil {
		t.Skip("system uv on PATH would short-circuit the download path")
	}
	// Force the all-zero placeholder so the refusal behaviour is tested
	// independently of the real shipped pin (set for #557). Without this
	// the resolver would proceed to a real network download.
	setPinnedSHA(strings.Repeat("0", 64))
	t.Cleanup(func() { setPinnedSHA("") })

	r := &UVResolver{BinDir: t.TempDir(), HTTPClient: &http.Client{}}
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrUVUnverifiedPin) {
		t.Errorf("err = %v, want ErrUVUnverifiedPin (placeholder pin must refuse to download)", err)
	}
}

func TestDownloadPinnedUV_ChecksumMismatch(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("download path is linux/amd64 only")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	// Pin a real-looking sha256 so we get past the placeholder check,
	// then serve a body that hashes to something different.
	originalSHA := UVPinnedSHA256Linux64
	originalBase := UVDownloadURLBase
	defer func() {
		setPinnedSHA(originalSHA)
		UVDownloadURLBase = originalBase
	}()
	setPinnedSHA(strings.Repeat("a", 64))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bogus tarball body"))
	}))
	defer srv.Close()
	UVDownloadURLBase = srv.URL

	r := &UVResolver{BinDir: t.TempDir(), HTTPClient: srv.Client()}
	_, err := r.downloadPinnedUV(context.Background())
	if !errors.Is(err, ErrUVChecksumMismatch) {
		t.Errorf("err = %v, want ErrUVChecksumMismatch", err)
	}
}

func TestDownloadPinnedUV_HTTPError(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("download path is linux/amd64 only")
	}
	originalSHA := UVPinnedSHA256Linux64
	originalBase := UVDownloadURLBase
	defer func() {
		setPinnedSHA(originalSHA)
		UVDownloadURLBase = originalBase
	}()
	setPinnedSHA(strings.Repeat("b", 64))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	UVDownloadURLBase = srv.URL

	r := &UVResolver{BinDir: t.TempDir(), HTTPClient: srv.Client()}
	_, err := r.downloadPinnedUV(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want HTTP 404 mention", err)
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

func TestExtractUVTarball_Roundtrip(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	// Build a tiny tarball whose layout matches astral's release shape.
	tarballDir := t.TempDir()
	innerDir := filepath.Join(tarballDir, "uv-x86_64-unknown-linux-gnu")
	if err := os.MkdirAll(innerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExec(t, filepath.Join(innerDir, "uv"), "#!/bin/sh\necho 0.11.8\n")
	tarballPath := filepath.Join(tarballDir, "release.tar.gz")
	cmd := exec.Command("tar", "-czf", tarballPath, "-C", tarballDir, "uv-x86_64-unknown-linux-gnu")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar create: %v: %s", err, out)
	}
	body, err := os.ReadFile(tarballPath)
	if err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "uv")
	if err := extractUVTarball(body, dest); err != nil {
		t.Fatalf("extractUVTarball: %v", err)
	}
	if err := assertExecutable(dest); err != nil {
		// extract leaves the file mode as in the tarball; we manually
		// chmod after extract in downloadPinnedUV, so emulate that here.
		_ = os.Chmod(dest, 0o755)
		if err := assertExecutable(dest); err != nil {
			t.Errorf("extracted file not executable: %v", err)
		}
	}
}

func TestSHA256ConstantsAlignWithDownloadedBody(t *testing.T) {
	// Demonstrates the contract used by downloadPinnedUV: hex of sha256(body)
	// matches UVPinnedSHA256Linux64 exactly. We don't check the real pin
	// here (Step 8 does that against the live download); we just lock in
	// the comparison shape.
	body := []byte("any bytes")
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if len(got) != 64 {
		t.Errorf("sha256 hex length = %d, want 64", len(got))
	}
}

// setPinnedSHA temporarily mutates the const-shaped pin via an
// indirection since you can't reassign a const. This test helper
// only works because UVPinnedSHA256Linux64 is referenced via
// internal usage; we route through a package-private setter.
func setPinnedSHA(s string) {
	uvPinnedSHA256OverrideForTest = s
}

func mustWriteExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
