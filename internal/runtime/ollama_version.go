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
// on every not-yet-converged host over a defect we fixed. That rule
// held for 0.33.2 as well — nothing below moves a per-variant floor.
//
// 0.32.15 -> 0.33.2 is taken for the caching work in 0.33.0, and the
// list below is what was re-measured rather than assumed
// (waired-agent#1132). Measured 2026-08-29 on three hosts: sv-mag
// (Linux, RTX PRO 4000 Blackwell), sv-evox2 (Windows), sv-macmini
// (macOS, M4).
//
// What 0.33.0 buys. Upstream disabled Claude Code's "tokens left"
// countdown system message, which ollama had been moving to the front
// of the prompt — breaking the KV cache on EVERY request — and reworked
// prefill restore points so a cancelled prefill keeps the points it
// crossed. Both land on this product's central path: a coding agent's
// second turn is supposed to be a prefix hit, and #1125 / #1127 reason
// on how far that reuse extends.
//
// What was checked because it could have broken silently:
//
//   - Asset names and the release's own sha256sum.txt still cover every
//     entry in ollamaReleaseFor's table. Verified per OS by downloading
//     and checksumming: linux .tar.zst, darwin .tgz, windows .zip.
//   - The archive LAYOUTS still match what ExtractSub assumes. darwin is
//     still flat (ollama, llama-server, *.dylib) plus mlx_metal_v3 /
//     mlx_metal_v4; windows still carries ollama.exe at the archive
//     root; linux still unpacks bin/ + lib/. A layout change here is the
//     404-on-one-OS class this comment exists for.
//   - A system turn that is not first is still accepted — HTTP 200 on
//     both /v1/chat/completions and /api/chat. That is the #1035
//     regression 0.32.14/0.32.15 fixed, and it stays fixed.
//   - keep_alive is still DISCARDED on the OpenAI-compatible surface and
//     still honoured on the native one. Measured against /api/ps:
//     keep_alive=37m via /v1 left the default expiry, keep_alive=41m via
//     /api/chat moved it. ResidencyEffect (#908) rests on exactly that
//     asymmetry.
//   - /api/ps still reports context_length / size_vram / expires_at, and
//     engine.log is still logfmt with a msg="..." field, so the verify
//     pass's read-backs (inference_ollama_verify.go) still parse.
//
// One thing came out narrower than the tree assumed, and it is recorded
// rather than fixed here. The gateway folds every instruction turn into
// one leading system turn joined by "\n\n", and convert.go said that
// renders the same prompt the fixed engine renders. Measured by
// comparing prompt_eval_count for the raw shape against the folded one:
//
//	qwen3.8-27b  one non-leading system turn   44 vs 44   agrees
//	qwen3.8-27b  two instruction turns         55 vs 55   agrees
//	qwen3.5:0.8b one non-leading system turn   44 vs 44   agrees
//	qwen3.5:0.8b two instruction turns         59 vs 55   differs
//
// So the agreement is a property of the MODEL'S CHAT TEMPLATE, not of
// the engine release: qwen3.5:0.8b diverged identically on Linux and on
// macOS, which rules out the OS, and qwen3.8-27b — the model #1035 was
// about — agrees on both counts. Nothing is broken by the difference,
// because the gateway normalizes before the engine ever sees the raw
// shape and both sides of a mesh request therefore count the same folded
// form. The claim in convert.go was simply wider than any measurement
// supports, and has been narrowed there.
// renovate: datasource=github-releases depName=ollama/ollama
const OllamaPinnedVersion = "0.33.2"
