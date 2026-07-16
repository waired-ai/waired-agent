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
	"time"

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

const ollamaAppDest = "/Applications"

// installOllamaApp is a seam so tests exercise installOllama's
// resolve/confirm/orchestration logic without downloading ~160MB.
var installOllamaApp = installOllamaAppImpl

// installOllama (macOS) installs the official Ollama.app into
// /Applications. If an ollama is already resolvable (PATH or a
// well-known install path) it is reused and nothing is downloaded.
//
// This is the manual `waired runtimes install` equivalent of what the
// one-liner installer (packaging/install/install.sh) does for fresh
// hosts. Unlike Linux's bundled-tarball model the app is global, not
// per-state-dir, so stateDir is unused here.
func installOllama(yes bool, stateDir string) error {
	_ = stateDir
	if path, err := download.ResolveBinary(""); err == nil {
		fmt.Printf("Ollama already present at %s — nothing to do.\n", path)
		fmt.Println("Run `waired runtimes status` to confirm the agent sees it.")
		return nil
	}

	if !yes && !confirmTTY(fmt.Sprintf("Download and install the official Ollama.app into %s ?", ollamaAppDest)) {
		return errors.New("aborted by user")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	fmt.Println("Installing Ollama.app (downloading the official release)...")
	if err := installOllamaApp(ctx); err != nil {
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
// unzips it, and copies Ollama.app into /Applications. /Applications is
// group-writable by admins, so the copy succeeds for the typical
// single-admin Mac without sudo; non-admin users get a clear error
// pointing at the one-liner installer (which escalates via sudo).
//
// The download runs in Go (download.Fetch) so the multi-hundred-MB
// transfer draws the same live progress bar + please-wait hint as the
// Linux tarball install, instead of the former buffered `curl -fsSL`
// silence (#615). unzip/cp stay as shelled-out steps.
func installOllamaAppImpl(ctx context.Context) error {
	progress := newOllamaInstallRenderer(os.Stdout, isTerminal(os.Stdout), "Ollama.app")

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
	progress(infruntime.OllamaInstallProgress{Stage: "unzip", Message: tmp})
	if err := runDarwinCmd(ctx, "unzip", "-q", "-o", zipPath, "-d", tmp); err != nil {
		return fmt.Errorf("unzip: %w", err)
	}
	app := filepath.Join(tmp, "Ollama.app")
	if _, err := os.Stat(app); err != nil {
		return fmt.Errorf("archive did not contain Ollama.app (layout changed?): %w", err)
	}
	progress(infruntime.OllamaInstallProgress{Stage: "install", Message: ollamaAppDest})
	if err := runDarwinCmd(ctx, "cp", "-R", app, ollamaAppDest+"/"); err != nil {
		return fmt.Errorf("copy into %s — move Ollama.app there manually, or use the waired "+
			"one-liner installer which escalates via sudo: %w", ollamaAppDest, err)
	}
	writeWairedManagedMarker(filepath.Join(ollamaAppDest, "Ollama.app"))
	return nil
}

// writeWairedManagedMarker drops the waired-managed marker at the bundle
// root so a later `waired init` recognises this Ollama as waired's own and
// skips the bundled-vs-reuse question (setup.DetectOllama.WairedManaged).
// Best-effort: a marker-write failure only means one extra question later.
func writeWairedManagedMarker(dir string) {
	body := []byte(`{"managed_by":"waired","installer":"waired runtimes install ollama"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, setup.WairedManagedMarkerName), body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not write the waired-managed marker: %v\n", err)
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
