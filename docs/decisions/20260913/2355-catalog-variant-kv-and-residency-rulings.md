---
status: accepted
supersedes:
  - docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md
  - docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md
  - docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md
  - docs/decisions/20260809/0110-serve-at-the-rung.md
  - docs/decisions/20260907/0030-lighter-builds-come-from-one-upstream.md
---

# variant と KV キャッシュの型は選べる、既定は手書き、推奨は完全常駐 (20260913 23:55)

## Status
Accepted。オーナー裁定 2026-09-13。一次記録は private monorepo の waired-ai/waired#1357（裁定コメント 5654063492、訂正 5654089278）とその決定記録 `docs/decisions/20260913/2350-l107-catalog-rulings-variant-kv-residency.md`（番号と path のみ。リポ間の supersede は guard が解決できないので散文で指す）。実装は #1346（proto）、#1347（hostfit / modelrank）、#1348（agent）、#1349（カタログ）、コントロールプレーンと NAVI は waired-ai/waired#1387 / waired-ai/waired#1388。#1349 は waired-ai/waired#1349 と番号が衝突するので、常にリポ名付きで書く。

次の記録を**部分的に狭める（覆さない）**。各記録の `## Status` に鏡の一文を置いた。

- `docs/decisions/20260907/0030-lighter-builds-come-from-one-upstream.md`: 「同一性は model_id、同じモデル内で一段下げると提示しない」（決定 4）。出所を unsloth に揃えること、tier の手書き、`TestBundledManifests_QualityTierFollowsPrecisionWithinAModel` はそのまま有効。
- `docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md`: 「高帯の override は能動更新の独立ランナーの出典を必須」（決定 3）。ladder がキュレーションの結果であるという定義と、理由の記載の必須はそのまま有効。
- `docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md`: GPU / UMA 予算があるホストの枝が `q8_0` → `q4_0`（決定 2）。CPU-only の `f16` と「文脈長を買えるときだけ量子化 KV を要求する」規律はそのまま有効。
- `docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md`: §3 の条件 3「`OllamaDeclaresWindow`（serve tuning のサイジングがコーディングのコンテキストウィンドウに到達する）」が「完全常駐」に（決定 10）。§5 の規則 3（単調性）は serve 側で有効。
- `docs/decisions/20260809/0110-serve-at-the-rung.md`: 規則 2（上限付きスピル）は推奨の判定からは退場（決定 10）。serve 側の rung 選びでは有効。

不触: `docs/decisions/20260808/1907-price-capacity-at-the-served-window.md`（容量は提供するコンテキストウィンドウで価格付けする）、`docs/decisions/20260822/1935-measurement-returns-to-the-ranking.md`、`docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md`（速度）、`docs/decisions/20260820/0005-windows-apu-carve-out-is-not-additive.md`、`docs/decisions/20260828/1900-retire-the-forced-generation-batch.md`、`docs/decisions/20260906/1900-stamp-the-renderer-on-the-pulled-tag.md`、`docs/decisions/20260828/1930-arm-the-request-shape-gate.md`（形状ゲートは破損検知として残る）。

## Context

### 今の実装（origin/main、2026-09-13）

- 推奨は `hostfit.OllamaRecommendModel` の 3 条件: モデル自身のコンテキストウィンドウが 200k に届く、重み + オーバーヘッドが GPU のメモリに常駐する、serve tuning のサイジングが 200k に到達する（期待スピル ≤ `OllamaMaxExpectedSpillFraction` = 0.20 を許す）。
- KV キャッシュの型は variant ごとに `planOllamaKV` が決め、GPU / UMA 予算のあるホストは `q8_0` + フラッシュアテンション、CPU-only は `f16`（`20260727/1715`）。fit の価格付けは `servingKVCacheDivisor = 2`（`q8_0` 固定）で、コメントは「`q4_0` は長文の想起を壊すので意図的に提供しない」と言う。
- 自動選択は `modelrank.RankModels`: `manual_only` を除外 → variant に展開して容量で篩う → 絞り込み 3 段（native 200k / 推奨 / 実測で遅い variant）→ `quality_tier` 降順。量子化の度合いは選択に使われない。行の代表 variant は agent の `FamilyBestFit` とコントロールプレーンの `betterFit` が別々に選ぶ。
- `hostfit.Presentation` は variant の識別子も量子化も持たず、`quality_tier` の数だけで判定した variant を示す。

### 実測との食い違い（測定は waired-ai/waired#1357。コメント ID で引く）

- 推奨したビルドが 24 GB 級の GPU で 200k を張ると 66 層のうち 13 層が CPU に落ち、176k で decode 1.54 tok/s。製品はスピル 11% と予測していた。CPU に落ちた層が 0 / 5 / 13 のとき 176k の decode は 1× / 4.2× / 28×（5647759996）。`/api/ps` はこの配置を数えない（`docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`）。
- KV を `q4_0` にすると同じホストで 63/66 層、10.41 tok/s。忠実度の段差（次トークン一致 85.8%、32 トークン完全一致 20.0%）は重み Q8_0 → Q4 の段差（70.0% / 13.3%）より小さい（5648517178。Qwen で測った）。
- 重みの量子化は 4bit まではほぼ無料、3bit はエージェント形のコーパスで崩れる（Q4 → Q3 で 32 トークン完全一致 13.3% → 1.7%。5647735188）。同じメモリなら大きいモデルの量子化が勝つ（5652323001）。agentgrade は Q4 と Q2 を区別しない（5652404231）。
- 出荷中の MoE の UD-Q2 / Q3 は router が F32、attention が Q5_K〜Q8_0。dense の UD-Q2 は attention Q2_K、FFN の一部が IQ1_S。同一モデルの UD ラダー内では配合が単調（5653840138）。

## Decision

決定番号は private 側の記録と揃えてある。9・12・13 は private 側だけ（コンソールのタブ、記録の置き場、tier の順序の議論）。

1. **KV キャッシュの型は利用者が選べる。** モデル選択時にコンテキストウィンドウの選択と同じ形で、エンジンごとの選択肢（Ollama: `f16` / `q8_0` / `q4_0`、vLLM: `fp16` / `fp8`）から選ぶ。エンジン・ハードウェア・モデルで成立しない型は出さない。CPU-only は `f16` 固定（#29）。選んだ型は `desired_kv_cache_type` で serve に届く。**推奨の価格付けは、serve tuning が実際に出す KV の型と同じ型を読む**（private 側の vLLM fp8 の記録 `docs/decisions/20260705/1930-vllm-fp8-kv-ada-on-ngram-opt-in.md` が持つ「選択側は既定 ON を仮定、opt-out は serve だけ」の非対称を解消する）。
2. **KV の既定は `q4_0`。使えないハードウェア・モデルでは `q4_0` 以上で最も小さい型。** Ollama: `q4_0` → `q8_0` → `f16`、vLLM: `fp8`（Ada 以降、#676）→ `fp16`。variant ごとの許容表 `kv_cache_types` をカタログに置く。Qwen 3.5 / 3.6 / 3.8 の dense と MoE は `q4_0` を実測済み。gpt-oss / granite / flash-next は調査が済むまで `q8_0` と `f16` のみ。既定が `q4_0` になるのは #1348 で `planOllamaKV` が変わった時点で、それまで推奨が GPU ホストを `q8_0` で価格付けするのは決定 1 の不変条件どおり。`proto/hostfit/hostfit.go` の `servingKVCacheDivisor` のコメント「`q4_0` は意図的に提供しない」はこれで supersede される（測定は上の Context）。深い文脈の想起課題での確認は n=5 に留まるので、既定の見直しの引き金として残す。
3. **カタログの構成はオーナーの裁量。** agent grade やリクエスト形状の記録は参考値で、定量的な基準を満たしたモデルが自動的に入るものではない。CI の `catalog-tool agentgrade --check --require-pass` と `shapes --check --require-accepted` は破損検知（tool 呼び出しが動くか、エンジンが形状を受けるか）として残し、品質の門とは呼ばない。`20260805/1427` の高帯 override は理由の記載を必須のまま、外部出典の要件を外す。
4. **同一モデル内の variant は利用者が選べる。** `Presentation` に `variant_id` と `quantization` を足し、コンソールはモデルごとに variant の配列を受ける。選んだ variant は `desired_variant_id` で届く。切替経路と「古い重みを消す」提案は variant 単位にする（同じモデルの別 variant に切り替えたとき、前の blob を消す提案が出る）。`lighter_picker` の「軽い候補は別のモデル」は軽いモデルの提案の規則として残る。
5. **tray は既定の variant だけを出す。** 「いま動いている」行は serve している variant（`inference_state.variant_id`）。
6. **既定の variant はモデルごとに手書き（`default_variant`、エンジンごと）。** 原則は公式の Q4_K_M（vLLM は FP8 / BF16）。公式 4bit の無いモデルは低い量子化を既定にする。`FamilyBestFit` とコントロールプレーンの `betterFit` の代表選びは `default_variant` に置き換える（`preferRecommended` は推奨の判定として残す）。
7. **推奨の範囲は `RankModels` の今の規則どおり。** 既定 variant に限定しない。軽い variant は重い方が載らない帯で推奨に残る。`quality_tier` 降順の並びも維持。
8. **1 台のホストの実測から導いた定数は機構に置き換える。** `OllamaSpillCalibration = 3.0`（#625 の単点較正）と `OllamaMaxExpectedSpillFraction` の導出（#664）は L113（#1337）で llama.cpp の fit の項（fit の余裕、再帰状態、MTP の第 2 context、host 側の重み）に置き換える。0.20 の意味の読み替えも L113 の裁量。設計の対象は 8 / 12 / 16 / 24 / 32 / 48 GB 以上の帯すべて。
10. **推奨条件は「コンテキストウィンドウ込みで完全常駐」。** 完全常駐とは、提供するコンテキストウィンドウ込みで GPU のメモリ（ユニファイドメモリ機では GPU が使えるプール `OllamaVRAMBudgetMB`）に収まり、CPU に落ちる層が 0 であること。discrete と unified の両方に掛かる。`OllamaRecommendModel` の第 3 条件を `OllamaPlannedRung(...).NoSpillCapacityTokens >= ServingWindow200k`（選んだ KV の型の係数で価格付け。係数は ggml のブロックサイズ由来で `q8_0` が f16 の 0.53125、`q4_0` が 0.28125。24 GB 級 GPU × 27B の実測 KV 6,664 / 3,528 MiB と一致する）に改める。必要量 = device 側の重み（`estimated_weight_gb` − `host_resident_weight_gb`）+ オーバーヘッド + KV。第 1・第 2 条件と GPU の無いホストの免除は据え置き。`ReasonWindowExceedsMemory` は「完全常駐しない」の意味で引き続きソフト（拒否は容量だけ）。**推奨だけに掛かる**: 手で選んだモデルの serve は今のまま、`OllamaPlannedRung` の規則 2（上限付きスピル）・3（単調性）で最上位の rung を選び、配置を報告する。vLLM は clamp する設計なので、予算内で 200k に届くことが既に完全常駐の意味で、変更なし。行には spill % でなく「GPU に載る層数 / 全層」を出し、ロード後の witness はエンジンのログ `offloaded N/M layers`（`/api/ps` は数えない）。
11. **`qwen3.8-flash-next` は 2bit を出荷し続け、サイズを直す（#1305）。4bit（`frob/qwen3.8-flash-next:125b-a6b-ud-q4_K_XL`、111.33 GB）を手で選べる variant として足してよい。** 3bit は入手できない（frob に無く、unsloth はシャード）。PLE のテーブルが GPU に載らない性質（`docs/knowledges/20260914/0120-input-layer-tensors-stay-on-the-cpu.md`）は `host_resident_weight_gb`（llama.cpp が入力層として CPU に置くテンソルの合計）で表し、推奨は device 側、容量は合計と「host 側 ≤ システム RAM」で判定する（#1337）。

## Consequences

- **proto（#1346、additive の小 PR 1 本）**: `Presentation` に `variant_id` / `quantization` / `kv_cache_type` / `gpu_layers` / `total_layers`、`Manifest.default_variant`、`Variant.kv_cache_types` / `host_resident_weight_gb`、desired に `desired_variant_id` / `desired_kv_cache_type`。欄名は #1346 の契約表のとおり。
- **hostfit / modelrank（#1347）**: `servingKVCacheDivisor` を型ごとの係数に（`OllamaKVFactorQ4_0` を additive で足す）、`OllamaRecommendModel` の第 3 条件、`window.go` の anchor 定数（L113 と同じ関数なので L113 の後）。
- **agent（#1348）**: `planOllamaKV` の既定と段、`inference_ollama_verify.go` の `"q8_0"` 直接比較（f16 への劣化検出が `q8_0` だけを見ている）、`lighter_picker` の注記、削除の variant 単位、tray。
- **カタログ（#1349）**: `default_variant`、`kv_cache_types`、flash-next のサイズ・Q4・`host_resident_weight_gb`、`source.tag` の `125b-a6b` と `display_name` の 180B-A6B の表記合わせ。
- 既存の `manual_only`（モデル単位）は変えない。`TestBundledManifests_QualityTierFollowsPrecisionWithinAModel` は据え置き。
- 決めないこと: `kv_cache_types` の Qwen 以外の値、27B の既定 variant（出荷中の MTP は一様 Q4_K、unsloth の UD-Q4_K_M は attention を守る。忠実度 13.3% 対 6.7%、decode は MTP が +62%）、MoE の量子化感度の測定（進行中）。

## Refs

- waired-ai/waired#1357（設計と実測。裁定コメント 5654063492、訂正 5654089278）/ waired-ai/waired#1361 の L107・L113・L118〜L121 / waired-ai/waired#1387 / waired-ai/waired#1388 / waired-ai/waired#1389
- #1346 / #1347 / #1348 / #1349（実装）/ #1337（L113）/ #1305 / #1265 / #29 / #676 / #625 / #664 / #1090
- private 側の対の記録: waired `docs/decisions/20260913/2350-l107-catalog-rulings-variant-kv-residency.md`
- 本記録が狭めた記録: `docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md` / `docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` / `docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md` / `docs/decisions/20260809/0110-serve-at-the-rung.md` / `docs/decisions/20260907/0030-lighter-builds-come-from-one-upstream.md`
- 知見: `docs/knowledges/20260914/0120-input-layer-tensors-stay-on-the-cpu.md`、`docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`、`docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md`、`docs/knowledges/20260907/0030-lighter-quants-only-exist-off-library.md`
