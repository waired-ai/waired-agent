//go:build linux

package runtime

// Bundled Ollama installer (Linux). waired's "out of the box" stance is
// to package Ollama itself: download a pinned official release tarball
// into a waired-owned directory and supervise the binary as a foreground
// child (the existing OllamaAdapter spawn model) — no system service, no
// systemctl. This mirrors the Windows ZIP approach in
// scripts/install/ollama-windows.ps1 and the download/extract pattern in
// uv.go, and replaces the earlier install.sh + `systemctl disable ollama`
// path (which fought the very service it created). See #188.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/waired-ai/waired-agent/internal/download"
)

// OllamaDownloadURLBase is the GitHub release prefix. A var so tests can
// point it at a local httptest server.
var OllamaDownloadURLBase = "https://github.com/ollama/ollama/releases/download"

// ollamaTarballMinBytes is a sanity floor: the real linux tarball is
// hundreds of MB. A response far below this is an error page / partial
// download, not a release, so we refuse to extract it. (Mirrors the
// size-floor guard in ollama-windows.ps1; combined with HTTPS this is
// the v1 integrity posture — a per-version SHA pin is a future
// hardening, but unlike uv we do not want to chase Ollama's frequent
// releases with a hardcoded hash.)
var ollamaTarballMinBytes = 50 << 20 // 50 MiB (var so tests can lower it)

// OllamaInstaller downloads + extracts a pinned Ollama release into
// BaseDir (typically <state-dir>/runtimes/ollama). The binary lands at
// BaseDir/bin/ollama, which OllamaAdapter is pointed at.
type OllamaInstaller struct {
	BaseDir    string
	HTTPClient *http.Client
	Now        func() time.Time

	// GPUVendor, when "amd", makes Install overlay the ROCm runtime on
	// top of the base tarball. Set by the caller from hardware detection;
	// "" (the default) installs the CUDA+CPU base only.
	GPUVendor string

	// Seams (defaulted by NewOllamaInstaller) so tests exercise the
	// orchestration without network or tar. onProgress (nil-ok) receives
	// throttled byte updates while the body streams down.
	downloadFn func(ctx context.Context, url string, onProgress func(completed, total, bytesPerSec int64)) ([]byte, error)
	extractFn  func(archive []byte, destDir string) error
}

// NewOllamaInstaller wires defaults rooted at baseDir.
func NewOllamaInstaller(baseDir string) *OllamaInstaller {
	i := &OllamaInstaller{
		BaseDir:    baseDir,
		HTTPClient: &http.Client{Timeout: 10 * time.Minute},
		Now:        time.Now,
	}
	i.downloadFn = i.httpGet
	i.extractFn = extractTarZst
	return i
}

// BinaryPath is the absolute path to the bundled ollama binary.
func (i *OllamaInstaller) BinaryPath() string {
	return filepath.Join(i.BaseDir, "bin", "ollama")
}

// ModelsDir is where the bundled engine stores blobs (kept under the
// waired-owned dir so a root-spawned ollama and `ollama pull` share it).
func (i *OllamaInstaller) ModelsDir() string {
	return filepath.Join(i.BaseDir, "models")
}

// Active reports whether a bundled ollama binary is already present and
// executable.
func (i *OllamaInstaller) Active() bool {
	return assertExecutable(i.BinaryPath()) == nil
}

// Install downloads the pinned tarball (+ ROCm overlay on AMD), extracts
// it into BaseDir, and returns once BaseDir/bin/ollama is executable.
// progress may be nil.
func (i *OllamaInstaller) Install(ctx context.Context, progress func(OllamaInstallProgress)) error {
	if progress == nil {
		progress = func(OllamaInstallProgress) {}
	}
	arch, err := ollamaLinuxArch()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(i.BaseDir, 0o755); err != nil {
		return fmt.Errorf("ollama install: mkdir %s: %w", i.BaseDir, err)
	}

	baseURL := fmt.Sprintf("%s/v%s/ollama-linux-%s.tar.zst", OllamaDownloadURLBase, OllamaPinnedVersion, arch)
	progress(OllamaInstallProgress{Stage: "download", Message: baseURL})
	body, err := i.downloadFn(ctx, baseURL, ByteProgress(progress, "download"))
	if err != nil {
		return fmt.Errorf("ollama install: download base: %w", err)
	}
	if len(body) < ollamaTarballMinBytes {
		return fmt.Errorf("ollama install: base tarball suspiciously small (%d bytes); refusing to extract", len(body))
	}
	progress(OllamaInstallProgress{Stage: "extract", Message: i.BaseDir})
	if err := i.extractFn(body, i.BaseDir); err != nil {
		return fmt.Errorf("ollama install: extract base: %w", err)
	}

	// AMD: overlay the ROCm runtime ZIP/tgz on top of the base install
	// (the base bundles CUDA + CPU only). Best-effort — a failure here
	// degrades to CPU/Vulkan rather than aborting the whole install.
	if i.GPUVendor == "amd" {
		rocmURL := fmt.Sprintf("%s/v%s/ollama-linux-%s-rocm.tar.zst", OllamaDownloadURLBase, OllamaPinnedVersion, arch)
		progress(OllamaInstallProgress{Stage: "download-rocm", Message: rocmURL})
		if rocm, derr := i.downloadFn(ctx, rocmURL, ByteProgress(progress, "download-rocm")); derr == nil && len(rocm) >= ollamaTarballMinBytes {
			if eerr := i.extractFn(rocm, i.BaseDir); eerr != nil {
				progress(OllamaInstallProgress{Stage: "download-rocm", Message: "ROCm overlay extract failed; continuing without it: " + eerr.Error()})
			}
		} else {
			progress(OllamaInstallProgress{Stage: "download-rocm", Message: "ROCm overlay unavailable; continuing without it"})
		}
	}

	progress(OllamaInstallProgress{Stage: "activate", Message: i.BinaryPath()})
	if err := assertExecutable(i.BinaryPath()); err != nil {
		return fmt.Errorf("ollama install: %s not executable after extract: %w", i.BinaryPath(), err)
	}
	return nil
}

// httpGet buffers download.Fetch (the shared progress-streaming HTTP
// download — progressReader lives there since #615 extracted it for the
// darwin Ollama.app flow) into memory, preserving downloadFn's []byte
// contract.
func (i *OllamaInstaller) httpGet(ctx context.Context, url string, onProgress func(completed, total, bytesPerSec int64)) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := download.Fetch(ctx, i.HTTPClient, url, &buf, i.Now, onProgress); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// extractTarZst unpacks a zstd-compressed tar (the ollama-linux 0.30+
// release layout has bin/ollama + lib/ollama/...) into destDir. The
// zstd layer is decompressed IN-PROCESS (klauspost/compress — no zstd
// binary required on the host) and streamed into the system tar via
// stdin, so symlink/permission semantics stay identical to the old
// `tar -xzf` path and the multi-GB decompressed stream never lands in
// memory or on disk as a whole.
func extractTarZst(body []byte, destDir string) error {
	zr, err := zstd.NewReader(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("zstd: %w", err)
	}
	defer zr.Close()
	cmd := exec.Command("tar", "-xf", "-", "-C", destDir)
	cmd.Stdin = zr
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ollamaLinuxArch maps GOARCH to the ollama release arch token.
func ollamaLinuxArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("ollama install: unsupported GOARCH %q (linux amd64/arm64 only)", runtime.GOARCH)
	}
}
