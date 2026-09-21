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
// A third a bump has to check does not break the install; it makes the
// catalog describe an engine nobody installs. The scheduler starts some
// model families with one request slot whatever OLLAMA_NUM_PARALLEL asks
// (server/sched.go Scheduler.load), and the catalog holds those builds to
// max_parallel 1 (waired-ai/waired-agent#1423). The catalog-sources
// workflow runs on this file and re-reads the list from the new tag
// (internal/catalog/max_parallel_integration_test.go); it is not a
// required check, so look at it.
//
// There is no AMD/ROCm supported-SKU list to revisit any more: waired
// fetches the ROCm overlay for any AMD GPU in use and lets the engine
// decide which devices it serves (WantsROCmOverlay, waired-agent#1492).
// What remains to re-read at a bump is the Windows Strix Halo arm's
// upstream threads, listed beside that arm in ollama_backend.go. (The
// dated notes below still name amdROCmSupportedRes; they record what was
// checked at the time.)
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
// held for 0.33.2 and for 0.33.3 — nothing below moves a per-variant
// floor.
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
//   - The raw-vs-folded prompt_eval_count relationship is unchanged, and
//     still a property of the model's chat template rather than of the
//     engine. On /api/chat, one non-leading system turn against the same
//     turns folded into a leading one: qwen3.8-27b 19 vs 19 and, for two
//     instruction turns, 24 vs 24 — agrees on both counts, as at 0.33.2.
//     qwen3.5:0.8b-q8_0 agrees on the first (19 vs 19) and differs on the
//     second (28 vs 24), also as at 0.33.2. Measured identically on macOS
//     and Linux, which is what rules the OS out.
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
//
// 0.33.2 -> 0.33.3 is taken for the ARCHITECTURE, not for the caching.
// The vendored llama.cpp moves b10630 -> b10760, and qwen4exp landed at
// b10666 (ggml-org/llama.cpp#27742), so this is the first release whose
// llama.cpp runner can load the Qwen3.8-Flash-Next family at all. That
// is the floor waired-agent#1192's catalog entry declares, and
// TestBundledEngineFloorsNeverExceedThePin is why the entry cannot land
// until this constant has moved. Measured 2026-09-06 on three OSes:
// pc-mbp14-m5 (macOS, M5 Pro), sv-evox2 (Windows, Strix Halo), and —
// sv-mag being held for the whole window — the WSL2 development machine
// for the Linux leg, which is a different card from the RTX PRO 4000
// the 0.33.2 entry used.
//
// Two release-note items land on this product's own path, and both were
// measured rather than taken on trust.
//
// "Report cached prompt tokens" is the one that changes what this
// product sees. Until 0.33.2 ollama reported no cache breakdown on
// either surface; 0.33.3 reports one on both, with no flag to ask for
// it. A second identical request on qwen3.5:0.8b-q8_0 answered
// prompt_tokens 610 with prompt_tokens_details.cached_tokens 606 where
// the first answered 0, and /api/chat carried the same figure as
// prompt_eval_cached_count. So OpenAIUsage.CachedPromptTokens starts
// returning real reuse on the ollama path instead of a constant zero,
// and convert.go's claim that ollama has no equivalent is narrowed
// there. One caveat worth carrying: appending to a prompt that had just
// hit reported cached_tokens 0 again rather than a shorter prefix, so
// this field is not yet a measure of reuse DEPTH (#1125 / #1127).
//
// "Honor GGUF model defined default parameters" did not move any window
// this product asks for, because the agent always exports
// OLLAMA_CONTEXT_LENGTH and the engine's own default never governs a
// window waired named. It did make that default host-dependent: the
// engine now logs "vram-based default context" at startup and derives
// it from the VRAM it found — 32768 on the 37.4 GiB Mac, 262144 on the
// 102.2 GiB Strix Halo. ollamaContextFloor's doc is narrowed to say so.
//
// What was checked because it could have broken silently:
//
//   - Asset names and the release's own sha256sum.txt still cover every
//     entry in ollamaReleaseFor's table
//     (TestPinnedReleasePublishesEveryAssetChecksum against the real
//     release). Archive LAYOUTS still match what ExtractSub assumes,
//     verified by unpacking all three: darwin is still flat (ollama,
//     llama-server, llama-quantize, *.dylib) plus mlx_metal_v3 /
//     mlx_metal_v4; windows still carries ollama.exe at the archive root
//     beside lib/, and its lib/ollama/ still holds cuda_v12, cuda_v13
//     and vulkan with no rocm — the Windows base package still ships
//     none, which is the premise amdROCmSupported rests on; linux
//     still unpacks bin/ + lib/.
//   - A system turn that is not first is still accepted — HTTP 200 on
//     both /v1/chat/completions and /api/chat. #1035 stays fixed.
//   - keep_alive is still DISCARDED on the OpenAI-compatible surface and
//     still honoured on the native one. Measured against /api/ps:
//     keep_alive=37m via /v1 left the default ~5-minute expiry in place,
//     41m via /api/chat moved expires_at to now+41m. ResidencyEffect
//     (#908) rests on exactly that asymmetry.
//   - /api/ps still reports context_length / size / size_vram /
//     expires_at (plus details / digest / model / name), and engine.log
//     is still logfmt with a msg="..." field, so the verify pass's
//     read-backs (inference_ollama_verify.go) still parse.
//   - The runner invocation still carries -np, so ObservedNumParallel's
//     read-back (#763) holds, and the engine still sizes its own prompt
//     batch: -c 32768 -np 1 ... -b 1024 -ub 1024.
//   - The Windows ROCm overlay is now rocm_v7_1 (it was v6.1 when
//     amdROCmSupportedRes was last stamped) and its rocBLAS kernels name
//     more gfx targets than that list knows. Nothing here changed —
//     both gaps end on Vulkan, which works — and #1233 carries the
//     measurement that would settle which set the agent should follow.
//   - The bench cache is keyed on EngineVersion (#1131), so every host
//     misses once after this bump and re-measures. That is the correct
//     result rather than a cost: the stored number was the old engine's.
//
// 0.33.3 -> 0.34.0 is a refresh with nothing on this product's path. No
// release sits between the two, and the vendored llama.cpp stays at
// b10760, so every term hostfit measured on 0.33.3 still describes the
// engine. The release adds OpenAI-surface features (response compaction,
// client tool search, a Codex proxy route) and touches server/sched.go
// only to fix a data race in a log line. Re-checked 2026-09-16 rather
// than taken from the notes:
//
//   - All four (goos, goarch) assets and sha256sum.txt
//     (TestPinnedReleasePublishesEveryAssetChecksum), and the archive
//     LAYOUTS: the darwin and windows archives list the same 59 and 87
//     entries as 0.33.3's, and the linux one still unpacks bin/ + lib/
//     (TestOllamaInstaller_RealArchive unpacks and runs it).
//   - On Linux (an RTX 5070 Laptop GPU, qwen3.5:0.8b-q8_0): a system turn
//     that is not first is HTTP 200 on /v1 and /api/chat; keep_alive=37m
//     via /v1 still leaves the default 5-minute expiry while 41m via
//     /api/chat moves it to 41; /api/ps returns the same eight keys;
//     cached_tokens is reported on both surfaces (918 on a resend);
//     engine.log is still logfmt with msg="..."; the runner still gets
//     -c / -np 1 / -b 512 -ub 512 and --cache-type-k/-v from
//     OLLAMA_KV_CACHE_TYPE.
//   - The same checks on Windows (a Strix Halo, Radeon 8060S on Vulkan
//     with OLLAMA_VULKAN=1) answered the same: 200 on both surfaces, 5 and
//     41 minutes, the same /api/ps keys, cached_tokens 918, and the
//     runner started with -c 262144 -np 1 from the 102.2 GiB VRAM tier.
//     macOS was checked by archive layout only.
//   - ParseLlamaPlacement reads a 0.34.0 load log to the same fields it
//     reads from 0.33.3's (offloaded layers, model buffers, n_ctx, KV
//     type, the fit projection), which is expected with llama.cpp
//     unchanged but is what agent#1384's placement read-back rests on.
//   - The renderers the catalog stamps, qwen3.5 and qwen3.8, are still
//     registered: stamped onto a tag, the non-first system turn answers
//     200, while an unregistered renderer name answers 500.
//
// 0.34.0 -> 0.34.2 is the first move since 0.33.3 that carries the
// vendored llama.cpp with it: b10760 -> b10969, 209 commits and 177 files
// under src/, common/ and tools/. So this one is measurement rather than a
// refresh, and what follows was taken rather than reasoned.
//
// Static first, because these are the ways the product breaks without
// erroring, and all of them held:
//
//   - Asset names and sha256sum.txt still cover all four (goos, goarch)
//     pairs (TestPinnedReleasePublishesEveryAssetChecksum against the real
//     release), and the LAYOUTS still match ExtractSub: linux unpacks
//     bin/ + lib/ (TestOllamaInstaller_RealArchive unpacks and runs it),
//     the windows zip is 87 entries with ollama.exe alone at the root and
//     lib/ollama/{cuda_v12, cuda_v13, vulkan} and still no rocm, and the
//     darwin tgz is flat plus mlx_metal_v3 / mlx_metal_v4.
//     A CORRECTION while re-reading that last one: the stamp above says
//     darwin lists 59 entries. It lists 57, and did at 0.33.3 and 0.34.0
//     too — those two archives have byte-identical file lists, so the 59
//     was a miscount rather than something that changed. 0.34.0 -> 0.34.2
//     differs only in the vendored sonames (libggml 0.22.0 -> 0.24.0,
//     libllama / libllama-common / libmtmd 0.3.0 -> 0.4.1), which is what
//     a llama.cpp move looks like from outside.
//   - The runner argv flag set in llm/llama_server.go is BYTE-IDENTICAL to
//     0.34.0's, so nothing ObservedNumParallel or ParseRunnerFlags reads
//     could have moved.
//   - api.ProcessModelResponse is unchanged, so /api/ps still carries the
//     same eight keys.
//   - The one-slot model-family list in server/sched.go is unchanged (only
//     its line numbers moved); TestOllamaSingleRequestFamiliesMatchThePin
//     re-parses the 0.34.2 source and agrees.
//   - The three llama.cpp lines ParseLlamaPlacement depends on still exist
//     verbatim at b10969: src/llama-model.cpp's "offloaded %d/%d layers to
//     GPU", src/llama-kv-cache.cpp's "size = ... K (%s) ... V (%s)", and
//     common/fit.cpp's "projected to use". common/fit.cpp changed
//     additively only (a JSONL fit_memory_breakdown log), so the margin
//     logic OllamaFitTargetMB describes is untouched.
//   - "ollama version is %s" is unchanged, so ParseEngineVersion is safe.
//
// Two changes in 0.34.1/0.34.2 that look dangerous and are not:
//
//   - typical_p is now rejected with HTTP 400 on requests AND on create
//     (server/routes.go, server/create.go). The product never sends it —
//     grep finds none, and a deliberate request carrying it answered 400
//     on all three OSes, which makes that a measurement rather than an
//     absence argument.
//   - 0.34.2 adds a first-run welcome, but runWelcome (cmd/welcome.go)
//     returns nil unless BOTH stdin and stdout are terminals, and it is
//     wired only to the bare `ollama` root command — not serve, create or
//     pull. A spawned engine never reaches it.
//   - /api/show changed shape (tensors only when verbose, uppercased type
//     strings, long arrays no longer flattened when not verbose). No Go in
//     this product calls /api/show.
//
// Re-measured on all three OSes this time, which closes the coverage gap
// 0.34.0 left when it checked macOS by archive layout only. One engine
// start each, qwen3.5:0.8b-q8_0, OLLAMA_CONTEXT_LENGTH=32768, on a 24 GB
// CUDA dGPU, a 128 GiB unified-memory Vulkan iGPU, and a 16 GiB Metal Mac:
//
//   - A system turn that is not first is HTTP 200 on both
//     /v1/chat/completions and /api/chat, everywhere. #1035 stays fixed.
//   - keep_alive is still DISCARDED on the OpenAI-compatible surface and
//     HONOURED on the native one, which is what ResidencyEffect (#908)
//     rests on. 37m via /v1 left the default ~5-minute expiry on all
//     three; 41m via /api/chat moved expires_at to now+41m on all three.
//   - /api/ps returns the same eight keys, and engine.log is still logfmt
//     with a msg="..." field, so inference_ollama_verify.go still parses.
//     The engine's own lines still carry the time= prefix that
//     lastRunnerLine uses to tell them from the runner's.
//   - The runner still gets -np, and still sizes its own prompt batch:
//     -c 32768 -np 1 with -b 512 -ub 512 on CUDA and -b 1024 -ub 1024 on
//     Vulkan and Metal.
//   - The VRAM tiering behind "vram-based default context" still holds at
//     all three rungs, which is the first time it has been read at the
//     bottom one: 11.8 GiB -> 4096, 23.5 GiB -> 32768, 102.2 GiB ->
//     262144.
//   - cached_tokens is reported on both surfaces (18 of 22 on a resend).
//   - ParseLlamaPlacement was run over a real 0.34.2 log rather than
//     reasoned about, and filled every field: ContextCells, offloaded and
//     total layers, device and host weight buffers, the KV type, and all
//     three fit terms. All eight of its regexes bind.
//
// The QSA-indexer item the previous stamp scheduled for "the next bump" is
// this bump, and it is now SETTLED. ggml-org/llama.cpp#28330 (311d4211b,
// first in b10889) is an ancestor of b10969, and serving
// qwen3.8-flash-next on it at 8192 cells gives:
//
//	llama_kv_cache: size = 192.00 MiB (8192 cells, 12 layers, 1/1 seqs), K (f16): 96.00 MiB, V (f16): 96.00 MiB
//	llama_memory_recurrent: size = 112.57 MiB (1 cells, 48 layers, 1 seqs 0 rs_seq)
//	llama_kv_cache: size =  24.00 MiB (8192 cells, 12 layers, 1/1 seqs), K (f16): 24.00 MiB, V (f16):  0.00 MiB
//
// The indexer cache's V half is gone — 0.00 MiB, and the engine now prints
// n_embd_head_k_all = 0 for it. Dividing back from (cells, layers) so the
// figures do not depend on the window: attention 24,576 B/token, indexer
// key 3,072 B/token, total 27,648 — which is exactly what the catalog
// annotates for this variant. At b10760 the indexer held K 3,072 plus a V
// half of 6,144 that the model has no projection for, and the measurement
// came to 33,792.
//
// So the annotation was right to carry the derivable number rather than
// the measured one: it needed no change when the engine was wrong, and it
// needs none now that the engine agrees. The scheduled item is discharged,
// and docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md
// §4 is struck in this PR.
//
// AT EVERY BUMP, re-read which integrated GPUs the engine uses by default:
// discover/runner.go integratedGPUAllowedByDefault and
// defaultIntegratedROCmGFXTargets. At 0.34.2 that is CUDA devices and
// ROCm gfx1151, nothing on Vulkan. internal/hardware/engine_gpus.go copies
// that rule so the host is described by the GPUs the engine will actually
// run on (waired-agent#1484); if upstream admits another gfx target, add
// it to ollamaDefaultIntegratedROCmGFXTargets there, and if it starts
// admitting Vulkan iGPUs, the copy has to change shape. The engine's own
// start-up log is now checked against that copy (waired-agent#1513): a
// "differs from waired's prediction" WARN on a host after a bump points at
// this rule.
//
// AT EVERY BUMP, also re-read what ParseInferenceCompute
// (ollama_discovery_log.go) reads: server/routes.go Serve's "Listening on"
// and "vram-based default context" lines and their order around the
// device block, discover/types.go LogDetails's "inference compute" fields,
// and discover/runner.go's "dropping integrated GPU" message. A change
// there makes the check report "not read" rather than compare.
//
// renovate: datasource=github-releases depName=ollama/ollama
const OllamaPinnedVersion = "0.34.2"
