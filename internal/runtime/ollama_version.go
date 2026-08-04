package runtime

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
