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
//
// 0.31.1 → 0.32.13 is not a routine refresh: it is what makes
// qwen3.8-27b reachable at all (#823). On 0.31.1 the registry refuses
// the tag outright — `ollama pull qwen3.8:27b-mtp-q4_K_M` answers
// `412: The model you are attempting to pull requires a newer version of
// Ollama` — so the catalog entry would sit there unpullable on every
// host waired installs for. Upstream added the model in v0.32.12
// ("Qwen 3.8 27B", 2026-08-14) and fixed its handling of developer
// instructions in v0.32.13, which is the message a coding agent's system
// prompt becomes on the way through.
//
// 0.32.13 → 0.32.15 closes the window that pin landed inside. In 0.32.12
// and 0.32.13 the qwen3.8 renderer rejects any system turn that is not
// first — measured on an RTX PRO 4000 Blackwell, 2026-08-27: a
// `[user, system]` body answers HTTP 500 "system message must be at the
// beginning", and Claude Code sends exactly that under its
// mid-conversation-system beta, so every real turn failed
// (waired-agent#1035, ollama/ollama#17754). Upstream tolerated the
// non-leading turn in v0.32.14 (#17757) and merged all instruction turns
// into one leading turn in v0.32.15 (#17855) — the later of the two,
// because that is the one whose rendering the gateway's own
// normalization now matches.
//
// The qwen3.8 variant's min_engine_version deliberately does NOT move
// with this. A floor answers "what does this MODEL need", and qwen3.8
// needs 0.32.13 (registry availability plus developer-instruction
// handling, decision 20260816/2024). The renderer bug is engine-wide,
// not model-specific, and raising the floor would dark the whole family
// on every not-yet-converged host over a defect we fixed.
// renovate: datasource=github-releases depName=ollama/ollama
const OllamaPinnedVersion = "0.32.15"
