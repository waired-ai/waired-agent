---
status: accepted
---

# 小さい Mac には、実際に実行できるモデルを渡す (20260807 14:12)

## Status

Accepted。2026-08-03 のオーナー決定（waired-ai/waired#1056、`waired`
`docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`）の**ルールはそのまま維持
する** — 拒否は確実な OOM だけに限る。変えるのは、その「確実な OOM」を判定している
**算術**であって、ルールではない。

その結果として 8 GB の Apple Silicon Mac に対する結論が 1 つ変わる。waired#1056 の
症状表にあった「8 GB Mac がローカルモデルを 1 つも持てない」状態には戻らない —
そのホストは qwen3.5-2b でローカル推論を維持する。

## Context

macOS の `install+inference` レグが 3 連続で赤（run 31030433108 / 31094256263 /
31164150206）。`waired init` の末尾ベンチマークが `HTTP 500` を返していた。

完全な `engine.log`（アーティファクト全 711 行）から分かったこと:

- `llama-server` は死んでいない。63 秒でロードを完了し `model loaded` /
  `listening on 127.0.0.1:49249` / `all slots are idle` まで到達し、500 の後も
  `GET /api/tags` に 200 を返し続けている。500 を返したのは**最初の実生成**
  （`POST /v1/chat/completions`、slot がタスクを掴んだ 2 秒後）。
- ollama は `reason=metal_partial_offload` で partial offload に落ちていた
  （`MTL0 model buffer size = 0.72 MiB` に対し `CPU_REPACK 2581.03 MiB`）。
  `ggml_metal_buffer_get_id: error: tensor ' (view)' buffer is nil` はその経路の
  症状で、qwen3.5 のハイブリッド（Gated Delta Net）× Metal partial-offload の
  組み合わせで出る。
- ホストは RAM 7.0 GiB / 空き 3.8 GiB / swap 0 B / 3 スレッド。

### なぜ容量ゲートが通したか

ユニファイドメモリのホストでは、ウィンドウの見積もりが
`OllamaVRAMBudgetMB − OllamaVRAMOverheadUMAMB` を**使い切る**ウィンドウを選ぶ。
つまり選ばれたウィンドウは `重み + KV == その予算` を満たす。容量ゲートはその同じ
ウィンドウを `重み + オーバーヘッド + KV` で見積もるので、結果は
`OllamaVRAMBudgetMB` そのものに戻る。そしてそれを `TotalMemoryMB()` と比較する。

つまりユニファイドホストでの判定は、モデルによらず

```
floor(3R/4)·1024  ≤  (R−2)·1024
```

に還元される（`UsableVRAMMB` は macOS で `RAMTotalGB * 3 / 4 * 1024`、整数除算）。
この両辺は **5 ≤ R ≤ 8 でちょうど等しい**。その帯では容量ゲートは、見積もりが今
決めたものを拒否できない。qwen3.5-4b の R=7 / R=8 での余白は 5 MiB だった。

その 5 MiB の外側にあったもの:

- OS の実フットプリント（`OSMemoryAllowanceGB` の想定 2 GiB に対し実測 3.2 GiB）
- グラフの compute バッファ（テキスト 250 MiB）
- **vision tower** — qwen3.5-4b はマルチモーダル GGUF で、画像を送るかどうかに
  関係なく ollama が `--mmproj` 付きでロードする。ロード時の予約が
  `reserve_compute_meta: MTL0 231.44 MiB + CPU 173.31 MiB = 404.75 MiB`。

## Decision

1. **vision tower のロード時予約を容量計算に計上する。**
   `catalog.Variant.VisionWorkingSetGB`（追加のみ、`omitempty`）を新設し、
   `hostfit.OllamaResidentMB` / `OllamaWindowResidentMB` が加算する。
   qwen3.5-4b に `0.42` を注記する（上の実測値）。

   `proto-guard` は既存 const の値変更を禁じるので、`OSMemoryAllowanceGB` や
   `OllamaVRAMOverheadUMAMB` を動かす道は取れない。計上されていない項を計上する
   のが、この規約と整合する唯一の形でもある。

2. **課金するのはロード時の予約であって、投影器の上限ではない。**
   同じロードは `[mtmd] estimated worst-case memory usage of mmproj is
   1143.19 MiB` も報告する。これは最大サイズの画像を処理したときの天井で、
   テキストだけのセッションは一度も触らない。確実な OOM だけが拒否の理由である
   以上（waired#1056）、ハードゲートが課金してよいのは「ロードが確実に確保する分」
   に限る。

3. **`OllamaWeightsResidentMB` には加算しない。** そちらは推奨（recommendation）
   側の項で、「重みがカードに常駐しているか」というバイトの置き場所の話。今回の
   測定はユニファイドメモリで取ったもので、それをディスクリート GPU の推奨判断に
   波及させる根拠にはならない。

4. **見積もられたウィンドウは一切狭めない。** ゲートが決めるのは「どのモデル」で、
   見積もりが決めるのが「そのモデルのウィンドウ」。8 GB Mac では
   qwen3.5-4b の 119,808 に対し qwen3.5-2b はネイティブ全域の 262,144 を
   1.7 GB の余白付きで実行できる。小さいモデルに落とすとコンテキストウィンドウは
   **広がる**（4b のハイブリッド構成は KV が 32768 B/token、2b は 12288 B/token）。

5. **品質スコアの下限（`InstallQualityFloorTier` = 30、#517）は、それを下回る
   候補しか入らないユニファイドホストでは譲る。** そのホストが実行できる最良の
   候補を提示する。ただし `NativeContextFloorTokens`（waired#1031）は譲らない —
   32k ネイティブのモデルはどのハードウェアでもコーディングセッションに答えられ
   ない。

   これは新しい発見ではない。`internal/router/uma_tiers_estimated_test.go` の
   8 GB 行が #448 以来この欠陥の見張りとして置かれていた:「when #1056 lands, an
   8 GB Mac should be OFFERED this pick rather than dropped, and this expectation
   is where that shows up first」。

6. **どの候補も入らないホスト（4-6 GB）は従来どおり**「推奨要件未満」として
   ローカル推論を既定オフにし、#465 の opt-in を残す。

## この経路で「誰がモデルを選んだか」

測定で確かめたこと（`SelectBundledModel` をレグと同じ 7 GB Apple プロファイルで
呼び出した結果）: **エージェント側の install-time 選択は、この帯ではそもそも
qwen3.5-4b を選んでいない。**

| RAM | 変更前 | 変更後 |
|---|---|---|
| 6 GB | `""`（forced: 0.8b）/ off | 変わらず |
| 7 GB | `""`（forced: 2b）/ off | 変わらず |
| 8 GB | **qwen3.5-4b / on** | `""`（forced: 2b）/ off |
| 9 GB | **qwen3.5-4b / on** | `""`（forced: 2b）/ off |
| 12 GB | qwen3.5-4b / on | 変わらず |
| 16 GB | qwen3.5-9b / on | 変わらず |

レグのホストは 7 GB（`bytesToGBRounded` は GiB 丸めで、llama.cpp も
`Apple M1 (Virtual) (7168 MiB)` と報告している）。つまりエージェントは何も選んで
いない。実際に qwen3.5-4b を名指したのは**コントロールプレーン**で、
`internal/controlplane/api/management_device_model_catalog.go` の
`recommendedModel` が、エージェントが publish したハードウェア要約に対して同じ
`proto/hostfit` を呼び、同じ escape ladder を降りて選んでいる。

したがってこの決定が macOS レグに効くのは、**`waired` が新しい proto タグに
上がってから**である。それまではコントロールプレーンが古い算術で 4b を名指し
続ける（`waired` の `go.mod` は現在 `waired-agent/proto v0.2.24`）。

## Consequences

- 5-8 GB のユニファイドホストが qwen3.5-4b を拒否されるようになる。実測があるのは
  7 GiB のホストだけだが、8 GB を許可する算術的な根拠は無い — 見積もりが余った
  メモリを全て KV に変換するので、RAM が増えても余白には変わらない。
- 9 GB 以上のホスト、CPU 専用ホスト、ディスクリート GPU ホストの判定は変わらない
  （`TestVisionTower_ChangesNothingItWasNotAimedAt` が全 bundled variant ×
  RAM スイープで pin）。
- `TestBundledCatalog_TheSymptomHostsKeepLocalInference` の 8 GB Mac 行を改訂した。
  その行が約束していること（そのホストはローカル推論を保つ）は変わっていない。
  保つ相手のモデルが変わった。
- 5 ≤ R ≤ 8 の帯そのものは**残る**。テキスト専用の variant には計上する項が無く、
  証拠のない拒否は waired#1056 に反する。qwen3.5-2b は 5 GB Mac を 1 MiB の余白で
  通る。Apple は 5 GB 機を出していないので実害は無いが、帯は
  `TestVisionTower_TheBandWhereTheTwoBudgetsCoincide` で可視のまま置いた。
- 同じ欠陥の別入口が 1 つ残っている: qwen2.5-coder-7b-instruct（4.7 GB、
  テキスト専用）は 8 GB のユニファイドホストを 189 MiB の余白で通る。
  `NativeContextFloorTokens` が自動選択からは外しているが、`waired models ls` と
  コントロールプレーンのウィザードは今も提示する。#552 に記録済み。

## Refs

- waired-ai/waired-agent#552（この決定の起点）、#549（これを露出させた PR）
- waired-ai/waired#1056 / `waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`
- #517（`InstallQualityFloorTier`）、#448（8 GB 行の見張り）、#424（UMA
  オーバーヘッド 1024 MiB）、#465（opt-in）、#624 / waired#1031（ネイティブ
  ウィンドウの下限）
- #496 / #550（測定によるホストカットオフ。`waired init` が desired-inference を
  書いた時点で走らなくなるため、この経路は守っていない）
