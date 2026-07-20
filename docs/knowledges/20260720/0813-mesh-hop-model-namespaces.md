# メッシュ hop をまたぐモデル名は 2 つの名前空間を行き来する (20260720 08:13)

## Issue

ピア経由の推論が提供側で必ず `404 model_not_found` になっていた (#107)。
原因は単純で、**hop の両側で「モデル名」の意味が違う**のに、提供側が片方の
名前空間しか解決できなかったこと。

- **カタログ名前空間**: `Manifest.ModelID` と `Manifest.ModelAliases`
  (`waired/default`, `qwen3-coder-30b-a3b`, ...)。`catalog.LookupByAlias` が扱う。
- **エンジン名前空間**: `Variant.Source.Tag`(ollama) と `Variant.Source.RepoID`(vLLM)
  (`qwen3.5:9b-q4_K_M`, `Qwen/Qwen3.5-9B-AWQ`)。エンジンが `/api/tags` や
  `/v1/models` で名乗る名前で、`InferenceState.Models` に載って mesh を流れる。

消費側は `buildMeshCandidates` でピアの広告を**エンジン名前空間**で照合し、
`Selection.EngineModel` にその名前を入れ、gateway が body の `model` をそれに
書き換えて送る。提供側はそれを `resolveModel` に渡すが、そこは
`LookupByAlias` = **カタログ名前空間**しか見ていなかった。

`proto/catalog/bundled/*.json` を全数検査すると、ollama タグが `model_aliases` に
含まれる manifest は **0/21**。vLLM の repo_id は 5/21 だけ一致するが、実運用で
使う AWQ/FP8/NVFP4 variant では一致しない。つまり ollama ピアへの hop は 100% 落ちていた。

## Learnings

- **2 ノードのテストはすべて提供側を stub にしていた**ため、このバグは
  ユニットテストでも CI でも一切検出されなかった:
  - `internal/runtime/peer/integration_test.go` — ピア B は `peerBEcho`
    (素の `http.Handler`)。`gateway.HandlerSet` も `router.Selector` も manifests も通らない。
  - `internal/gateway/phase8_integration_test.go` / `phase9_integration_test.go` —
    `ListManifests` は nil を返し、selector は fake。
  - `internal/router/*_test.go` — すべて消費側のみ。`sel.EngineModel` が
    `"qwen3:8b-q4_K_M"` になることを assert して終わっている。
  - `routing-sentinel.yml` / `installtest-inference.yml` は**単ノード**。
  - **教訓**: hop をまたぐ契約は、両端を実物で繋いだテストが 1 本ないと成立を保証できない。
    片側だけの assert は「送った値」を固定するだけで「相手が受け取れるか」を何も言っていない。
- 逆引き自体は既にツリーにあった (`router.ResolveModelForPeer`) が、消費側の
  Claude intercept の `ResolveUnknownModel` からしか呼ばれていなかった。
  「必要な部品はあるが配線されていない」型の欠陥は grep では見つからない。
- `overlayDeps` の
  「peer traffic on :9474 is OpenAI-shaped with an already-resolved EngineModel —
  exact catalog semantics are correct for it」というコメントが、まさにこの誤った前提を
  明文化していた。**コメントが断言している前提こそテストで固定する価値がある。**

## 現在の解決順序

`Selector.resolveModel`:

1. `DynamicCodingAliases` (`waired/default` 等) → `Inputs.DefaultModelID`
2. `catalog.LookupByAlias` (カタログ名前空間)
3. `lookupByEngineModel` (エンジン名前空間、#107 で追加)

alias が上位なので既存の解決結果は変わらない。3 の照合規則は `variantWantSets` と
同一 — `Source.Tag` は ollama 対応 variant のみ、`Source.RepoID` は vLLM 対応 variant のみを
指す。両名前空間を同時に探すのは、リクエストがエンジンのヒントを持たないため
(実際には ollama タグは `:`、repo id は `/` を含むので衝突しない)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/107
- `internal/router/peer_resolve.go` — `lookupByEngineModel` / `ResolveModelForPeer`
- `internal/gateway/serving_leg_test.go` — 提供側 leg の初の実物カバレッジ
