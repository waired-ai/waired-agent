package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
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
	// The stub carries the RUNNING OS's binary name, as the sibling test
	// below explains: download.ResolveBinary looks for the name fixed by the
	// build tag of the host, not by the goos argument. On Windows an
	// extensionless "ollama" is invisible to exec.LookPath, which consults
	// PATHEXT — so without the suffix this reads as "no engine on PATH" and
	// the reuse way-out below cannot be exercised at all (#216).
	stubName := "ollama"
	if runtime.GOOS == "windows" {
		stubName = "ollama.exe"
	}
	stub := filepath.Join(dir, stubName)
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
	if filepath.Base(got) != stubName {
		t.Errorf("reuse resolved %q, want the PATH stub %q", got, stubName)
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

// recordingRun is the version-runner seam's fake. It records the exact
// (engine, path) pair it was handed, because that pair IS the defect in
// #238 — a fake that dropped the path would make the failing case
// unwritable (CLAUDE.md §Test discipline).
type recordingRun struct {
	calls   int
	engine  string
	path    string
	version string // "" ⇒ report not-installed
}

func (r *recordingRun) run(_ context.Context, engine, path string) (bool, string) {
	r.calls++
	r.engine, r.path = engine, path
	if r.version == "" {
		return false, ""
	}
	return true, r.version
}

func sourceLabel(source string) string {
	if source == "" {
		return "unset"
	}
	return source
}

// THE #238 REGRESSION BAR. Product contract: the engine VERSION is
// probed on the binary the daemon resolved — state dir first — not on
// whatever `ollama` happens to name on $PATH. The old probe was
// hardware.defaultEngineVersion's exec.LookPath, the same predicate
// #179 removed one layer up: it answered "no engine, no version" on
// exactly the hosts waired had set up itself, and an unknown version
// makes the catalog's MinEngineVersion floors fail closed.
//
// Runs across all three GOOS values because the state-dir branch is the
// first thing resolveOllamaBinary tries, before any OS-specific
// fallback, and across all three ollama_source values because the
// version question is the same in bundled and reuse mode.
func TestEngineVersionOnHost_StateDirEngineWithEmptyPATH(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()
	want := fakeBundledOllama(t, stateDir)

	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, source := range []string{agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceReuse, ""} {
			t.Run(goos+"/"+sourceLabel(source), func(t *testing.T) {
				rec := &recordingRun{version: "0.31.1"}
				fn := engineVersionOnHost(goos, stateDir,
					agentconfig.InferenceConfig{OllamaSource: source}, rec.run)

				installed, ver := fn(context.Background(), catalog.RuntimeOllama)
				if !installed || ver != "0.31.1" {
					t.Errorf("engineVersionOnHost(%q, source=%q)(ollama) = (%v, %q), want (true, %q)",
						goos, source, installed, ver, "0.31.1")
				}
				if rec.calls != 1 {
					t.Fatalf("version runner called %d times, want 1", rec.calls)
				}
				if rec.engine != catalog.RuntimeOllama {
					t.Errorf("version runner engine = %q, want %q (the parse keys off the kind, "+
						"not the path)", rec.engine, catalog.RuntimeOllama)
				}
				if rec.path != want {
					t.Errorf("version runner path = %q, want %q (the state-dir binary — this is #238)",
						rec.path, want)
				}
			})
		}
	}
}

// The same bar with the REAL runner, so the fake above cannot be the
// only thing that ever calls it (CLAUDE.md §Test discipline: a seam
// needs a test on the real implementation too). This is the exact
// composition setupInference wires: engineVersionOnHost +
// hardware.EngineVersionAt, against an engine that exists only under
// the state dir with $PATH sealed.
//
// Unix-only, and the reason is a product fact rather than a test
// limitation: bundledOllamaBinPath deliberately has no ".exe", because
// only the Linux installer lands an engine inside the state dir. The
// Windows / macOS "bundled" install goes to an OS well-known location,
// which download.ResolveBinary covers and TestResolveOllamaBinary_*
// exercises.
func TestEngineVersionOnHost_RealRunnerExecutesTheStateDirBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the state-dir engine is a Linux/macOS shape; see bundledOllamaBinPath")
	}
	sealPATH(t)
	stateDir := t.TempDir()
	bin := bundledOllamaBinPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	// Prints what a fresh install prints: the warning line first, which
	// the parse must skip.
	script := "#!/bin/sh\n" +
		"echo 'Warning: could not connect to a running Ollama instance'\n" +
		"echo 'ollama version is 0.31.1'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	fn := engineVersionOnHost(runtime.GOOS, stateDir,
		agentconfig.InferenceConfig{}, hardware.EngineVersionAt)
	installed, ver := fn(context.Background(), catalog.RuntimeOllama)
	if !installed || ver != "0.31.1" {
		t.Errorf("engineVersionOnHost with the production runner = (%v, %q), want (true, %q) "+
			"(engine under the state dir, nothing on $PATH — this is #238)", installed, ver, "0.31.1")
	}
}

// The other direction: no engine resolves, so nothing is executed and
// the version is unknown rather than guessed. Asserted on Linux bundled
// mode, whose strictness is host-independent — the darwin/windows
// fallback can legitimately find a real ollama at an OS well-known path
// on the machine running the tests.
func TestEngineVersionOnHost_NoEngineRunsNothing(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir() // deliberately empty: no bundled engine
	rec := &recordingRun{version: "0.31.1"}
	fn := engineVersionOnHost("linux", stateDir, agentconfig.InferenceConfig{}, rec.run)

	if installed, ver := fn(context.Background(), catalog.RuntimeOllama); installed || ver != "" {
		t.Errorf("engineVersionOnHost(linux, empty state dir)(ollama) = (%v, %q), want (false, \"\")",
			installed, ver)
	}
	if rec.calls != 0 {
		t.Errorf("version runner called %d times with no engine to run, want 0", rec.calls)
	}
}

// vLLM's version comes from the venv the installer verified, and an
// unknown engine kind has no version at all. Neither may execute
// anything: the vLLM installer already recorded the version, and
// running an unknown binary name would be the PATH probe again.
func TestEngineVersionOnHost_VLLMAndUnknownExecuteNothing(t *testing.T) {
	sealPATH(t)
	stateDir := t.TempDir()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			rec := &recordingRun{version: "0.31.1"}
			fn := engineVersionOnHost(goos, stateDir, agentconfig.InferenceConfig{}, rec.run)

			if installed, ver := fn(context.Background(), catalog.RuntimeVLLM); installed || ver != "" {
				t.Errorf("engineVersionOnHost(%q)(vllm) = (%v, %q) with no venv, want (false, \"\")",
					goos, installed, ver)
			}
			if installed, ver := fn(context.Background(), "tensorrt"); installed || ver != "" {
				t.Errorf("engineVersionOnHost(%q)(tensorrt) = (%v, %q), want (false, \"\")",
					goos, installed, ver)
			}
			if rec.calls != 0 {
				t.Errorf("version runner called %d times for vllm / unknown, want 0", rec.calls)
			}
		})
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
