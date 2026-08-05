package runtime

// OllamaPinnedVersion is the Ollama release waired bundles. Maintenance
// constant: bump when validating a newer upstream.
//
// BUMPING IT IS ONE LINE. There is no checksum to recompute here, unlike
// uv (UVPinnedSHA256Linux64, and scripts/dev/update-uv-sha.sh to refresh
// it): the installer fetches the release's OWN sha256sum.txt and verifies
// every asset it downloads against that. Ollama releases often, and
// chasing each one with three hardcoded hashes is the maintenance this
// deliberately avoids — the trade is that a compromised release signs its
// own checksums, which HTTPS to github.com already had to be trusted for.
//
// Two things a bump DOES have to check, because the installer treats both
// as fatal rather than degrading around them:
//
//   - the release publishes sha256sum.txt, and it lists every asset in
//     ollamaReleaseFor's table for the OSes we ship (three archives plus
//     the two ROCm overlays);
//   - the asset NAMES have not changed. 0.30.x renamed the Linux archive
//     from .tgz to .tar.zst, which is the kind of change that turns an
//     install into a 404 on one OS only.
//
// The AMD/ROCm supported-SKU list is the other thing to revisit, and it
// now lives in exactly one place — amdROCmSupported in ollama_backend.go.
// It used to be mirrored in scripts/install/ollama-windows.ps1, which
// #493 retired along with the second copy.
//
// 0.30.x changed the Linux release asset format from .tgz to .tar.zst
// (ollama_install.go decompresses in-process) and reworked the
// llama.cpp backend — validated on an RTX PRO 4000 Blackwell:
// qwen3.6-27b q4 19.7 → 31.9 tok/s vs 0.24.0, and the qwen3.6 -mtp-
// (multi-token prediction) tags become pullable (54.9 tok/s).
// renovate: datasource=github-releases depName=ollama/ollama
const OllamaPinnedVersion = "0.31.1"
