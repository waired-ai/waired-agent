//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/download"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// ollamaDarwinDefaultURL is the official universal (amd64+arm64)
// Ollama.app download. macOS Ollama ships as a signed .app inside this
// zip rather than a bare CLI tarball (the Linux model), so we install
// the whole app bundle into /Applications; its CLI then lives at
// /Applications/Ollama.app/Contents/Resources/ollama — the first path
// download.ResolveBinary probes. Override via WAIRED_OLLAMA_DARWIN_URL
// to pin a version or point at a mirror (matches install.sh).
const ollamaDarwinDefaultURL = "https://github.com/ollama/ollama/releases/latest/download/Ollama-darwin.zip"

// ollamaAppDest is where the app bundle lands. A var, not a const, so the
// installer's own test can point it at a temp dir and run installOllamaAppImpl
// for real instead of stubbing the function that holds the behaviour.
var ollamaAppDest = "/Applications"

// installOllamaApp is a seam so tests exercise installOllama's
// resolve/confirm/orchestration logic without downloading ~160MB.
var installOllamaApp = installOllamaAppImpl

// installOllama (macOS) installs the official Ollama.app into
// /Applications. If an ollama is already resolvable (PATH or a
// well-known install path) it is reused and nothing is downloaded.
//
// This is the manual `waired runtimes install` equivalent of what the
// one-liner installer (packaging/install/install.sh) does for fresh
// hosts. Unlike Linux's bundled-tarball model the app itself is global,
// not per-state-dir; stateDir is where we record that the global app is
// ours (#329), since the bundle cannot carry that marker itself.
//
// sink, when non-nil, receives the same progress events the terminal
// renderer draws — that is how the browser wizard gets the download it
// used to have no view of (waired-agent#197). nil for every caller that
// is not the setup executor.
func installOllama(yes bool, stateDir string, sink func(infruntime.OllamaInstallProgress)) error {
	if path, err := download.ResolveBinary(""); err == nil {
		fmt.Printf("Ollama already present at %s — nothing to do.\n", path)
		fmt.Println("Run `waired runtimes status` to confirm the agent sees it.")
		return nil
	}

	if !yes && !confirmTTY(fmt.Sprintf("Download and install the official Ollama.app into %s ?", ollamaAppDest)) {
		return errors.New("aborted by user")
	}

	// A backstop, not the working bound: the download itself is bounded by
	// download.Fetch's no-progress watchdog (#189).
	budget := ollamaInstallTimeout(os.Getenv)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	fmt.Println("Installing Ollama.app (downloading the official release)...")
	if err := installOllamaApp(ctx, stateDir, sink); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf(
				"ollama install: timed out after %s (raise it with %s, e.g. %s=3h): %w",
				budget, ollamaInstallTimeoutEnv, ollamaInstallTimeoutEnv, err)
		}
		return fmt.Errorf("ollama install: %w", err)
	}
	fmt.Println("Ollama installed. Launch it once so the 127.0.0.1:11434 server starts:")
	fmt.Println("  open -a Ollama")
	fmt.Println("waired-agent will adopt it on the next engine start.")
	return nil
}

func ollamaDarwinURL() string {
	if u := os.Getenv("WAIRED_OLLAMA_DARWIN_URL"); u != "" {
		return u
	}
	return ollamaDarwinDefaultURL
}

// ollamaZipMinBytes is a sanity floor: the real Ollama-darwin.zip is
// hundreds of MB. A response far below this is an error page / partial
// download, not a release, so we refuse to unzip it. (Mirrors
// ollamaTarballMinBytes in the Linux installer; combined with HTTPS this
// is the v1 integrity posture.)
var ollamaZipMinBytes int64 = 50 << 20 // 50 MiB (var so tests can lower it)

// installOllamaAppImpl downloads Ollama-darwin.zip to a temp dir,
// extracts it, and copies Ollama.app into /Applications. /Applications is
// group-writable by admins, so the copy succeeds for the typical
// single-admin Mac without sudo; non-admin users get a clear error
// pointing at the one-liner installer (which escalates via sudo).
//
// The download runs in Go (download.Fetch) so the multi-hundred-MB
// transfer draws the same live progress bar + please-wait hint as the
// Linux tarball install, instead of the former buffered `curl -fsSL`
// silence (#615). Extraction and copy stay as shelled-out steps, but as
// `ditto` rather than unzip/cp: ditto is Apple's own bundle-aware copier
// and preserves the metadata a signed .app's seal is computed over. It is
// the same reason we no longer write a marker into the bundle (#329) —
// everything about a signed bundle has to arrive and stay byte-exact.
//
// Nothing is written inside the bundle. Ownership is recorded in the state
// dir instead; see setup.WriteDarwinManagedRecord.
func installOllamaAppImpl(ctx context.Context, stateDir string, sink func(infruntime.OllamaInstallProgress)) error {
	// The terminal bar and the daemon sink are peers: teeOllamaProgress
	// keeps the former even when the latter is absent.
	progress := teeOllamaProgress(
		newOllamaInstallRenderer(os.Stdout, isTerminal(os.Stdout), "Ollama.app"),
		sink,
	)

	tmp, err := os.MkdirTemp("", "waired-ollama-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	url := ollamaDarwinURL()
	zipPath := filepath.Join(tmp, "Ollama-darwin.zip")
	progress(infruntime.OllamaInstallProgress{Stage: "download", Message: url})
	if err := downloadOllamaZip(ctx, url, zipPath, progress); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	extracted := filepath.Join(tmp, "extracted")
	progress(infruntime.OllamaInstallProgress{Stage: "unzip", Message: extracted})
	if err := runDarwinCmd(ctx, "ditto", "-x", "-k", zipPath, extracted); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	app := filepath.Join(extracted, "Ollama.app")
	if _, err := os.Stat(app); err != nil {
		return fmt.Errorf("archive did not contain Ollama.app (layout changed?): %w", err)
	}
	dest := filepath.Join(ollamaAppDest, "Ollama.app")
	progress(infruntime.OllamaInstallProgress{Stage: "install", Message: ollamaAppDest})
	// ditto src dst (no trailing-slash subtlety, unlike cp -R): dst names the
	// bundle itself and is replaced wholesale if it already exists.
	if err := runDarwinCmd(ctx, "ditto", app, dest); err != nil {
		return fmt.Errorf("copy into %s — move Ollama.app there manually, or use the waired "+
			"one-liner installer which escalates via sudo: %w", ollamaAppDest, err)
	}
	clearQuarantine(ctx, dest)
	recordDarwinManaged(stateDir, dest, setup.DarwinManagedInstallerFresh)
	return nil
}

// clearQuarantine strips com.apple.quarantine from the freshly installed
// bundle. Belt and braces: a Go download sets no quarantine xattr today (it is
// LaunchServices, not the kernel, that applies one), so this is a no-op on the
// current path — it exists so a future change of download route cannot
// reintroduce the "Ollama is damaged" class through a different door.
// Best-effort by design: failing to remove an xattr that is probably not there
// must never fail an otherwise good install.
func clearQuarantine(ctx context.Context, appPath string) {
	if err := runDarwinCmd(ctx, "xattr", "-dr", "com.apple.quarantine", appPath); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not clear the quarantine xattr on %s: %v\n", appPath, err)
	}
}

// recordDarwinManaged records, OUTSIDE the bundle, that waired installed this
// Ollama.app. It used to be a file at the bundle root, which invalidated the
// bundle's code-signature seal and made macOS kill every exec of the engine
// (#329). Best-effort: losing the record only costs recognition later, whereas
// writing it into the bundle cost the whole install.
func recordDarwinManaged(stateDir, appPath, installer string) {
	if stateDir == "" {
		return
	}
	if err := setup.WriteDarwinManagedRecord(stateDir, appPath, installer); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not record the waired-managed engine: %v\n", err)
	}
}

// downloadOllamaZip streams url into dest, emitting byte-level progress
// as "download"-stage events, and refuses bodies below the size sanity
// floor.
func downloadOllamaZip(ctx context.Context, url, dest string, progress func(infruntime.OllamaInstallProgress)) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := download.Fetch(ctx, nil, url, f, nil, infruntime.ByteProgress(progress, "download"))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n < ollamaZipMinBytes {
		return fmt.Errorf("release zip suspiciously small (%d bytes); refusing to unzip", n)
	}
	return nil
}

func runDarwinCmd(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
