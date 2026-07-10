// Package codeui vendors and supervises the bundled OpenCode coding-agent
// web UI (#429). waired ships a ready-to-run coding agent in the browser by
// downloading a pinned, self-contained `opencode` binary into a waired-owned
// directory and running `opencode serve` as a foreground child on a dedicated
// loopback port — the same "vendor + supervise" stance as the bundled Ollama
// engine (internal/runtime/ollama_install.go). The instance is wired to the
// agent's no-token data-plane gateway via an isolated OPENCODE_CONFIG_DIR that
// reuses the existing waired provider plugin, so it never touches the user's
// own ~/.config/opencode.
package codeui

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OpenCodePinnedVersion is the bundled OpenCode CLI release. OpenCode ships
// the browser web UI embedded in this single self-contained binary (verified:
// `opencode serve` serves the UI from the binary, not a CDN), so the bundle
// has no Node/Bun runtime dependency.
//
// renovate: datasource=github-releases depName=sst/opencode
const OpenCodePinnedVersion = "1.17.13"

// OpenCodeDownloadURLBase is the GitHub release prefix. A var so tests can
// point it at a local httptest server.
var OpenCodeDownloadURLBase = "https://github.com/sst/opencode/releases/download"

// archiveMinBytes is a sanity floor: the real CLI archive is tens of MB. A
// response far below this is an error page / truncated download, not a
// release, so we refuse to extract it (the sha256 pin is the real integrity
// gate; this just yields a clearer error).
var archiveMinBytes = 8 << 20 // 8 MiB (var so tests can lower it)

// platformArtifact describes one platform's release asset and its pinned
// sha256. sst/opencode publishes no per-asset checksum file, so the digests
// are self-pinned: a Renovate version bump MUST recompute this whole set
// (scripts/dev/update-opencode-sha.sh does it; see renovate.json +
// docs/records). The values were computed from the official v1.17.13
// release assets.
type platformArtifact struct {
	name   string // asset filename, e.g. "opencode-linux-x64.tar.gz"
	sha256 string
	isZip  bool // true: .zip (darwin/windows); false: .tar.gz (linux)
}

// artifacts maps "GOOS/GOARCH" to the v<OpenCodePinnedVersion> asset.
var artifacts = map[string]platformArtifact{
	"linux/amd64":   {"opencode-linux-x64.tar.gz", "157afa289d1a8d9372de0ce19ac726119b937a1f6b201808d46f06e4e59bb348", false},
	"linux/arm64":   {"opencode-linux-arm64.tar.gz", "bbaccdd374aaab66cd97c7f8ad1c080aa393610fa5f80ee8dfc007f9500afaf9", false},
	"darwin/amd64":  {"opencode-darwin-x64.zip", "0bf3d9d134097ca698b83f64c55db960d6d2d0c409069bf4cfd863e5de503b4a", true},
	"darwin/arm64":  {"opencode-darwin-arm64.zip", "dd016d3e26b347d675ab26c45d1e287545912d5c4c49fa0770b622d4a1367e23", true},
	"windows/amd64": {"opencode-windows-x64.zip", "18aa3df701a6eafcca201b5bcc63e086c96c8daa6ae2495cf718e12cb0ce3361", true},
	"windows/arm64": {"opencode-windows-arm64.zip", "bafec2dd6b89055910284ba910d59605295866563ccdb3d035c0c4b887dd11e6", true},
}

// InstallProgress is one human-facing update during Install.
type InstallProgress struct {
	Stage   string // "download" | "verify" | "extract" | "activate"
	Message string
}

// Installer downloads + extracts the pinned OpenCode binary into BaseDir
// (typically <state-dir>/runtimes/codeui). The binary lands at
// BaseDir/bin/opencode[.exe]; the isolated config/data/workspace dirs hang
// off the same BaseDir so the whole bundled instance is self-contained.
type Installer struct {
	BaseDir    string
	HTTPClient *http.Client

	// LocalBinary (WAIRED_CODEUI_BINARY) points at an already-present
	// opencode binary, skipping the download/verify entirely. The
	// developer / offline / forks path.
	LocalBinary string

	// downloadFn is a seam so tests exercise the orchestration without
	// network. Defaulted by NewInstaller.
	downloadFn func(ctx context.Context, url string) ([]byte, error)
}

// NewInstaller wires defaults rooted at baseDir.
func NewInstaller(baseDir string) *Installer {
	i := &Installer{
		BaseDir:    baseDir,
		HTTPClient: &http.Client{Timeout: 10 * time.Minute},
	}
	i.downloadFn = i.httpGet
	return i
}

// binaryName is "opencode" everywhere except Windows.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "opencode.exe"
	}
	return "opencode"
}

// BinaryPath is the absolute path to the bundled opencode binary.
func (i *Installer) BinaryPath() string {
	if i.LocalBinary != "" {
		return i.LocalBinary
	}
	return filepath.Join(i.BaseDir, "bin", binaryName())
}

// ConfigDir is the isolated XDG_CONFIG_HOME for the bundled instance. opencode
// reads its config from <ConfigDir>/opencode/ (see OpenCodeConfigDir), so this
// is the value exported as XDG_CONFIG_HOME — the real isolation knob in
// opencode 1.17.x (verified on 1.17.13; OPENCODE_CONFIG_DIR is ignored). The user's
// ~/.config/opencode is never touched.
func (i *Installer) ConfigDir() string { return filepath.Join(i.BaseDir, "config") }

// OpenCodeConfigDir is <ConfigDir>/opencode — the directory opencode actually
// loads opencode.json and plugin/waired.js from when XDG_CONFIG_HOME=ConfigDir.
// The waired provider plugin + default-model config are seeded here.
func (i *Installer) OpenCodeConfigDir() string { return filepath.Join(i.ConfigDir(), "opencode") }

// DataDir backs XDG_DATA_HOME for the bundled instance (sessions, etc.).
func (i *Installer) DataDir() string { return filepath.Join(i.BaseDir, "data") }

// LogDir captures the server's stdout/stderr.
func (i *Installer) LogDir() string { return filepath.Join(i.BaseDir, "logs") }

// Active reports whether the opencode binary is present and executable.
func (i *Installer) Active() bool {
	fi, err := os.Stat(i.BinaryPath())
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

// versionFilePath records which OpenCodePinnedVersion the on-disk binary was
// installed from, so a waired upgrade that bumps the pin re-downloads the new
// opencode instead of keeping the stale one.
func (i *Installer) versionFilePath() string { return filepath.Join(i.BaseDir, "opencode-version") }

// InstalledVersion returns the pin the present binary was installed from, or
// "" when unknown / not installed.
func (i *Installer) InstalledVersion() string {
	b, err := os.ReadFile(i.versionFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// NeedsInstall reports whether Install must (re)download: the binary is
// missing OR its recorded version differs from the current pin. A
// WAIRED_CODEUI_BINARY override never needs a download. This is what makes a
// waired update (new pin, via the update command or the installer) pull the
// matching opencode on the next "Open Coding Agent".
func (i *Installer) NeedsInstall() bool {
	if i.LocalBinary != "" {
		return false
	}
	return !i.Active() || i.InstalledVersion() != OpenCodePinnedVersion
}

// Install downloads the pinned archive for the current platform, verifies
// its sha256, extracts the single opencode binary into BaseDir/bin, and
// ensures the isolated dirs exist. progress may be nil. A WAIRED_CODEUI_BINARY
// override skips the download. Idempotent: an already-active install only
// (re)creates the dirs.
func (i *Installer) Install(ctx context.Context, progress func(InstallProgress)) error {
	if progress == nil {
		progress = func(InstallProgress) {}
	}
	for _, d := range []string{i.OpenCodeConfigDir(), i.DataDir(), i.LogDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("codeui install: mkdir %s: %w", d, err)
		}
	}
	// Skip the download only when the binary is present AND matches the
	// current pin (a waired upgrade bumps the pin, so a stale binary fails
	// this check and is replaced below).
	if !i.NeedsInstall() {
		return nil
	}

	art, ok := artifacts[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return fmt.Errorf("codeui install: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("%s/v%s/%s", OpenCodeDownloadURLBase, OpenCodePinnedVersion, art.name)
	progress(InstallProgress{Stage: "download", Message: url})
	body, err := i.downloadFn(ctx, url)
	if err != nil {
		return fmt.Errorf("codeui install: download %s: %w", art.name, err)
	}
	if len(body) < archiveMinBytes {
		return fmt.Errorf("codeui install: %s suspiciously small (%d bytes); refusing to extract", art.name, len(body))
	}

	progress(InstallProgress{Stage: "verify", Message: art.sha256})
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != art.sha256 {
		return fmt.Errorf("codeui install: %s sha256 mismatch: got %s want %s", art.name, got, art.sha256)
	}

	binDir := filepath.Join(i.BaseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("codeui install: mkdir %s: %w", binDir, err)
	}
	dst := filepath.Join(binDir, binaryName())
	progress(InstallProgress{Stage: "extract", Message: dst})
	if art.isZip {
		err = extractBinaryFromZip(body, dst)
	} else {
		err = extractBinaryFromTarGz(body, dst)
	}
	if err != nil {
		return fmt.Errorf("codeui install: extract %s: %w", art.name, err)
	}

	progress(InstallProgress{Stage: "activate", Message: dst})
	if !i.Active() {
		return fmt.Errorf("codeui install: %s not executable after extract", dst)
	}
	// Stamp the installed pin so a later waired upgrade (bumped pin) knows to
	// replace this binary. Best-effort: a write failure only forces a
	// re-download next time, never a wrong-version run.
	_ = os.WriteFile(i.versionFilePath(), []byte(OpenCodePinnedVersion+"\n"), 0o644)
	return nil
}

func (i *Installer) httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// isOpenCodeEntry matches the single binary inside a release archive
// ("opencode" on linux/darwin, "opencode.exe" on windows).
func isOpenCodeEntry(name string) bool {
	base := filepath.Base(name)
	return base == "opencode" || base == "opencode.exe"
}

// extractBinaryFromTarGz writes the opencode entry of a .tar.gz archive to
// dst (mode 0755). Decompression is in-process (no host tar dependency).
func extractBinaryFromTarGz(body []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !isOpenCodeEntry(hdr.Name) {
			continue
		}
		return writeBinary(dst, tr)
	}
	return fmt.Errorf("tar: no opencode binary entry found")
}

// extractBinaryFromZip writes the opencode entry of a .zip archive to dst.
func extractBinaryFromZip(body []byte, dst string) error {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isOpenCodeEntry(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("zip open %s: %w", f.Name, err)
		}
		err = writeBinary(dst, rc)
		_ = rc.Close()
		return err
	}
	return fmt.Errorf("zip: no opencode binary entry found")
}

// writeBinary streams r into dst (truncating any prior copy) with mode 0755.
func writeBinary(dst string, r io.Reader) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// O_CREATE honours the mode only when the file is new; ensure exec bit
	// on overwrite too.
	return os.Chmod(dst, 0o755)
}
