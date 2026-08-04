package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// One rule for "is this engine installed on this host".
//
// Every place that answers that question must go through
// engineInstalledOnHost. The reason it exists as a single named
// predicate rather than three convenient inline probes is that the
// convenient probe — exec.LookPath — is WRONG here, and has been wrong
// four separate times in this repo (#67's nvidia-smi detection, the
// deploy-path one, #179's setup state, and #238's engine-version probe
// one layer below it). The engine waired installs
// for itself lives under the state dir and is deliberately NOT on
// $PATH; a LocalSystem service on Windows does not inherit a user PATH
// at all. A PATH-only probe therefore reports "no engine" on exactly
// the hosts waired set up itself, and the daemon — which resolves the
// binary by stat — disagrees with it at the same instant on the same
// machine.

// bundledOllamaBinPath is where waired's own ollama install lands,
// relative to the agent state dir. The executor is told the state dir
// (SetupStateResponse.StateDir) and joins the same path, so the two
// agree by construction rather than by coincidence.
//
// No ".exe" on Windows on purpose: the Windows / macOS "bundled"
// installs land at global well-known locations outside the state dir,
// which download.ResolveBinary covers instead. Only Linux installs
// here (cmd/waired/runtimes_install_linux.go).
func bundledOllamaBinPath(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", "ollama", "bin", "ollama")
}

// resolveOllamaBinary is the daemon's rule for locating ollama: the
// waired-managed binary under the state dir first, then
// download.ResolveBinary's $WAIRED_OLLAMA_BINARY → $PATH → well-known
// paths walk.
//
// Linux is STRICT: only the waired-managed binary qualifies. Falling
// back to PATH used to spawn whatever system ollama was installed
// (unpinned version) on our port. Windows/macOS keep the fallback
// because their waired-managed installs still live outside the state
// dir (#492, #493 relocate them; #494 then removes the fallback).
//
// goos is a parameter, not runtime.GOOS, so the Linux-strict branch is
// table-testable from any runner (repo rule: route GOOS-varying
// decisions through a function taking runtime.GOOS).
func resolveOllamaBinary(goos, stateDir string) (string, error) {
	bundled := bundledOllamaBinPath(stateDir)
	if fi, err := os.Stat(bundled); err == nil && fi.Mode().IsRegular() {
		return bundled, nil
	}
	if goos == "linux" {
		return "", fmt.Errorf(
			"bundled ollama not installed (expected at %s): run `sudo waired runtimes install ollama`",
			bundled)
	}
	return download.ResolveBinary("")
}

// vllmActiveVersion returns the version recorded by the verified vLLM
// venv under <state-dir>/runtimes/vllm — the path the installer writes
// (#525). A $HOME-relative default would diverge from a sudo-run
// install (root's home is not the User=waired daemon's home). The
// Windows/darwin installer stubs always answer "no active install",
// which is what makes vLLM Linux-only without a build tag here.
//
// Presence and version come from this one call, so they cannot disagree
// the way a separate PATH probe did (#238).
func vllmActiveVersion(stateDir string) (string, bool) {
	active, ok := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm")).Active()
	if !ok {
		return "", false
	}
	return active.Version, true
}

// vllmVenvActive reports whether that venv exists at all.
func vllmVenvActive(stateDir string) bool {
	_, ok := vllmActiveVersion(stateDir)
	return ok
}

// engineInstalledOnHost answers "is this engine installed here" the way
// the daemon itself would. Unknown engine kinds are not installed.
//
// It deliberately does NOT consult hardware.Profile.Engines. That field
// now resolves through this same rule (engineVersionOnHost is injected
// into the daemon's profiler, #238), but it is cached for 30 s, so it
// is still LATE for a fresh install — which is exactly what the wizard
// could not tolerate (#179). The profile keeps it for what it is
// genuinely good at: reporting an engine's VERSION, which needs the
// binary executed either way.
func engineInstalledOnHost(goos, stateDir, engine string) bool {
	switch engine {
	case catalog.RuntimeOllama:
		_, err := resolveOllamaBinary(goos, stateDir)
		return err == nil
	case catalog.RuntimeVLLM:
		return vllmVenvActive(stateDir)
	default:
		return false
	}
}

// engineVersionOnHost answers "which version of this engine is
// installed here" through the SAME resolution as engineInstalledOnHost,
// shaped for hardware.WithEngineVersion so the profiler's
// Profile.Engines stops being a second opinion.
//
// The profiler's own probe was PATH-only: it named the engine and asked
// $PATH (#238). That is the predicate #179 removed from the wizard
// path, still live one layer down — a bundled engine under the state
// dir reported no version at all, and an unknown version makes the
// catalog's MinEngineVersion floors fail closed, so variant selection
// silently narrowed on exactly the hosts waired provisioned.
//
// run is a parameter rather than a direct call so the resolution can be
// table-tested across all three GOOS values without executing anything;
// production passes hardware.EngineVersionAt.
func engineVersionOnHost(
	goos, stateDir string,
	run func(ctx context.Context, engine, path string) (bool, string),
) func(context.Context, string) (bool, string) {
	return func(ctx context.Context, engine string) (bool, string) {
		switch engine {
		case catalog.RuntimeOllama:
			bin, err := resolveOllamaBinary(goos, stateDir)
			if err != nil {
				return false, ""
			}
			return run(ctx, engine, bin)
		case catalog.RuntimeVLLM:
			// Nothing to execute: the installer already verified the
			// venv and recorded what it holds.
			if v, ok := vllmActiveVersion(stateDir); ok {
				return true, v
			}
			return false, ""
		default:
			return false, ""
		}
	}
}
