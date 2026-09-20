//go:build linux

// Package runtime additions: the uv binary the vLLM installer runs.
// Linux-only — uv is only consumed by the vLLM installer
// (vllm_install.go), which itself is Linux-only; Windows and macOS have
// no vLLM and no uv (vllm_stub_windows.go / vllm_stub_darwin.go).
//
// uv (https://github.com/astral-sh/uv) is the Python package and
// interpreter manager the installer uses to build the vLLM venv without
// touching the host's Python.
//
// The installer uses exactly one uv: the pinned release, kept under the
// state dir at <state-dir>/runtimes/uv/<UVPinnedVersion>/uv. There is no
// other place it looks — not a uv on PATH, not a copy under the invoking
// user's home (waired-ai/waired#1435). Resolution:
//
//  1. <Root>/<UVPinnedVersion>/uv, if it is an executable regular file.
//  2. Otherwise download the pinned release tarball for this GOARCH,
//     verify its SHA256 against the compile-time pin, extract the uv
//     member into <Root>/.tmp-*, and rename that directory into place.
//  3. Remove every other version directory (and stale .tmp-* left by an
//     interrupted download) under Root. The cache directory stays.
//
// Why only this one:
//
//   - A uv pin move reaches every host. The previous chain reused a uv
//     on PATH or ~/.local/share/waired/bin/uv without looking at its
//     version, so a new pin only reached hosts that had no uv yet, and a
//     user's older or newer uv ran untested.
//   - A root-run install (`sudo waired runtimes install vllm`, install.sh)
//     and the daemon's converge as the service user resolve the SAME
//     binary. HOME differs between them (/root vs /var/lib/waired), so a
//     home-relative copy split into two; under the state dir it is one,
//     and the ownership hand-off that covers the venv
//     (service.FixStateOwnership) covers it too.
//   - Uninstall removes it. `uninstall.sh --clean` and the deb purge both
//     delete the state dir; nothing removed /root/.local/share/waired/bin.
//
// The pinned version + SHA256s live as compile-time constants so that
// reproducible builds always materialise the same uv.

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UVPinnedVersion is the uv release the vLLM installer uses. Bump
// together with UVPinnedSHA256Linux64 and UVPinnedSHA256LinuxARM64;
// all three must change in lockstep so the integrity check stays
// meaningful. scripts/dev/update-uv-sha.sh recomputes both digests, and
// Renovate runs it on the uv PR (see renovate.json).
// renovate: datasource=github-releases depName=astral-sh/uv
const UVPinnedVersion = "0.12.17"

// UVPinnedSHA256Linux64 is the sha256 of the linux x86_64 tarball at
// https://github.com/astral-sh/uv/releases/download/<UVPinnedVersion>/uv-x86_64-unknown-linux-gnu.tar.gz
//
// Bump in lockstep with UVPinnedVersion: download the release asset,
// verify against the official `.sha256` sidecar, and paste the digest
// here. A leftover all-zero placeholder makes Resolve fail closed with
// ErrUVUnverifiedPin rather than download something unverified — which
// is exactly what blocked `waired runtimes install vllm` end-to-end
// (#557). Verified against
// https://github.com/astral-sh/uv/releases/download/0.12.17/uv-x86_64-unknown-linux-gnu.tar.gz.sha256
const UVPinnedSHA256Linux64 = "fa82fd8dde8e8eefdecada6aa0889666556cfceb690d06e0c3bca49eb3070a63"

// UVPinnedSHA256LinuxARM64 is the sha256 of the linux aarch64 tarball at
// https://github.com/astral-sh/uv/releases/download/<UVPinnedVersion>/uv-aarch64-unknown-linux-gnu.tar.gz
//
// Same lockstep rule as UVPinnedSHA256Linux64. An arm64 host used to get
// a uv only if one was on PATH; with PATH no longer consulted
// (waired-ai/waired#1435) this pin is its only source. Verified against
// https://github.com/astral-sh/uv/releases/download/0.12.17/uv-aarch64-unknown-linux-gnu.tar.gz.sha256
const UVPinnedSHA256LinuxARM64 = "d636d1b678e9e7f367ecb22b46bd1cabbed234d6bc3b4d96365d2b507f72f86c"

// UVDownloadURLBase is the GitHub release download prefix the
// download path uses. Centralised so tests can swap it.
var UVDownloadURLBase = "https://github.com/astral-sh/uv/releases/download"

// uvPinnedSHA256OverrideForTest, when it has an entry for a GOARCH,
// takes precedence over that arch's compile-time pin. Tests use it to
// exercise the download path without mutating the consts.
var uvPinnedSHA256OverrideForTest = map[string]string{}

// uvCacheDirName is the directory under Root that holds uv's download
// cache (UV_CACHE_DIR). Pruning never removes it.
const uvCacheDirName = "cache"

// uvTmpPrefix names the per-download staging directories under Root.
const uvTmpPrefix = ".tmp-"

// uvStaleTmpAge is how old a staging directory must be before pruning
// treats it as abandoned. A younger one may belong to a resolver that is
// still downloading in another process.
const uvStaleTmpAge = 30 * time.Minute

// uvMaxBinaryBytes bounds the extracted member. The real binary is
// ~50 MB; anything far past that is not a uv release.
const uvMaxBinaryBytes = 512 << 20

// uvAsset describes one pinned release asset.
type uvAsset struct {
	// triple is both the tarball's name stem and its top-level
	// directory: <triple>.tar.gz contains <triple>/uv.
	triple string
	sha256 string
}

// uvAssetFor returns the pinned asset for goarch, or ok=false when uv
// has no pin for that architecture.
func uvAssetFor(goarch string) (uvAsset, bool) {
	var a uvAsset
	switch goarch {
	case "amd64":
		a = uvAsset{triple: "uv-x86_64-unknown-linux-gnu", sha256: UVPinnedSHA256Linux64}
	case "arm64":
		a = uvAsset{triple: "uv-aarch64-unknown-linux-gnu", sha256: UVPinnedSHA256LinuxARM64}
	default:
		return uvAsset{}, false
	}
	if s, ok := uvPinnedSHA256OverrideForTest[goarch]; ok && s != "" {
		a.sha256 = s
	}
	return a, true
}

// ErrUVUnverifiedPin is returned when the download path triggers but the
// arch's SHA256 pin is still the all-zero placeholder. Refuses to
// download anything until the pin has been verified by an operator.
var ErrUVUnverifiedPin = errors.New("runtime: uv SHA256 pin not yet verified (operator must update the UVPinnedSHA256 constants)")

// ErrUVChecksumMismatch is returned when the downloaded tarball's
// sha256 doesn't match the pin.
var ErrUVChecksumMismatch = errors.New("runtime: uv tarball sha256 mismatch (refusing to install)")

// ErrUVUnsupportedPlatform is returned when uv has no pinned asset for
// the running architecture (only linux/amd64 and linux/arm64 do).
var ErrUVUnsupportedPlatform = errors.New("runtime: pinned uv tarball only available for linux/amd64 and linux/arm64")

// UVResolver materialises the pinned uv under Root.
type UVResolver struct {
	// Root is <state-dir>/runtimes/uv. Version directories and the
	// cache live directly under it.
	Root string

	// HTTPClient performs the download. Defaults to a 60s-timeout client.
	HTTPClient *http.Client

	// GOARCH selects the asset. Empty means runtime.GOARCH; tests set it
	// to exercise the other architecture's asset.
	GOARCH string
}

// NewUVResolverAt returns a resolver rooted at root (normally
// <state-dir>/runtimes/uv).
func NewUVResolverAt(root string) *UVResolver {
	return &UVResolver{
		Root:       root,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// CacheDir is the directory the installer points UV_CACHE_DIR at. It
// sits beside the version directories so uv can hardlink from it into
// venvs on the same filesystem, and uninstall removes it with the state
// dir.
func (r *UVResolver) CacheDir() string {
	return filepath.Join(r.Root, uvCacheDirName)
}

// Path is where the pinned uv lives once resolved.
func (r *UVResolver) Path() string {
	return filepath.Join(r.Root, UVPinnedVersion, "uv")
}

// Resolve returns the absolute path to the pinned uv, downloading and
// verifying it first when it is not already in place. See the package
// doc for why nothing else is consulted.
func (r *UVResolver) Resolve(ctx context.Context) (string, error) {
	if r.Root == "" {
		return "", errors.New("runtime: uv resolver has no root directory")
	}
	final := r.Path()
	if err := assertExecutable(final); err != nil {
		if err := r.download(ctx); err != nil {
			return "", err
		}
	}
	r.pruneOthers()
	return final, nil
}

func (r *UVResolver) goarch() string {
	if r.GOARCH != "" {
		return r.GOARCH
	}
	return runtime.GOARCH
}

// download fetches the pinned tarball for this arch, verifies it,
// extracts the uv member into a staging directory under Root and renames
// the staging directory to <Root>/<UVPinnedVersion>. A concurrent
// resolver that got there first wins; this call then uses its result.
func (r *UVResolver) download(ctx context.Context) error {
	goarch := r.goarch()
	asset, ok := uvAssetFor(goarch)
	if !ok {
		return fmt.Errorf("%w: GOOS/GOARCH=%s/%s", ErrUVUnsupportedPlatform, runtime.GOOS, goarch)
	}
	if isPlaceholderSHA(asset.sha256) {
		return ErrUVUnverifiedPin
	}

	body, err := r.fetch(ctx, fmt.Sprintf("%s/%s/%s.tar.gz", UVDownloadURLBase, UVPinnedVersion, asset.triple))
	if err != nil {
		return err
	}
	gotSum := sha256.Sum256(body)
	gotHex := hex.EncodeToString(gotSum[:])
	if !strings.EqualFold(gotHex, asset.sha256) {
		return fmt.Errorf("%w: want %s, got %s", ErrUVChecksumMismatch, asset.sha256, gotHex)
	}

	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		return fmt.Errorf("runtime: mkdir uv root: %w", err)
	}
	stage, err := os.MkdirTemp(r.Root, uvTmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("runtime: uv staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	// MkdirTemp creates 0700; the version directory has to be readable by
	// the service user after the ownership hand-off, and by other users
	// who only run the venv it built.
	if err := os.Chmod(stage, 0o755); err != nil {
		return fmt.Errorf("runtime: chmod uv staging dir: %w", err)
	}
	if err := extractUVMember(body, asset.triple+"/uv", filepath.Join(stage, "uv")); err != nil {
		if assertExecutable(r.Path()) == nil {
			return nil
		}
		return fmt.Errorf("runtime: extract uv: %w", err)
	}

	final := filepath.Dir(r.Path())
	if err := os.Rename(stage, final); err != nil {
		// Another resolver may have put the same version in place while
		// this one downloaded. Its result is equivalent — same pin, same
		// verified bytes — so use it.
		if assertExecutable(r.Path()) == nil {
			return nil
		}
		return fmt.Errorf("runtime: move uv into place: %w", err)
	}
	return nil
}

func (r *UVResolver) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("runtime: uv download request: %w", err)
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runtime: uv download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runtime: uv download HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("runtime: uv read body: %w", err)
	}
	return body, nil
}

// extractUVMember writes the tarball member named member (the astral
// release layout is <triple>/uv inside a gzipped tar) to dest with mode
// 0755. In-process, so the download path does not depend on a tar binary
// on the host.
func extractUVMember(body []byte, member, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("member %s not found in tarball", member)
		}
		if err != nil {
			return err
		}
		if strings.TrimPrefix(hdr.Name, "./") != member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("member %s is not a regular file", member)
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(tr, uvMaxBinaryBytes+1))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n > uvMaxBinaryBytes {
			return fmt.Errorf("member %s exceeds %d bytes", member, uvMaxBinaryBytes)
		}
		// OpenFile's mode is masked by the umask; the binary must be
		// executable regardless.
		return os.Chmod(dest, 0o755)
	}
}

// pruneOthers removes version directories other than the pinned one and
// staging directories an interrupted download left behind (older than
// uvStaleTmpAge, so a download still running elsewhere is not pulled out
// from under it). Best effort: a failure leaves disk behind but never
// fails the install. Entries that look like neither, and the cache, are
// left alone.
func (r *UVResolver) pruneOthers() {
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == UVPinnedVersion || name == uvCacheDirName {
			continue
		}
		switch {
		case strings.HasPrefix(name, uvTmpPrefix):
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < uvStaleTmpAge {
				continue
			}
		case looksLikeUVVersion(name):
		default:
			continue
		}
		_ = os.RemoveAll(filepath.Join(r.Root, name))
	}
}

// looksLikeUVVersion reports whether name is shaped like a uv release
// directory ("0.11.26"): digits and dots, starting with a digit.
func looksLikeUVVersion(name string) bool {
	if name == "" || name[0] < '0' || name[0] > '9' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
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
