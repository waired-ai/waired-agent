package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
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
// three separate times in this repo (#67's nvidia-smi detection, the
// deploy-path one, and #179's setup state). The engine waired installs
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
// Bundled mode on Linux is STRICT: only the waired-managed binary
// qualifies. Falling back to PATH used to spawn whatever system ollama
// was installed (unpinned version) on our port. Windows/macOS keep the
// fallback because their "bundled" installs live outside the state dir;
// reuse mode keeps it too, since there the binary is only ever used as
// a pull client, never spawned.
//
// goos is a parameter, not runtime.GOOS, so the Linux-strict branch is
// table-testable from any runner (repo rule: route GOOS-varying
// decisions through a function taking runtime.GOOS).
func resolveOllamaBinary(goos, stateDir string, borrowed bool) (string, error) {
	bundled := bundledOllamaBinPath(stateDir)
	if fi, err := os.Stat(bundled); err == nil && fi.Mode().IsRegular() {
		return bundled, nil
	}
	if !borrowed && goos == "linux" {
		return "", fmt.Errorf(
			"bundled ollama not installed (expected at %s): run `sudo waired runtimes install ollama`, "+
				"or switch ollama_source to \"reuse\" in agent.json / re-run `sudo waired init`",
			bundled)
	}
	return download.ResolveBinary("")
}

// vllmVenvActive reports whether a verified vLLM venv exists under
// <state-dir>/runtimes/vllm — the path the installer writes (#525). A
// $HOME-relative default would diverge from a sudo-run install (root's
// home is not the User=waired daemon's home). The Windows/darwin
// installer stubs always answer "no active install", which is what
// makes vLLM Linux-only without a build tag here.
func vllmVenvActive(stateDir string) bool {
	_, ok := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm")).Active()
	return ok
}

// engineInstalledOnHost answers "is this engine installed here" the way
// the daemon itself would. Unknown engine kinds are not installed.
//
// It deliberately does NOT consult hardware.Profile.Engines: that field
// comes from a PATH probe (internal/hardware/profiler.go
// defaultEngineVersion) and is cached for 30 s, so it is both wrong for
// state-dir installs and late for fresh ones. The profile keeps it for
// what it is genuinely good at — reporting an engine's VERSION, which
// needs the binary executed either way.
func engineInstalledOnHost(goos, stateDir string, cfg agentconfig.InferenceConfig, engine string) bool {
	switch engine {
	case catalog.RuntimeOllama:
		_, err := resolveOllamaBinary(goos, stateDir, cfg.OllamaSource == agentconfig.OllamaSourceReuse)
		return err == nil
	case catalog.RuntimeVLLM:
		return vllmVenvActive(stateDir)
	default:
		return false
	}
}
