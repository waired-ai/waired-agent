package runtime

// Bundled Ollama installer. waired's "out of the box" stance is to
// package Ollama itself: download a pinned official release archive into a
// waired-owned directory and supervise the binary as a foreground child
// (the existing OllamaAdapter spawn model) — no system service, no
// systemctl, no /Applications, no %ProgramFiles%. See #188 for the model
// and #488 for the rule that made it the only one: every engine instance
// the agent serves with is one waired installed and manages.
//
// Linux got here first (#188). #492 and #493 brought macOS and Windows
// onto the same path, which is why this file lost its `//go:build linux`
// tag: the orchestration below is identical everywhere, and the three real
// differences — which asset, where its payload lands, and how the archive
// is unpacked — are isolated in ollama_release.go and ollama_extract_*.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/download"
)

// OllamaDownloadURLBase is the GitHub release prefix. A var so tests can
// point it at a local httptest server.
var OllamaDownloadURLBase = "https://github.com/ollama/ollama/releases/download"

// ollamaArchiveMinBytes is a sanity floor: every real engine archive is
// hundreds of MB. It is not the integrity check — that is the SHA-256
// comparison below — but it is the one that produces a readable message,
// because a captive portal's login page fails this before it fails as a
// hash mismatch.
var ollamaArchiveMinBytes int64 = 50 << 20 // 50 MiB (var so tests can lower it)

// ollamaStageDirName is where archives are staged, inside BaseDir so the
// download lands on the same volume it will be extracted onto. Swept at
// the start of every install: a killed install cannot clean up after
// itself, and nothing else ever swept the ~1.4 GB it leaves behind (#191).
const ollamaStageDirName = ".stage"

// OllamaInstaller downloads + extracts a pinned Ollama release into
// BaseDir (typically <state-dir>/runtimes/ollama). The binary lands at
// BaseDir/bin/ollama[.exe], which OllamaAdapter is pointed at.
type OllamaInstaller struct {
	BaseDir    string
	HTTPClient *http.Client
	Now        func() time.Time

	// WantROCmOverlay makes Install fetch the AMD ROCm runtime on top of
	// the base archive, on the OSes that publish one. Set by the caller
	// from hardware detection; false (the default) installs the base only,
	// which is the right answer for NVIDIA, Intel, Apple and for the AMD
	// GPUs Ollama's ROCm build does not cover.
	WantROCmOverlay bool

	// Seams (defaulted by NewOllamaInstaller) so tests exercise the
	// orchestration without network or tar. onProgress (nil-ok) receives
	// throttled byte updates while the body streams down.
	downloadFn  func(ctx context.Context, url, destPath string, onProgress func(completed, total, bytesPerSec int64)) (int64, error)
	extractFn   func(archivePath, destDir string, fresh bool) error
	checksumsFn func(ctx context.Context) (map[string]string, error)
}

// NewOllamaInstaller wires defaults rooted at baseDir.
func NewOllamaInstaller(baseDir string) *OllamaInstaller {
	i := &OllamaInstaller{
		BaseDir:    baseDir,
		HTTPClient: newOllamaDownloadClient(),
		Now:        time.Now,
	}
	i.downloadFn = i.fetchToFile
	i.extractFn = extractOllamaArchive
	i.checksumsFn = i.fetchChecksums
	return i
}

// newOllamaDownloadClient builds the HTTP client for the ~1.4 GB archive.
//
// Deliberately NO http.Client.Timeout: that is a whole-request cap, so it
// counts body streaming and fails deterministically below roughly 2.5 MB/s
// no matter how healthy the transfer is (#189). The phases that genuinely
// should be bounded by elapsed time -- connect, TLS, waiting for response
// headers -- are bounded individually here, and the body is bounded on
// no-progress by download.Fetch's stall watchdog.
func newOllamaDownloadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// BinaryPath is the absolute path to the bundled ollama binary.
func (i *OllamaInstaller) BinaryPath() string {
	return filepath.Join(i.BaseDir, "bin", OllamaBinaryName(runtime.GOOS))
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

// Install downloads the pinned archive (+ ROCm overlay on AMD, where one
// exists), extracts it under BaseDir, and returns once the binary is
// executable. progress may be nil.
func (i *OllamaInstaller) Install(ctx context.Context, progress func(OllamaInstallProgress)) error {
	if progress == nil {
		progress = func(OllamaInstallProgress) {}
	}
	rel, err := ollamaReleaseFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	destDir := i.BaseDir
	if rel.ExtractSub != "" {
		destDir = filepath.Join(i.BaseDir, rel.ExtractSub)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("ollama install: mkdir %s: %w", destDir, err)
	}

	stageDir := filepath.Join(i.BaseDir, ollamaStageDirName)
	_ = os.RemoveAll(stageDir)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return fmt.Errorf("ollama install: mkdir %s: %w", stageDir, err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	// The checksums come first and their absence is fatal, deliberately.
	// The version is pinned, so whether this release publishes the file is
	// something a version bump verifies once — not something every install
	// on every host rediscovers, and certainly not something that should
	// silently downgrade to "HTTPS and a size floor" in the field.
	sums, err := i.checksumsFn(ctx)
	if err != nil {
		return fmt.Errorf("ollama install: %w", err)
	}

	archive, err := i.fetchVerified(ctx, rel.Base, sums, stageDir, "download", progress)
	if err != nil {
		return fmt.Errorf("ollama install: base: %w", err)
	}
	progress(OllamaInstallProgress{Stage: "extract", Message: destDir})
	// fresh: this archive IS the install, so an extractor that has to worry
	// about what a previous version left behind may replace the target
	// wholesale. The overlay below is additive and must not.
	if err := i.extractFn(archive, destDir, true); err != nil {
		return fmt.Errorf("ollama install: extract base: %w", err)
	}
	// Free the archive before the overlay so peak disk stays one archive
	// wide rather than two.
	_ = os.Remove(archive)

	// AMD: overlay the ROCm runtime on top of the base install (the base
	// bundles CUDA/Vulkan + CPU only). Best-effort — a failure here
	// degrades to CPU/Vulkan rather than aborting the whole install.
	if i.WantROCmOverlay && rel.ROCm != "" {
		overlay, derr := i.fetchVerified(ctx, rel.ROCm, sums, stageDir, "download-rocm", progress)
		if derr != nil {
			progress(OllamaInstallProgress{Stage: "download-rocm", Message: "ROCm overlay unavailable; continuing without it: " + derr.Error()})
		} else if eerr := i.extractFn(overlay, destDir, false); eerr != nil {
			progress(OllamaInstallProgress{Stage: "download-rocm", Message: "ROCm overlay extract failed; continuing without it: " + eerr.Error()})
		}
	}

	progress(OllamaInstallProgress{Stage: "activate", Message: i.BinaryPath()})
	if err := assertExecutable(i.BinaryPath()); err != nil {
		return fmt.Errorf("ollama install: %s not usable after extract: %w", i.BinaryPath(), err)
	}
	return nil
}

// fetchVerified downloads one release asset into stageDir and returns its
// path, having proven it is byte-for-byte the asset upstream published.
func (i *OllamaInstaller) fetchVerified(
	ctx context.Context, asset string, sums map[string]string,
	stageDir, stage string, progress func(OllamaInstallProgress),
) (string, error) {
	want, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("the release checksum list has no entry for %s", asset)
	}
	url := fmt.Sprintf("%s/v%s/%s", OllamaDownloadURLBase, OllamaPinnedVersion, asset)
	dest := filepath.Join(stageDir, asset)
	progress(OllamaInstallProgress{Stage: stage, Message: url})
	n, err := i.downloadFn(ctx, url, dest, ByteProgress(progress, stage))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	if n < ollamaArchiveMinBytes {
		return "", fmt.Errorf("%s is suspiciously small (%d bytes); refusing to extract", asset, n)
	}
	got, err := sha256File(dest)
	if err != nil {
		return "", fmt.Errorf("checksum %s: %w", asset, err)
	}
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf(
			"%s does not match the checksum upstream published (got %s, want %s); refusing to extract",
			asset, got, want)
	}
	return dest, nil
}

// fetchChecksums reads the release's own sha256sum.txt. It is a couple of
// kilobytes, so it streams straight into memory with no progress
// reporting.
func (i *OllamaInstaller) fetchChecksums(ctx context.Context) (map[string]string, error) {
	url := fmt.Sprintf("%s/v%s/sha256sum.txt", OllamaDownloadURLBase, OllamaPinnedVersion)
	var buf bytes.Buffer
	if _, err := download.Fetch(ctx, i.HTTPClient, url, &buf, i.Now, nil); err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	sums := parseSHA256Sums(buf.Bytes())
	if len(sums) == 0 {
		return nil, fmt.Errorf("%s carried no checksums", url)
	}
	return sums, nil
}

// parseSHA256Sums reads the `sha256sum` output format upstream publishes:
// one "<64 hex>  <name>" line per asset, where the name is written
// "./ollama-darwin.tgz". Unparseable lines are skipped rather than fatal —
// the caller's real check is whether the asset it wants is in the map.
func parseSHA256Sums(body []byte) map[string]string {
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./")
		if name == "" {
			continue
		}
		sums[name] = strings.ToLower(fields[0])
	}
	return sums
}

// sha256File hashes a staged archive. Hashing after the download rather
// than through a tee keeps downloadFn a plain "bytes to a path" seam that
// a test double can satisfy, and the second pass comes off the page cache.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchToFile streams url into destPath, reporting byte progress, and
// returns the number of bytes written.
//
// The archive is staged on disk rather than buffered in memory (which is
// what the Linux path did until #492). Windows needs it — archive/zip
// requires random access — and it also takes ~1.4 GB of resident memory
// off a host that is about to spend it on a model instead.
func (i *OllamaInstaller) fetchToFile(
	ctx context.Context, url, destPath string,
	onProgress func(completed, total, bytesPerSec int64),
) (int64, error) {
	f, err := os.Create(destPath)
	if err != nil {
		return 0, err
	}
	n, err := download.Fetch(ctx, i.HTTPClient, url, f, i.Now, onProgress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(destPath)
		return n, err
	}
	return n, nil
}
