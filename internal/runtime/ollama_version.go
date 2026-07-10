package runtime

import "github.com/waired-ai/waired-agent/internal/version"

// OllamaPinnedVersion is the Ollama release waired bundles. Maintenance
// constant: bump when validating a newer upstream (track alongside the
// AMD/ROCm notes in scripts/install/ollama-windows.ps1).
//
// 0.30.x changed the Linux release asset format from .tgz to .tar.zst
// (ollama_install.go decompresses in-process) and reworked the
// llama.cpp backend — validated on an RTX PRO 4000 Blackwell:
// qwen3.6-27b q4 19.7 → 31.9 tok/s vs 0.24.0, and the qwen3.6 -mtp-
// (multi-token prediction) tags become pullable (54.9 tok/s).
// renovate: datasource=github-releases depName=ollama/ollama
const OllamaPinnedVersion = "0.31.1"

// OllamaSupportedMinVersion is the floor below which a *user-provided*
// (reuse) Ollama is flagged "unsupported" in the `waired init` prompt.
// It does not gate the bundled install (always the pinned version) and
// does not hard-block reuse — it only drives a warning. Bundled is the
// default regardless of a detected version.
const OllamaSupportedMinVersion = "0.6.0"

// OllamaVersionAtLeast reports whether version v (e.g. "0.24.0", or the
// raw "ollama version is 0.24.0" line) is >= min. Unparseable input
// returns false (treated as "not known-good").
//
// The dotted-numeric comparison was generalised into internal/version
// (shared with the installer-driven update check, #292); this remains a
// thin back-compat shim for the existing reuse-floor callers.
func OllamaVersionAtLeast(v, min string) bool {
	return version.AtLeast(v, min)
}
