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

// resolveOllamaBinary is the daemon's rule for locating ollama: the
// waired-managed binary under the state dir, or nothing.
//
// Linux and macOS are STRICT. Falling back to PATH used to spawn
// whatever system ollama happened to be installed — an unpinned version,
// software waired never tested against — on our port. macOS joined the
// strict side with #492, which moved its bundled install off
// /Applications and under the state dir; until then the fallback was
// load-bearing, because the waired-managed install genuinely lived
// outside the state dir there.
//
// Windows still keeps the fallback for one more change (#493 relocates
// its install; the fallback goes with it), which is why this takes a
// goos at all.
//
// goos is a parameter, not runtime.GOOS, so the branch is table-testable
// from any runner (repo rule: route GOOS-varying decisions through a
// function taking runtime.GOOS).
func resolveOllamaBinary(goos, stateDir string) (string, error) {
	bundled := infruntime.BundledOllamaBinaryPath(goos, stateDir)
	if fi, err := os.Stat(bundled); err == nil && fi.Mode().IsRegular() {
		return bundled, nil
	}
	if goos != "windows" {
		return "", fmt.Errorf(
			"bundled ollama not installed (expected at %s): run `%s`",
			bundled, elevatedInstallHint(goos))
	}
	return download.ResolveBinary("")
}

// elevatedInstallHint spells the install command the way the operator has
// to invoke it. The state dir is root-owned on both strict OSes, so both
// need sudo — but only after #492 on macOS, where the install used to go
// to the admin-writable /Applications and needed none.
func elevatedInstallHint(goos string) string {
	if goos == "windows" {
		return "waired runtimes install ollama"
	}
	return "sudo waired runtimes install ollama"
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
