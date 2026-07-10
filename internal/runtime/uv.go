//go:build linux

// Package runtime additions: uv binary auto-resolution. Linux-only —
// uv is only consumed by the vLLM installer (vllm_install.go) which
// itself is Linux-only.
//
// uv (https://github.com/astral-sh/uv) is the lightweight Python
// package + interpreter manager Step 2 uses to bootstrap the vLLM
// venv without touching the host's Python install. The agent must
// have an executable uv on disk before it can build the venv.
//
// Resolution order:
//   1. If override is non-empty, use it verbatim (= caller-supplied
//      bundled binary).
//   2. exec.LookPath("uv") — honour an existing system install.
//   3. ~/.local/share/waired/bin/uv if it exists and is executable.
//   4. Download the pinned release tarball from astral.sh, verify
//      against UVPinnedSHA256, extract uv into ~/.local/share/waired/bin,
//      chmod +x, and use that path.
//
// The pinned version + SHA256 live as compile-time constants (per the
// plan's "uv version pinned in code, bump together" decision) so that
// reproducible builds always materialise the same uv.

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UVPinnedVersion is the uv release we ship/recommend. Bump together
// with UVPinnedSHA256; both must change in lockstep so the integrity
// check stays meaningful. Refresh this whenever astral.sh ships a
// stable that fixes a security issue or a correctness bug we depend on.
// NOTE: UVPinnedSHA256Linux64 below must be recomputed in lockstep when
// this is bumped — Renovate flags this on the uv PR (see renovate.json).
// renovate: datasource=github-releases depName=astral-sh/uv
const UVPinnedVersion = "0.11.26"

// UVPinnedSHA256Linux64 is the sha256 of the linux x86_64 tarball at
// https://github.com/astral-sh/uv/releases/download/<UVPinnedVersion>/uv-x86_64-unknown-linux-gnu.tar.gz
//
// Bump in lockstep with UVPinnedVersion: download the release asset,
// verify against the official `.sha256` sidecar, and paste the digest
// here. A leftover all-zero placeholder makes ResolveUV() (no override,
// no system uv) fail closed with ErrUVUnverifiedPin rather than download
// something unverified — which is exactly what blocked
// `waired runtimes install vllm` end-to-end (#557). Verified against
// https://github.com/astral-sh/uv/releases/download/0.11.26/uv-x86_64-unknown-linux-gnu.tar.gz.sha256
const UVPinnedSHA256Linux64 = "6426a73c3837e6e2483ee344cbc00f36394d179afcba6183cb77437e67db4af0"

// UVDownloadURLBase is the GitHub release download prefix the
// auto-download path uses. Centralised so tests can swap it.
var UVDownloadURLBase = "https://github.com/astral-sh/uv/releases/download"

// uvPinnedSHA256OverrideForTest, when non-empty, takes precedence
// over UVPinnedSHA256Linux64. Tests use it to exercise the download
// path without mutating the const itself.
var uvPinnedSHA256OverrideForTest = ""

// effectivePinnedSHA returns the SHA the download path should
// compare against. The test override beats the compile-time const.
func effectivePinnedSHA() string {
	if uvPinnedSHA256OverrideForTest != "" {
		return uvPinnedSHA256OverrideForTest
	}
	return UVPinnedSHA256Linux64
}

// ErrUVUnverifiedPin is returned when the auto-download path triggers
// but UVPinnedSHA256Linux64 is still the placeholder. Refuses to
// download anything until the pin has been verified by an operator.
var ErrUVUnverifiedPin = errors.New("runtime: uv SHA256 pin not yet verified (operator must update UVPinnedSHA256Linux64)")

// ErrUVChecksumMismatch is returned when the downloaded tarball's
// sha256 doesn't match the pin.
var ErrUVChecksumMismatch = errors.New("runtime: uv tarball sha256 mismatch (refusing to install)")

// ErrUVUnsupportedPlatform is returned when running on something
// other than linux/amd64. Step 2 only ships pins for that target;
// other platforms can fall back to a system uv via override.
var ErrUVUnsupportedPlatform = errors.New("runtime: pinned uv tarball only available for linux/amd64")

// UVResolver discovers (and, if needed, materialises) a uv binary.
type UVResolver struct {
	// BinDir is the directory used to store the auto-downloaded uv.
	// Defaults to $XDG_DATA_HOME/waired/bin (or $HOME/.local/share/waired/bin).
	BinDir string

	// HTTPClient is the seam tests use to inject a fake download.
	// Defaults to a 60s-timeout client.
	HTTPClient *http.Client

	// Now / clock seam for tests asserting on installed_at metadata.
	Now func() time.Time
}

// NewUVResolver returns a resolver with sensible defaults.
func NewUVResolver() *UVResolver {
	return &UVResolver{
		BinDir:     defaultUVBinDir(),
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		Now:        time.Now,
	}
}

// Resolve returns an absolute path to an executable uv binary. See
// the package doc for the resolution order. override is the highest-
// precedence layer (caller-supplied bundled path).
func (r *UVResolver) Resolve(ctx context.Context, override string) (string, error) {
	if override != "" {
		if err := assertExecutable(override); err != nil {
			return "", fmt.Errorf("runtime: uv override %q: %w", override, err)
		}
		return override, nil
	}
	if p, err := exec.LookPath("uv"); err == nil {
		return p, nil
	}
	cached := filepath.Join(r.BinDir, "uv")
	if err := assertExecutable(cached); err == nil {
		return cached, nil
	}
	// Materialise the pin.
	return r.downloadPinnedUV(ctx)
}

// downloadPinnedUV fetches UVPinnedVersion from astral.sh, verifies
// the sha256 against the pin, extracts the binary into r.BinDir, and
// returns the absolute path. Refuses to proceed if the pin is the
// placeholder or the platform is unsupported.
func (r *UVResolver) downloadPinnedUV(ctx context.Context) (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("%w: GOOS/GOARCH=%s/%s", ErrUVUnsupportedPlatform, runtime.GOOS, runtime.GOARCH)
	}
	pin := effectivePinnedSHA()
	if isPlaceholderSHA(pin) {
		return "", ErrUVUnverifiedPin
	}

	url := fmt.Sprintf("%s/%s/uv-x86_64-unknown-linux-gnu.tar.gz", UVDownloadURLBase, UVPinnedVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("runtime: uv download request: %w", err)
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("runtime: uv download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("runtime: uv download HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("runtime: uv read body: %w", err)
	}

	gotSum := sha256.Sum256(body)
	gotHex := hex.EncodeToString(gotSum[:])
	if !strings.EqualFold(gotHex, pin) {
		return "", fmt.Errorf("%w: want %s, got %s", ErrUVChecksumMismatch, pin, gotHex)
	}

	if err := os.MkdirAll(r.BinDir, 0o755); err != nil {
		return "", fmt.Errorf("runtime: mkdir uv bin: %w", err)
	}
	target := filepath.Join(r.BinDir, "uv")
	if err := extractUVTarball(body, target); err != nil {
		return "", fmt.Errorf("runtime: extract uv: %w", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", fmt.Errorf("runtime: chmod uv: %w", err)
	}
	return target, nil
}

// extractUVTarball unpacks the uv binary out of a .tar.gz body into
// dest. The astral release layout is `uv-x86_64-unknown-linux-gnu/uv`
// inside a gzipped tar.
func extractUVTarball(body []byte, dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "uv-extract-*.tar.gz")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Use the system tar to avoid pulling in archive/tar parsing here;
	// the file is small (~30 MB tarball, ~50 MB extracted) and tar is
	// guaranteed available on every Linux host the agent supports.
	cmd := exec.Command("tar",
		"-xzf", tmpName,
		"-C", filepath.Dir(dest),
		"--strip-components=1",
		"uv-x86_64-unknown-linux-gnu/uv",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultUVBinDir returns $XDG_DATA_HOME/waired/bin (or $HOME/.local/share/waired/bin).
func defaultUVBinDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "waired", "bin")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "waired", "bin")
}

// assertExecutable reports nil iff path exists, is a regular file,
// and has at least one execute bit set.
func assertExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	if st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable: %s", path)
	}
	return nil
}

// isPlaceholderSHA returns true iff s is the all-zero placeholder
// sentinel. Used to refuse downloads before the pin has been
// verified by an operator.
func isPlaceholderSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
