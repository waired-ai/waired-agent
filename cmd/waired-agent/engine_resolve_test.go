package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// fakeBundledOllama lays down the on-disk shape resolveOllamaBinary
// stats: <stateDir>/runtimes/ollama/bin/ollama, a regular executable
// file. That is what `waired runtimes install ollama` produces, and
// what the setup executor installs when the wizard asks for an engine.
// Returns the path so callers can assert on it.
func fakeBundledOllama(t *testing.T, stateDir string) string {
	t.Helper()
	bin := bundledOllamaBinPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// sealPATH removes every way resolveOllamaBinary could find an engine
// other than the state dir, so a positive result can only have come
// from the state-dir stat. A host with a real ollama at an OS
// well-known candidate path (macOS's Ollama.app, Windows's
// %ProgramFiles%) would still resolve, which is why the not-found
// assertions below skip rather than fail in that case.
func sealPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
}

// THE #179 REGRESSION BAR. An engine installed under the state dir —
// the normal case, and the only one on Linux — with nothing on $PATH
// must read as installed. The old probe was exec.LookPath, which said
// "no engine" on exactly the hosts waired had set up itself.
//
// Runs on all three GOOS values because the state-dir branch is the
// first thing resolveOllamaBinary tries, before any OS-specific
// fallback: a bundled install is visible everywhere.
func TestEngineInstalledOnHost_StateDirEngineWithEmptyPATH(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()
	fakeBundledOllama(t, stateDir)

	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			for _, source := range []string{agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceReuse, ""} {
				cfg := agentconfig.InferenceConfig{OllamaSource: source}
				if !engineInstalledOnHost(goos, stateDir, cfg, catalog.RuntimeOllama) {
					t.Errorf("engineInstalledOnHost(%q, source=%q) = false, want true "+
						"(engine present under the state dir)", goos, source)
				}
			}
		})
	}
}

// The Linux bundled-mode strictness, and the two ways out of it. This
// is the branch that makes the resolver disagree with a plain PATH
// probe in the OTHER direction: a system ollama on $PATH is NOT the
// engine this daemon would spawn, so it must not count as installed.
func TestResolveOllamaBinary_LinuxBundledIsStrict(t *testing.T) {
	sealPATH(t)
	dir := t.TempDir()
	stub := filepath.Join(dir, "ollama")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// Bundled + linux: only the state-dir binary qualifies.
	stateDir := t.TempDir()
	if got, err := resolveOllamaBinary("linux", stateDir, false); err == nil {
		t.Fatalf("resolveOllamaBinary(linux, bundled) = %q, want an error "+
			"(a system ollama on PATH is not the bundled engine)", got)
	}

	// Way out 1: reuse mode borrows the user's engine, so PATH is back.
	got, err := resolveOllamaBinary("linux", stateDir, true)
	if err != nil {
		t.Fatalf("resolveOllamaBinary(linux, reuse): %v, want the PATH stub", err)
	}
	if filepath.Base(got) != "ollama" {
		t.Errorf("reuse resolved %q, want the PATH stub", got)
	}

	// Way out 2: install the bundled engine. It wins over PATH.
	want := fakeBundledOllama(t, stateDir)
	if got, err := resolveOllamaBinary("linux", stateDir, false); err != nil || got != want {
		t.Errorf("resolveOllamaBinary(linux, bundled) = (%q, %v), want (%q, nil)", got, err, want)
	}
}

// Windows and macOS keep the PATH / well-known-paths fallback: their
// "bundled" installs land outside the state dir, so refusing to look
// there would report every GUI-installed Ollama as missing.
//
// Asserted on the error IDENTITY rather than on a stub being found,
// because download.ResolveBinary's binary name ("ollama" vs
// "ollama.exe") is fixed by the build tag of the RUNNING OS, not by the
// goos argument. Reaching download.ErrNotInstalled proves the fallback
// ran; the Linux strict branch returns its own error and never gets
// there. The fallback actually resolving is covered by the reuse case
// in TestResolveOllamaBinary_LinuxBundledIsStrict, which can use the
// running OS's real binary name.
func TestResolveOllamaBinary_NonLinuxKeepsFallback(t *testing.T) {
	sealPATH(t)
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			stateDir := t.TempDir() // deliberately empty: no bundled engine
			_, err := resolveOllamaBinary(goos, stateDir, false)
			if err == nil {
				t.Skip("host has an ollama at an OS well-known path; cannot exercise not-found")
			}
			if !errors.Is(err, download.ErrNotInstalled) {
				t.Errorf("resolveOllamaBinary(%q, bundled) err = %v, want download.ErrNotInstalled "+
					"(the Linux-strict fast-fail must not apply off Linux)", goos, err)
			}
		})
	}

	// The contrast that makes the above meaningful: same inputs, Linux,
	// bundled — a different error, raised before the fallback runs.
	stateDir := t.TempDir()
	_, err := resolveOllamaBinary("linux", stateDir, false)
	if err == nil {
		t.Fatal("resolveOllamaBinary(linux, bundled) with an empty state dir succeeded, want the strict error")
	}
	if errors.Is(err, download.ErrNotInstalled) {
		t.Errorf("linux bundled err = %v, want the strict bundled-path error, not the fallback's", err)
	}
	if !strings.Contains(err.Error(), bundledOllamaBinPath(stateDir)) {
		t.Errorf("linux bundled err = %v, want it to name the expected install path", err)
	}
}

// THE #179 REGRESSION BAR at the wizard's own endpoint. /setup/state's
// engine_installed must agree with what the daemon resolves, on the host
// shape where they used to disagree: engine under the state dir, nothing
// on $PATH.
//
// It also pins the freshness half of the fix. The two calls happen
// microseconds apart with the install in between; against the old
// profiler-backed implementation the second would have been served from
// a 30 s cache and still said false — which is precisely how the wizard
// came to sit on a stale snapshot until the executor lease expired.
//
// The provider is built with a nil profiler on purpose: this path must
// no longer touch it at all.
func TestSetupEngineState_SeesStateDirEngineImmediately(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()
	p := &agentInferenceProvider{
		stateDir: stateDir,
		store:    catalog.NewStore(filepath.Join(stateDir, "state.json")),
	}
	ctx := context.Background()

	if installed, ready := p.setupEngineState(ctx, catalog.RuntimeOllama); installed || ready {
		t.Fatalf("setupEngineState before the install = (%v, %v), want (false, false)", installed, ready)
	}

	fakeBundledOllama(t, stateDir)

	installed, ready := p.setupEngineState(ctx, catalog.RuntimeOllama)
	if !installed {
		t.Error("setupEngineState installed = false right after the executor's install " +
			"(engine is under the state dir, nothing on PATH — this is #179)")
	}
	if ready {
		t.Error("setupEngineState ready = true with no active model; readiness is a separate gate")
	}
}

// An unknown engine kind is not installed, and vLLM viability keys off
// the venv rather than anything on PATH. The vLLM installer stubs on
// Windows/darwin always report "no active install", so this asserts
// only the absent case portably.
func TestEngineInstalledOnHost_UnknownAndAbsentVLLM(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()
	cfg := agentconfig.InferenceConfig{}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if engineInstalledOnHost(goos, stateDir, cfg, catalog.RuntimeVLLM) {
			t.Errorf("engineInstalledOnHost(%q, vllm) = true with no venv", goos)
		}
		if engineInstalledOnHost(goos, stateDir, cfg, "tensorrt") {
			t.Errorf("engineInstalledOnHost(%q, tensorrt) = true for an unknown engine", goos)
		}
	}
}
