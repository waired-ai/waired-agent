package runtime

import (
	"fmt"
	"path/filepath"
)

// Where waired's own Ollama lives, and which upstream release assets feed
// it. Both answers are pure functions of (goos, goarch), so the whole
// table is testable from a single runner — the same split
// internal/download already uses between ollamaCandidates (takes a goos)
// and ollamaCmdName (build-tagged): this file answers "what does an
// install on THAT OS look like", and the ollama_extract_*.go files answer
// "how does THIS process unpack it".

// BundledOllamaDir is the engine's whole subtree under the agent state
// dir: the binary, its model store, its logs, and the HOME it is spawned
// with. Callers join from here instead of repeating the literals, because
// the installer and the daemon's resolver disagreeing about this path is
// exactly the #179 class of bug — an engine that exists on disk and that
// nothing will admit is installed.
func BundledOllamaDir(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", "ollama")
}

// BundledOllamaBinaryPath is the engine binary daemon resolution accepts.
// It is the same shape on every OS since #492/#493 moved macOS and Windows
// off their global well-known locations (/Applications/Ollama.app,
// %ProgramFiles%\Ollama).
//
// goos is a parameter rather than runtime.GOOS so all three answers are
// table-testable from one runner (repo rule: route GOOS-varying decisions
// through a function taking runtime.GOOS).
func BundledOllamaBinaryPath(goos, stateDir string) string {
	return filepath.Join(BundledOllamaDir(stateDir), "bin", OllamaBinaryName(goos))
}

// OllamaBinaryName is the engine executable's file name on goos.
func OllamaBinaryName(goos string) string {
	if goos == "windows" {
		return "ollama.exe"
	}
	return "ollama"
}

// ollamaRelease names the upstream assets for one host and says where the
// archive's payload has to land.
type ollamaRelease struct {
	// Base is the release asset carrying the engine.
	Base string
	// ROCm is the AMD overlay asset, or "" on an OS upstream ships none
	// for. macOS has none because Metal and MLX are inside the binary.
	ROCm string
	// ExtractSub is the sub-directory of the install's base dir that the
	// archive unpacks into; "" means the base dir itself.
	//
	// It exists because the three archives are laid out differently, and
	// the binary still has to end up at one path on every OS:
	//
	//	linux    bin/ollama + lib/ollama/…   -> unpack into BaseDir
	//	darwin   ollama, llama-server, *.dylib, mlx_metal_v*/  (flat)
	//	windows  ollama.exe + lib/ollama/…   (payload at the root)
	//
	// Unpacking the last two into BaseDir/bin puts their binary where
	// BundledOllamaBinaryPath says it is, and — because everything in
	// those archives moves together — leaves each payload's own internal
	// layout intact. That matters: ollama locates its runners and
	// libraries relative to its own executable.
	ExtractSub string
}

// ollamaReleaseFor resolves the release assets for a host.
//
// Arch handling differs per OS and follows what upstream actually
// publishes: Linux ships one asset per architecture, macOS ships a single
// universal binary covering both slices, and Windows ships an amd64 build
// we can use plus an arm64 one we cannot (waired itself has no
// windows/arm64 target — see the Makefile's verify-cross).
func ollamaReleaseFor(goos, goarch string) (ollamaRelease, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return ollamaRelease{
				Base: "ollama-linux-amd64.tar.zst",
				ROCm: "ollama-linux-amd64-rocm.tar.zst",
			}, nil
		case "arm64":
			// No ROCm overlay: upstream publishes none for linux/arm64, and
			// what it ships instead (the jetpack variants) is NVIDIA Jetson.
			// Deriving the name from goarch, as this used to, produced
			// ollama-linux-arm64-rocm.tar.zst — an asset that has never
			// existed, fetched on any arm64 host whose GPU reports as AMD.
			return ollamaRelease{Base: "ollama-linux-arm64.tar.zst"}, nil
		default:
			return ollamaRelease{}, fmt.Errorf(
				"ollama install: unsupported GOARCH %q (linux amd64/arm64 only)", goarch)
		}
	case "darwin":
		// One asset for both slices: the Mach-O inside is universal
		// (FAT_MAGIC, x86_64 + arm64), so there is no arch token to map.
		return ollamaRelease{
			Base:       "ollama-darwin.tgz",
			ExtractSub: "bin",
		}, nil
	case "windows":
		if goarch != "amd64" {
			return ollamaRelease{}, fmt.Errorf(
				"ollama install: unsupported GOARCH %q (windows amd64 only)", goarch)
		}
		return ollamaRelease{
			Base:       "ollama-windows-amd64.zip",
			ROCm:       "ollama-windows-amd64-rocm.zip",
			ExtractSub: "bin",
		}, nil
	}
	return ollamaRelease{}, fmt.Errorf("ollama install: unsupported GOOS %q", goos)
}
